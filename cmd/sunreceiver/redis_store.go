// sunReceiver
// Copyright (C) 2026  Aleksandr Galinskii
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// redisStore — тонкая обёртка над клиентом Redis для текущих значений и временного
// ряда. Общие ключи sunreceiver:current / :series:* выведены из эксплуатации —
// инверторы обособлены по марке (см. inv_store.go), прочее — по модулям.
type redisStore struct {
	rdb *redis.Client
	ctx context.Context

	// mapWin помнит последний member ряда каждого МАП-устройства: нужен, чтобы
	// дополнить недостающие МАП-теги из предыдущего снимка (mergeMAPSnap) — гейт
	// МАП нестабильно отдаёт блоки. Запись каждого показания не подавляется.
	mapMu  sync.Mutex
	mapWin map[string]mapWinMember

	// tsMu/tsCreated/tsFailUntil — кэш уже созданных серий RedisTimeSeries
	// (key -> подпись меток) и негативный бэкофф при сбое TS.CREATE. См. rts_store.go.
	tsMu        sync.Mutex
	tsCreated   map[string]string
	tsFailUntil map[string]time.Time
}

type mapWinMember struct {
	member string // JSON-снапшот последнего снимка МАП — источник mergeMAPSnap
}

// openRedis создаёт клиент Redis. Retry-логику оставляем библиотеке go-redis.
// Read/Write-таймауты ограничивают зависшую операцию (повисший Redis без
// OOM-свопа/сбоев TCP не должен замораживать всех пулеров и дашборд);
// go-redis сам переподнимает зависшее соединение.
func openRedis(addr string) (*redis.Client, error) {
	rdb := redis.NewClient(&redis.Options{
		Addr:         addr,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 2 * time.Second,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		rdb.Close()
		return nil, fmt.Errorf("redis ping %s: %w", addr, err)
	}
	return rdb, nil
}

// SetCtx привязывает контекст, на котором выполняются все операции Redis, к
// единому сигналу остановки (stopCtx из main.go). До вызова используется
// context.Background() (см. литерал redisStore в main.go). При отмене контекста
// зависшие долгие операции (scan/запись бакетов) прерываются, а не висят до
// Read/Write-таймаута. Вызывается один раз до запуска горутин пулеров.
func (s *redisStore) SetCtx(ctx context.Context) {
	s.ctx = ctx
}

// SaveSnapshot пишет снимок сетевого инвертора в обособленные ключи его марки
// (deye/sofar) — по серийному номеру инвертора (InverterSN) как ключу. Общие
// sunreceiver:current / sunreceiver:series:* выведены из эксплуатации.
func (s *redisStore) SaveSnapshot(snap deviceSnapshot, ts time.Time) error {
	b := invBrandByKind(snap.Kind)
	if b == nil {
		return fmt.Errorf("save snapshot: неизвестная марка инвертора %q (ip=%s)", snap.Kind, snap.IP)
	}
	return s.saveInvSnapshot(*b, snap, ts)
}

// mergeMAPSnap дополняет снимок устройства МАП недостающими МАП-тегами
// (grid_voltage, grid_power, battery_voltage, battery_power) значениями из
// предыдущего снимка prevMember этого же устройства. Нужно из-за того, что гейт
// МАП нестабильно отдаёт блоки ячеек: тег может отсутствовать в части кадров.
// Возвращает снимок с полным набором последних известных значений МАП.
func mergeMAPSnap(snap deviceSnapshot, prevMember string) deviceSnapshot {
	if prevMember == "" {
		return snap
	}
	var prev deviceSnapshot
	if err := json.Unmarshal([]byte(prevMember), &prev); err != nil {
		return snap
	}
	out := valuesContract{}
	for k, v := range snap.Values {
		out[k] = v
	}
	for _, tag := range []string{"grid_voltage", "grid_power", "battery_voltage", "battery_current", "battery_power"} {
		if _, ok := out[tag]; !ok {
			if pv, ok2 := prev.Values[tag]; ok2 {
				out[tag] = pv
			}
		}
	}
	snap.Values = out
	return snap
}

// PruneMPPT удаляет из HASH current все MPPT-ключи (начинающиеся с "mppt-"),
// которых нет в active (множество актуальных devKey каждого MPPT, присутствующем в
// ответе ПАК «Малина»/«Малина26»). Нужно, чтобы исчезнувшие с МАП контроллеры
// переставали считаться «актуальными» на дашборде, но их история во временном ряду
// сохранялась.
func (s *redisStore) PruneMPPT(active map[string]struct{}) {
	m, err := s.rdb.HGetAll(s.ctx, redisKesCurrentKey).Result()
	if err != nil {
		log.Printf("redis prune mppt HGETALL: %v", err)
		return
	}
	var keys []string
	for ip := range m {
		if _, ok := active[ip]; ok {
			continue
		}
		keys = append(keys, ip)
	}
	if len(keys) == 0 {
		return
	}
	if err := s.rdb.HDel(s.ctx, redisKesCurrentKey, keys...).Err(); err != nil {
		log.Printf("redis prune mppt del %v: %v", keys, err)
	}
}

// SetBMS обновляет коллекцию BMS-устройств в HASH sunreceiver:bms одной
// транзакцией: пишет все активные (поле = ключ bmsKey, значение = JSON bmsDevice)
// и удаляет те, которых нет в active (устройство исчезло из коллекции —
// адаптер отключился/замолчал, bmslistener уже забыл его).
func (s *redisStore) SetBMS(active map[string]string) error {
	// КОСТЫЛЬ: старый ANT-пулер пишет не в sunreceiver:bms, а в ряды bms26
	// (sunreceiver:bms26:current) с новым serial (ANT — по ёмкости). См. bms26_legacy.go.
	cur := make(map[string]string, len(active))
	for _, raw := range active {
		var d bmsDevice
		if json.Unmarshal([]byte(raw), &d) != nil {
			continue
		}
		cap, ok := antCapFromName(d.DeviceName)
		if !ok {
			continue
		}
		serial := legacyAntSerial(cap)
		if serial == "" {
			continue
		}
		b, err := json.Marshal(antDeviceToBms26(d, serial))
		if err != nil {
			continue
		}
		cur[serial] = string(b)
	}
	if len(cur) == 0 {
		return nil
	}
	return s.SaveBms26Current(cur)
}

// eachMonth вызывает fn для каждого календарного месяца в диапазоне [start, end]
// (включительно), начиная с месяца start. fn возвращает false — прервать обход.

// scanPrefixKeys возвращает все ключи с данным префиксом через SCAN — не
// блокирует Redis в отличие от KEYS.
func (s *redisStore) scanPrefixKeys(prefix string) ([]string, error) {
	var (
		keys   []string
		cursor uint64
	)
	for {
		batch, next, err := s.rdb.Scan(s.ctx, cursor, prefix, 200).Result()
		if err != nil {
			return nil, fmt.Errorf("scan keys %s: %w", prefix, err)
		}
		keys = append(keys, batch...)
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return keys, nil
}

// IsEmpty возвращает true, если в Redis нет ни текущего состояния, ни одного
// сегмента временного ряда (т.е. in-memory данные потеряны и нужна реставрация
// из persistent-хранилища PostgreSQL).
func (s *redisStore) IsEmpty() (bool, error) {
	// Достаточно любого current-HASH модуля (последнее состояние) — уже «не пусто».
	for _, key := range []string{
		deyeBrand.curKey, sofarBrand.curKey,
		redisDds238CurrentKey, redisMapCurrentKey, redisKesCurrentKey,
		redisBms26CurrentKey, redisDts017CurrentKey, redisCE308CurrentKey,
	} {
		n, err := s.rdb.HLen(s.ctx, key).Result()
		if err != nil {
			return false, err
		}
		if n > 0 {
			return false, nil
		}
	}
	// Либо есть хоть одна серия RedisTimeSeries.
	keys, err := s.scanPrefixKeys(tsSeriesPrefix + "*")
	if err != nil {
		return false, err
	}
	return len(keys) == 0, nil
}

// recentCutoff возвращает момент начала вторых из последних двух календарных
// суток (в локальной зоне). Redis хранит данные за последние 2 календарных
// суток: «сегодня» и «вчера», т.е. начиная с 00:00 вчерашнего дня. Данные
// строго старше cutofа удаляются фоновой очисткой и не читаются из Redis.
func recentCutoff(t time.Time) time.Time {
	y, m, d := t.In(time.Local).Date()
	startOfToday := time.Date(y, m, d, 0, 0, 0, 0, time.Local)
	return startOfToday.AddDate(0, 0, -1)
}

// seriesRawWindow — сколько «свежего» времени отдаём из Redis СЫРЫМИ (1 с), а всё
// более старое в окне запроса — из PG (5-мин средние). Ряды лежат общими месячными
// ZSET (сотни тысяч членов на все устройства), поэтому чтение широкого окна из Redis
// — это полный скан с JSON-разбором каждого члена (десятки секунд). Ограничиваем
// Redis-хвост небольшим окном: детализация 1 с нужна только для «зума», для обзорных
// графиков достаточно 5-мин средних из PG.
const seriesRawWindow = 1 * time.Hour

// seriesReadCutoff — граница Redis/PG для чтения ряда по запросу [start, now]:
// Redis покрывает только последние seriesRawWindow, остальное — PG.
func seriesReadCutoff(start, now time.Time) time.Time {
	c := now.Add(-seriesRawWindow)
	if c.Before(start) {
		c = start
	}
	return c
}

// PurgeOld удаляет из временных рядов Redis (месячные сегменты: инверторы deye/sofar,
// МАП, DDS238, КЭС, DTS017M, BMS26, мгновенные CE308) точки старше окна последних
// 2 календарных суток (recentCutoff). Пустые сегменты удаляются целиком. Текущие
// HASH (current-ключи модулей) не трогаются — последнее состояние хранится всегда.
// Вызывается фоновым процессом (см. runRedisCleanup).
// PurgeOld — no-op: ротацию временных рядов выполняет сам RedisTimeSeries
// (RETENTION 2 суток на серию). Оставлен для совместимости фонового цикла
// (runRedisCleanup) до удаления старых ZSET-рядов (этап 9 плана).
func (s *redisStore) PurgeOld(now time.Time) {}

// CurrentOne возвращает один последний снимок по ЛОГИЧЕСКОМУ ключу обособленного
// устройства: meterDeviceKey → Dds238Current, mapDevKey → MapCurrent,
// `mppt-<serial>` → соответствующий КЭС из KesCurrent. Инверторы читаются
// марочными читалками (invCurrent); общий HASH current выведен из эксплуатации.
// Используется точечными потребителями (AGM-плашки, лампы-индикаторы,
// уведомления). Если снимка нет — redis.Nil.
func (s *redisStore) CurrentOne(ip string) (deviceSnapshot, error) {
	switch {
	case ip == meterDeviceKey:
		d, err := s.Dds238Current()
		if err != nil {
			return deviceSnapshot{}, err
		}
		if d == nil {
			return deviceSnapshot{}, redis.Nil
		}
		return *d, nil
	case ip == mapDevKey:
		m, err := s.MapCurrent()
		if err != nil {
			return deviceSnapshot{}, err
		}
		if m == nil {
			return deviceSnapshot{}, redis.Nil
		}
		return *m, nil
	case isKesKey(ip):
		ks, err := s.KesCurrent()
		if err != nil {
			return deviceSnapshot{}, err
		}
		for _, k := range ks {
			if k.IP == ip {
				return k, nil
			}
		}
		return deviceSnapshot{}, redis.Nil
	}
	return deviceSnapshot{}, fmt.Errorf("current %q: неизвестный ключ устройства", ip)
}

// queryAllSeries возвращает объединённый мгновенный ряд за [start, end]:
// сетевые инверторы (deye + sofar) + обособленные DDS238/МАП/КЭС (их собственные
// ряды). Для накопителя/восстановления, которым нужен полный набор устройств:
// общий ряд sunreceiver:series:* выведен из эксплуатации.
func (s *redisStore) queryAllSeries(start, end time.Time) ([]deviceSnapshot, error) {
	var all []deviceSnapshot
	for _, b := range []invBrand{deyeBrand, sofarBrand} {
		pts, err := s.queryInvSeries(b, start, end)
		if err != nil {
			return nil, err
		}
		all = append(all, pts...)
	}
	if d, derr := s.QueryDds238Series(start, end); derr != nil {
		log.Printf("queryAllSeries dds238: %v", derr)
	} else {
		all = append(all, d...)
	}
	if m, merr := s.QueryMapSeries(start, end); merr != nil {
		log.Printf("queryAllSeries map: %v", merr)
	} else {
		all = append(all, m...)
	}
	if ks, kerr := s.QueryKesSeries(start, end); kerr != nil {
		log.Printf("queryAllSeries kes: %v", kerr)
	} else {
		all = append(all, ks...)
	}
	return all, nil
}
