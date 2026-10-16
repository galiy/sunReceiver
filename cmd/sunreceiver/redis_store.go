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
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// Ключи Redis.
const (
	// redisCurrentKey — HASH текущих (последних) значений: поле=ip инвертора,
	// значение=JSON deviceSnapshot. Один HGETALL отдаёт состояние всех инверторов.
	redisCurrentKey = "sunreceiver:current"
	// redisSeriesPrefix — временной ряд. Ключи вида sunreceiver:series:<YYYY-MM>,
	// каждый — ZSET: score=Unix (сек.), member=JSON deviceSnapshot.
	// Месячная сегментация: чтение произвольного периода — это несколько ZRANGEBYSCORE
	// по затронутым месяцам, отсортированных по времени.
	redisSeriesPrefix = "sunreceiver:series:"
)

// redisStore — тонкая обёртка над клиентом Redis для текущих значений и временного ряда.
type redisStore struct {
	rdb *redis.Client
	ctx context.Context

	// mapWin помнит последний member ряда каждого МАП-устройства: нужен, чтобы
	// дополнить недостающие МАП-теги из предыдущего снимка (mergeMAPSnap) — гейт
	// МАП нестабильно отдаёт блоки. Запись каждого показания не подавляется.
	mapMu  sync.Mutex
	mapWin map[string]mapWinMember

	// legacyMPPTCleaned — одноразовая зачистка устаревших MPPT-ключей старого
	// формата; выполняется при первом вызове PruneLegacyMPPT (после реставрации
	// Redis из PG), далее — no-op.
	legacyMPPTCleaned atomic.Bool
}

type mapWinMember struct {
	member string // JSON-снапшот (member ZSET) — источник mergeMAPSnap
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

// redisSeriesKey возвращает ключ месячного сегмента временного ряда для ts.
func redisSeriesKey(ts time.Time) string {
	return redisSeriesPrefix + ts.Format("2006-01")
}

// SaveSnapshot пишет снимок в Redis одной транзакцией:
//   - обновляет текущее значение (HASH current[ip]);
//   - кладёт точку в месячный ZSET временного ряда (score = Unix-секунды).
//
// Хранится КАЖДОЕ снятое показание (окно удержания — 2 календарных суток).
// Тот же (устройство, секунда) может записываться дважды (повторный опрос в
// пределах секунды): голый ZADD оставил бы два member с одинаковым score,
// поэтому перед записью удаляются старые версии того же устройства на этом
// score — остаётся ровно одна точка на (устройство, секунда).
func (s *redisStore) SaveSnapshot(snap deviceSnapshot, ts time.Time) error {
	return s.saveSnapshot(snap, ts, false)
}

// SaveSnapshotMAP — как SaveSnapshot, но для целей МАП/MPPT: недостающие МАП-теги
// (grid_voltage/grid_power/battery_voltage/battery_power) дополняются из
// предыдущего снимка того же устройства (гейт МАП нестабильно отдаёт блоки).
// Пишется каждое снятое показание (без 10-секундного окна).
func (s *redisStore) SaveSnapshotMAP(snap deviceSnapshot, ts time.Time) error {
	return s.saveSnapshot(snap, ts, true)
}

// saveSnapshot — общая реализация записи. mergeMAP=true — дополнить МАП-теги из
// предыдущего снимка и запомнить member как источник для следующего merge.
func (s *redisStore) saveSnapshot(snap deviceSnapshot, ts time.Time, mergeMAP bool) error {
	if mergeMAP && isMAPDevice(snap.Values) {
		prevMember := ""
		s.mapMu.Lock()
		if p, had := s.mapWin[snap.IP]; had {
			prevMember = p.member
		}
		s.mapMu.Unlock()
		snap = mergeMAPSnap(snap, prevMember)
	}
	b, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("marshal snapshot %s: %w", snap.IP, err)
	}
	key := redisSeriesKey(ts)
	// Старые версии этого же устройства на том же моменте (score): ищем среди
	// member'ов с точным score, разбираем и собираем для удаления.
	score := strconv.FormatInt(ts.Unix(), 10)
	old, err := s.rdb.ZRangeByScore(s.ctx, key, &redis.ZRangeBy{Min: score, Max: score}).Result()
	if err != nil {
		return fmt.Errorf("dedup %s: %w", snap.IP, err)
	}
	var stale []any
	for _, m := range old {
		var q deviceSnapshot
		if json.Unmarshal([]byte(m), &q) != nil {
			continue
		}
		if q.IP == snap.IP {
			stale = append(stale, m)
		}
	}
	pipe := s.rdb.TxPipeline()
	pipe.HSet(s.ctx, redisCurrentKey, snap.IP, b)
	if len(stale) > 0 {
		pipe.ZRem(s.ctx, key, stale...)
	}
	pipe.ZAdd(s.ctx, key, redis.Z{Score: float64(ts.Unix()), Member: string(b)})
	// Держим хоть один месяц истории; при смене месяца эта строка оставит ключ живым.
	pipe.Expire(s.ctx, key, 40*24*time.Hour)
	if _, err := pipe.Exec(s.ctx); err != nil {
		return fmt.Errorf("save %s: %w", snap.IP, err)
	}
	if mergeMAP {
		s.mapMu.Lock()
		if s.mapWin == nil {
			s.mapWin = map[string]mapWinMember{}
		}
		s.mapWin[snap.IP] = mapWinMember{member: string(b)}
		s.mapMu.Unlock()
	}
	return nil
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

// PruneLegacyMPPT удаляет из HASH current устаревшие MPPT-ключи старого формата
// (содержащие "#mppt", вид "<host>#mppt-<uid>"). Новый формат ключа — "mppt-<serial>"
// (без host), такие ключи в active-множестве PruneMPPT и здесь не трогаются.
// Выполняется ОДИН РАЗ за жизнь процесса (при первом вызове, после реставрации
// Redis из PG) — вызывается из горячего секундного цикла, поэтому повторные
// HGetAll-вычитки исключены флагом.
func (s *redisStore) PruneLegacyMPPT() {
	if !s.legacyMPPTCleaned.CompareAndSwap(false, true) {
		return
	}
	m, err := s.rdb.HGetAll(s.ctx, redisCurrentKey).Result()
	if err != nil {
		log.Printf("redis prune legacy mppt HGETALL: %v", err)
		return
	}
	var keys []string
	for ip := range m {
		if strings.Contains(ip, "#mppt") {
			keys = append(keys, ip)
		}
	}
	if len(keys) == 0 {
		return
	}
	if err := s.rdb.HDel(s.ctx, redisCurrentKey, keys...).Err(); err != nil {
		log.Printf("redis prune legacy mppt HDEL: %v", err)
		return
	}
	log.Printf("redis: удалены устаревшие MPPT-ключи (%d)", len(keys))
}

// PruneMPPT удаляет из HASH current все MPPT-ключи (начинающиеся с "mppt-"),
// которых нет в active (множество актуальных devKey каждого MPPT, присутствующем в
// ответе ПАК «Малина»/«Малина26»). Нужно, чтобы исчезнувшие с МАП контроллеры
// переставали считаться «актуальными» на дашборде, но их история во временном ряду
// сохранялась.
func (s *redisStore) PruneMPPT(active map[string]struct{}) {
	m, err := s.rdb.HGetAll(s.ctx, redisCurrentKey).Result()
	if err != nil {
		log.Printf("redis prune mppt HGETALL: %v", err)
		return
	}
	var keys []string
	for ip := range m {
		if !strings.HasPrefix(ip, "mppt-") {
			continue
		}
		if _, ok := active[ip]; ok {
			continue
		}
		keys = append(keys, ip)
	}
	if len(keys) == 0 {
		return
	}
	if err := s.rdb.HDel(s.ctx, redisCurrentKey, keys...).Err(); err != nil {
		log.Printf("redis prune mppt del %v: %v", keys, err)
	}
	// Чистим в памяти mapWin по удалённым MPPT-ключам (иначе карта растёт при
	// ротации контроллеров).
	s.mapMu.Lock()
	for _, k := range keys {
		delete(s.mapWin, k)
	}
	s.mapMu.Unlock()
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

// BMSCurrent возвращает текущее состояние всех BMS-устройств (поле = ключ
// bmsKey, значение = JSON bmsDevice).
func eachMonth(start, end time.Time, fn func(y int, m time.Month) bool) {
	y, m := start.Year(), start.Month()
	for {
		t := time.Date(y, m, 1, 0, 0, 0, 0, time.Local)
		if t.After(end) {
			break
		}
		if !fn(y, m) {
			break
		}
		if m == 12 {
			y++
			m = 1
		} else {
			m++
		}
	}
}

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

// scanSeriesKeys возвращает все месячные ключи временного ряда инверторов
// (prefix*) — для проверки пустоты (IsEmpty).
func (s *redisStore) scanSeriesKeys() ([]string, error) {
	return s.scanPrefixKeys(redisSeriesPrefix + "*")
}

// IsEmpty возвращает true, если в Redis нет ни текущего состояния, ни одного
// сегмента временного ряда (т.е. in-memory данные потеряны и нужна реставрация
// из persistent-хранилища PostgreSQL).
func (s *redisStore) IsEmpty() (bool, error) {
	n, err := s.rdb.HLen(s.ctx, redisCurrentKey).Result()
	if err != nil {
		return false, err
	}
	if n > 0 {
		return false, nil
	}
	keys, err := s.scanSeriesKeys()
	if err != nil {
		return false, err
	}
	if len(keys) > 0 {
		return false, nil
	}
	// Обособленные данные DDS238 (current/ряд) тоже означают «не пусто».
	if n, err := s.rdb.HLen(s.ctx, redisDds238CurrentKey).Result(); err != nil {
		return false, err
	} else if n > 0 {
		return false, nil
	}
	if ks, err := s.scanPrefixKeys(redisDds238SeriesPrefix + "*"); err != nil {
		return false, err
	} else if len(ks) > 0 {
		return false, nil
	}
	// Обособленные данные МАП (current/ряд).
	if n, err := s.rdb.HLen(s.ctx, redisMapCurrentKey).Result(); err != nil {
		return false, err
	} else if n > 0 {
		return false, nil
	}
	if ks, err := s.scanPrefixKeys(redisMapSeriesPrefix + "*"); err != nil {
		return false, err
	} else if len(ks) > 0 {
		return false, nil
	}
	return true, nil
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

// PurgeOld удаляет из временных рядов Redis (месячные сегменты инверторов и
// сырых показаний ANT BMS/EnBMS, мгновенных значений CE308) все точки, timestamp
// которых строго старше окна последних 2 календарных суток (recentCutoff).
// Пустые сегменты удаляются целиком. Текущие HASH (current, sunreceiver:bms,
// sunreceiver:enbms:current) не трогаются — последнее состояние устройств
// хранится всегда.
// Вызывается фоновым процессом (см. runRedisCleanup).
func (s *redisStore) PurgeOld(now time.Time) {
	cutoff := recentCutoff(now)
	keys, err := s.scanSeriesKeys()
	if err != nil {
		log.Printf("redis cleanup keys: %v", err)
		return
	}
	// Ряд BMS (bms26) чистится тем же окном.
	bmsKeys, err := s.scanPrefixKeys(redisBms26SeriesPrefix + "*")
	if err != nil {
		log.Printf("redis cleanup bms26 keys: %v", err)
	} else {
		keys = append(keys, bmsKeys...)
	}
	// Ряд DDS238 (обособленный) — то же окно удержания.
	ddsKeys, err := s.scanPrefixKeys(redisDds238SeriesPrefix + "*")
	if err != nil {
		log.Printf("redis cleanup dds238 keys: %v", err)
	} else {
		keys = append(keys, ddsKeys...)
	}
	// Ряд МАП (обособленный) — то же окно удержания.
	mapKeys, err := s.scanPrefixKeys(redisMapSeriesPrefix + "*")
	if err != nil {
		log.Printf("redis cleanup map keys: %v", err)
	} else {
		keys = append(keys, mapKeys...)
	}
	// Ряд мгновенных значений CE308 (окно удержания — те же 2 календарных суток).
	ce308Keys, err := s.scanPrefixKeys(redisCE308SeriesPrefix + "*")
	if err != nil {
		log.Printf("redis cleanup ce308 keys: %v", err)
	} else {
		keys = append(keys, ce308Keys...)
	}
	// Обособленный ряд счётчика DTS017M (окно 2 календарных суток).
	dts017Keys, err := s.scanPrefixKeys(redisDts017SeriesPrefix + "*")
	if err != nil {
		log.Printf("redis cleanup dts017m keys: %v", err)
	} else {
		keys = append(keys, dts017Keys...)
	}
	// Операцию выполняем так, чтобы «строго старше cutoff», т.е. ZRemRangeByScore
	// убирает [ -inf ; cutoff-1 ], поэтому ровно cutoff остаётся в ряде.
	remBelow := strconv.FormatInt(cutoff.Unix()-1, 10)
	for _, key := range keys {
		if err := s.rdb.ZRemRangeByScore(s.ctx, key, "-inf", remBelow).Err(); err != nil {
			log.Printf("redis cleanup %s: %v", key, err)
			continue
		}
		n, err := s.rdb.ZCard(s.ctx, key).Result()
		if err != nil {
			continue
		}
		if n == 0 {
			if err := s.rdb.Del(s.ctx, key).Err(); err != nil {
				log.Printf("redis cleanup del %s: %v", key, err)
			}
		}
	}
}

// Current возвращает текущие снимки всех инверторов (поля HASH current) из Redis,
// отсортированные по имени для стабильного порядка на дашборде.
func (s *redisStore) Current() ([]deviceSnapshot, error) {
	m, err := s.rdb.HGetAll(s.ctx, redisCurrentKey).Result()
	if err != nil {
		return nil, fmt.Errorf("HGETALL %s: %w", redisCurrentKey, err)
	}
	snaps := make([]deviceSnapshot, 0, len(m))
	for _, v := range m {
		var snap deviceSnapshot
		if e := json.Unmarshal([]byte(v), &snap); e != nil {
			continue
		}
		if snap.IP == meterDeviceKey {
			continue // старое поле DDS238 в общем current — игнорируем (см. dds238:current)
		}
		if snap.IP == mapDevKey {
			continue // старое поле МАП в общем current — игнорируем (см. map:current)
		}
		snaps = append(snaps, snap)
	}
	// DDS238 хранится обособленно — подмешиваем его снимок прозрачно.
	if d, derr := s.Dds238Current(); derr == nil && d != nil {
		snaps = append(snaps, *d)
	}
	// МАП хранится обособленно — подмешиваем прозрачно.
	if m, merr := s.MapCurrent(); merr == nil && m != nil {
		snaps = append(snaps, *m)
	}
	// Сортируем по логическому имени — стабильный порядок карточек.
	for i := 1; i < len(snaps); i++ {
		for j := i; j > 0 && snaps[j].Name < snaps[j-1].Name; j-- {
			snaps[j], snaps[j-1] = snaps[j-1], snaps[j]
		}
	}
	return snaps, nil
}

// CurrentOne возвращает один последний снимок из current-HASH по IP
// (для донаса аппаратных серийных номеров в runInverterPoll).
func (s *redisStore) CurrentOne(ip string) (deviceSnapshot, error) {
	v, err := s.rdb.HGet(s.ctx, redisCurrentKey, ip).Result()
	if err != nil {
		return deviceSnapshot{}, err
	}
	var snap deviceSnapshot
	if e := json.Unmarshal([]byte(v), &snap); e != nil {
		return deviceSnapshot{}, e
	}
	return snap, nil
}

// QuerySeries возвращает все снимки за период [start, end] включительно из временного ряда.
// Читает по одному месячному сегменту ZRANGEBYSCORE, объединяя в порядке времени.
func (s *redisStore) QuerySeries(start, end time.Time) ([]deviceSnapshot, error) {
	if start.After(end) {
		return nil, errors.New("start after end")
	}
	min := strconv.FormatInt(start.Unix(), 10)
	max := strconv.FormatInt(end.Unix(), 10)
	var all []deviceSnapshot
	var queryErr error
	eachMonth(start, end, func(y int, m time.Month) bool {
		key := redisSeriesKey(time.Date(y, m, 1, 0, 0, 0, 0, time.Local))
		vals, err := s.rdb.ZRangeByScore(s.ctx, key, &redis.ZRangeBy{
			Min: min,
			Max: max,
		}).Result()
		if err != nil {
			queryErr = err
			return false
		}
		for _, v := range vals {
			var snap deviceSnapshot
			if e := json.Unmarshal([]byte(v), &snap); e != nil {
				continue
			}
			if snap.IP == meterDeviceKey {
				continue // старые точки DDS238 в общем ряду — игнорируем (см. dds238:series)
			}
			if snap.IP == mapDevKey {
				continue // старые точки МАП в общем ряду — игнорируем (см. map:series)
			}
			all = append(all, snap)
		}
		return true
	})
	if queryErr != nil {
		return nil, queryErr
	}
	// DDS238 хранится обособленным рядом — подмешиваем прозрачно.
	if d, derr := s.QueryDds238Series(start, end); derr == nil {
		all = append(all, d...)
	}
	// МАП хранится обособленным рядом — подмешиваем прозрачно.
	if m, merr := s.QueryMapSeries(start, end); merr == nil {
		all = append(all, m...)
	}
	return all, nil
}
