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
	"encoding/json"
	"log"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// meterMigrateMarkerKey — маркер однократной миграции идентификатора счётчика на
// единый ключ meterDeviceKey. Пока маркер есть — миграция пропускается.
const meterMigrateMarkerKey = "sunreceiver:migrated:meter-key"

// migrateMeterDeviceKey — однократная миграция: переводит идентификатор счётчика
// DDS238 на единый стабильный ключ meterDeviceKey (не зависящий от IP/транспорта).
// Раньше identity счётчика было его IP, из-за чего при смене адреса (напр. .77 →
// прозрачный шлюз .75) распадались current-поле, история Redis, агрегаты PG и
// добор тарифных границ. Миграция:
//   - в HASH current выбирает свежайший снимок счётчика и переносит его под новый
//     ключ, старые IP-поля удаляет;
//   - во временном ряду Redis (окно удержания) переписывает ip снимков счётчика
//     на новый ключ, сохраняя score;
//   - в PG averages переводит исторические строки счётчика (values ? meter_voltage)
//     на новый ключ.
//
// Идемпотентна: повторный запуск без маркера ничего не находит и завершается.
// Выполняется синхронно при старте ДО запуска пулеров и аккумулятора.
func migrateMeterDeviceKey(store *redisStore, pg *pgStore, now time.Time) {
	if store == nil {
		return
	}
	if ok, err := store.rdb.Exists(store.ctx, meterMigrateMarkerKey).Result(); err != nil {
		log.Printf("meter migrate: проверка маркера: %v", err)
		return
	} else if ok > 0 {
		return
	}
	log.Printf("meter migrate: единый ключ счётчика %q", meterDeviceKey)
	// Собираем все встреченные IP счётчика (в current и в ряду) — по ним
	// адресно переносим строки PG averages (по индексу (ip,ts), без полного скана
	// jsonb с риском упереться в statement_timeout).
	ips := map[string]struct{}{}
	migrateMeterCurrent(store, ips)
	migrateMeterSeries(store, now, ips)
	if pg != nil {
		migrateMeterPGAverages(pg, ips)
	}
	if err := store.rdb.Set(store.ctx, meterMigrateMarkerKey, "1", 0).Err(); err != nil {
		log.Printf("meter migrate: запись маркера: %v", err)
	}
}

// migrateMeterCurrent переносит свежайший снимок счётчика в current-HASH под
// единым ключом и удаляет старые IP-поля. Найденные старые IP добавляет в ips.
func migrateMeterCurrent(store *redisStore, ips map[string]struct{}) {
	cur, err := store.rdb.HGetAll(store.ctx, redisCurrentKey).Result()
	if err != nil {
		log.Printf("meter migrate current: %v", err)
		return
	}
	var oldKeys []string
	var newest deviceSnapshot
	found := false
	for field, raw := range cur {
		var snap deviceSnapshot
		if json.Unmarshal([]byte(raw), &snap) != nil || !isMeterDevice(snap.Values) {
			continue
		}
		if field != meterDeviceKey {
			oldKeys = append(oldKeys, field)
			ips[field] = struct{}{}
		}
		if !found || snap.Timestamp > newest.Timestamp {
			newest, found = snap, true
		}
	}
	if !found || len(oldKeys) == 0 {
		return
	}
	newest.IP = meterDeviceKey
	b, err := json.Marshal(newest)
	if err != nil {
		log.Printf("meter migrate current marshal: %v", err)
		return
	}
	pipe := store.rdb.TxPipeline()
	pipe.HSet(store.ctx, redisCurrentKey, meterDeviceKey, string(b))
	pipe.HDel(store.ctx, redisCurrentKey, oldKeys...)
	if _, err := pipe.Exec(store.ctx); err != nil {
		log.Printf("meter migrate current write: %v", err)
		return
	}
	log.Printf("meter migrate current: %v → %q", oldKeys, meterDeviceKey)
}

// migrateMeterSeries переписывает ip снимков счётчика во временном ряду Redis
// (окно удержания, все месячные сегменты) на единый ключ, сохраняя score.
// Найденные старые IP добавляет в ips.
func migrateMeterSeries(store *redisStore, now time.Time, ips map[string]struct{}) {
	start := recentCutoff(now)
	min := strconv.FormatInt(start.Unix(), 10)
	max := strconv.FormatInt(now.Unix(), 10)
	total := 0
	eachMonth(start, now, func(y int, m time.Month) bool {
		key := redisSeriesKey(time.Date(y, m, 1, 0, 0, 0, 0, time.Local))
		zs, err := store.rdb.ZRangeByScoreWithScores(store.ctx, key, &redis.ZRangeBy{Min: min, Max: max}).Result()
		if err != nil {
			log.Printf("meter migrate series %s: %v", key, err)
			return true
		}
		var stale []any
		var adds []redis.Z
		for _, z := range zs {
			member, _ := z.Member.(string)
			var snap deviceSnapshot
			if json.Unmarshal([]byte(member), &snap) != nil || !isMeterDevice(snap.Values) {
				continue
			}
			if snap.IP == meterDeviceKey {
				continue
			}
			ips[snap.IP] = struct{}{}
			snap.IP = meterDeviceKey
			b, merr := json.Marshal(snap)
			if merr != nil {
				continue
			}
			stale = append(stale, member)
			adds = append(adds, redis.Z{Score: z.Score, Member: string(b)})
		}
		if len(adds) == 0 {
			return true
		}
		pipe := store.rdb.TxPipeline()
		pipe.ZRem(store.ctx, key, stale...)
		pipe.ZAdd(store.ctx, key, adds...)
		pipe.Expire(store.ctx, key, 40*24*time.Hour)
		if _, err := pipe.Exec(store.ctx); err != nil {
			log.Printf("meter migrate series write %s: %v", key, err)
			return true
		}
		total += len(adds)
		return true
	})
	if total > 0 {
		log.Printf("meter migrate series: точек счётчика перенесено: %d", total)
	}
}

// migrateMeterPGAverages переводит исторические усреднённые строки счётчика в
// sunreceiver.averages на единый ключ (колонка ip). Переносятся только строки с
// конкретными старыми IP счётчика (ips) — выборка идёт по индексу (ip, ts), без
// полного скана jsonb. Дополнительно строки фильтруются по наличию тега
// meter_voltage (защита от смены назначения IP).
func migrateMeterPGAverages(pg *pgStore, ips map[string]struct{}) {
	if len(ips) == 0 {
		return
	}
	old := make([]string, 0, len(ips))
	for ip := range ips {
		old = append(old, ip)
	}
	// UPDATE затронуть может сотни тысяч строк (вся история счётчика) — снимаем
	// statement_timeout только для этой транзакции, иначе упрёмся в 15 с пула.
	var n int64
	err := pg.withTx(func(q pgExecer) error {
		if _, err := q.Exec(pg.ctx, `SET LOCAL statement_timeout = 0`); err != nil {
			return err
		}
		tag, err := q.Exec(pg.ctx,
			`UPDATE sunreceiver.averages SET ip=$1 WHERE ip = ANY($2) AND values ? 'meter_voltage'`,
			meterDeviceKey, old)
		if err != nil {
			return err
		}
		n = tag.RowsAffected()
		return nil
	})
	if err != nil {
		log.Printf("meter migrate pg averages: %v", err)
		return
	}
	if n > 0 {
		log.Printf("meter migrate pg averages: строк счётчика перенесено: %d (%v)", n, old)
	}
}
