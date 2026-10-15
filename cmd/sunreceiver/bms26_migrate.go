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
	"fmt"
	"log"
	"regexp"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// Миграция истории BMS на ключ/схему bms26 (этап 3).
//
// Переносит историю из старых хранилищ в новую:
//   - Redis ряд ANT  sunreceiver:bms:series:*      (member bmsSeriesPoint)
//   - Redis ряд EnBMS sunreceiver:enbms:series:*   (member enbmsSeriesPoint)
//   - PG sunreceiver.bms_averages / enbms_averages (values — *_averaged)
//     → sunreceiver.bms26_averages (values — bms26Snapshot)
//
// Идентификатор: ANT — по ёмкости из старого имени ("AntBms <cap> A/h") →
// serial "AntBMS-DSP-<cap>-NNNNN"; EnBMS — единственный MAC → единственный
// EnBMS-serial (при >1 EnBMS-serial — не мигрируем: неоднозначно).
//
// current-HASH не мигрируем: новый пулер bms26 перезапишет его в первую секунду.

const bms26MigrateMarkerKey = "sunreceiver:migrated:bms26"

// antNameCap — ёмкость из старого имени ANT-устройства ("AntBms 160 A/h ...").
var antNameCap = regexp.MustCompile(`AntBms\s+(\d+)\s*A/h`)

// antBaseCap — ёмкость из нового BASE bms26 ("AntBMS-DSP-160").
var antBaseCap = regexp.MustCompile(`AntBMS-DSP-(\d+)`)

func antCapFromName(name string) (int, bool) {
	return capFromRe(antNameCap, name)
}

func antCapFromBase(base string) (int, bool) {
	return capFromRe(antBaseCap, base)
}

func capFromRe(re *regexp.Regexp, s string) (int, bool) {
	m := re.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

// bms26LegacyMap строит карту старых идентификаторов на новые serial по текущему
// составу bms26 (cap→serial для ANT; enbmsSerial для EnBMS).
func bms26LegacyMap(store *redisStore) (antCapSerial map[int]string, enbmsSerial string, err error) {
	snaps, err := store.Bms26Current()
	if err != nil {
		return nil, "", err
	}
	antCapSerial = map[int]string{}
	var enSerials []string
	for _, sn := range snaps {
		switch sn.BmsType {
		case "AntBMS-DSP":
			if cap, ok := antCapFromBase(sn.Base); ok {
				antCapSerial[cap] = sn.Serial
			}
		case "EnBMS-RS485":
			enSerials = append(enSerials, sn.Serial)
		}
	}
	if len(enSerials) == 1 {
		enbmsSerial = enSerials[0]
	}
	return antCapSerial, enbmsSerial, nil
}

// migrateBms26History — однократный (по маркеру) перенос истории. Идемпотентна.
func migrateBms26History(store *redisStore, pg *pgStore, now time.Time) {
	if store == nil {
		return
	}
	if ok, err := store.rdb.Exists(store.ctx, bms26MigrateMarkerKey).Result(); err != nil {
		log.Printf("bms26 migrate: маркер: %v", err)
		return
	} else if ok > 0 {
		return
	}
	antSer, enSer, err := bms26LegacyMap(store)
	if err != nil {
		log.Printf("bms26 migrate: карта идентификаторов: %v — пропуск", err)
		return
	}
	if len(antSer) == 0 && enSer == "" {
		log.Printf("bms26 migrate: нечего мигрировать (нет соответствия id) — маркер не ставлю")
		return
	}
	log.Printf("bms26 migrate: история → bms26 (ANT: %d ёмкостей, EnBMS serial=%q)", len(antSer), enSer)
	if err := migrateBms26Series(store, antSer, enSer); err != nil {
		log.Printf("bms26 migrate: series: %v — маркер не ставлю", err)
		return
	}
	if pg != nil {
		if err := migrateBms26PG(pg, antSer, enSer); err != nil {
			log.Printf("bms26 migrate: pg: %v — маркер не ставлю", err)
			return
		}
		if err := migrateBms26Errors(pg, antSer, enSer); err != nil {
			log.Printf("bms26 migrate: device_errors: %v — маркер не ставлю", err)
			return
		}
	}
	if err := store.rdb.Set(store.ctx, bms26MigrateMarkerKey, "1", 0).Err(); err != nil {
		log.Printf("bms26 migrate: запись маркера: %v", err)
	}
}

// bms26FromAntPoint — старая точка ANT (bmsSeriesPoint) → bms26Snapshot.
func bms26FromAntPoint(p bmsSeriesPoint, serial string) bms26Snapshot {
	return bms26Snapshot{
		Serial: serial, Timestamp: p.Ts, CellCount: p.CellCount,
		CellsV: p.CellsV, TemperaturesC: p.Temperatures,
		CurrentA: p.CurrentA, PowerW: p.PowerW, Soc: p.Soc,
		CapacityAh: p.CapacityAh, RemainingAh: p.RemainingAh,
		MaxCellV: p.MaxCellV, MinCellV: p.MinCellV, AvgCellV: p.AvgCellV,
	}
}

// bms26FromEnbmsPoint — старая точка EnBMS (enbmsSeriesPoint) → bms26Snapshot.
func bms26FromEnbmsPoint(p enbmsSeriesPoint, serial string) bms26Snapshot {
	return bms26Snapshot{
		Serial: serial, Timestamp: p.Ts, CellCount: p.CellCount,
		CellsV: p.CellsV, TemperaturesC: p.Temperatures,
		CurrentA: p.CurrentA, PowerW: p.PowerW, Soc: p.Soc, Soh: p.Soh,
		CapacityAh: p.CapacityAh, RemainingAh: p.RemainingAh,
		TotalV: p.TotalVoltageV, PortV: p.PortVoltageV, Cycles: p.Cycles,
		MaxCellV: p.MaxCellV, MinCellV: p.MinCellV, AvgCellV: p.AvgCellV,
	}
}

type bms26MigKey struct {
	key    string
	serial string
	score  int64
}

// migrateBms26Series переписывает старые ряды Redis в bms26:series:<month>
// (дедуп по (serial, секунда): несколько старых имён одного устройства → один serial).
func migrateBms26Series(store *redisStore, antSer map[int]string, enSer string) error {
	adds := map[bms26MigKey]string{}
	for _, prefix := range []string{redisBMSSeriesPrefix, redisEnBmsSeriesPrefix} {
		keys, err := store.rdb.Keys(store.ctx, prefix+"*").Result()
		if err != nil {
			return fmt.Errorf("scan %s: %w", prefix, err)
		}
		for _, k := range keys {
			zs, err := store.rdb.ZRangeWithScores(store.ctx, k, 0, -1).Result()
			if err != nil {
				return fmt.Errorf("zrange %s: %w", k, err)
			}
			for _, z := range zs {
				member, _ := z.Member.(string)
				var sn bms26Snapshot
				if prefix == redisBMSSeriesPrefix {
					var p bmsSeriesPoint
					if json.Unmarshal([]byte(member), &p) != nil {
						continue
					}
					cap, ok := antCapFromName(p.Name)
					serial := antSer[cap]
					if !ok || serial == "" {
						continue
					}
					sn = bms26FromAntPoint(p, serial)
				} else {
					if enSer == "" {
						continue
					}
					var p enbmsSeriesPoint
					if json.Unmarshal([]byte(member), &p) != nil {
						continue
					}
					sn = bms26FromEnbmsPoint(p, enSer)
				}
				b, err := json.Marshal(sn)
				if err != nil {
					continue
				}
				ts := time.Unix(int64(z.Score), 0)
				adds[bms26MigKey{key: bms26SeriesKey(ts), serial: sn.Serial, score: int64(z.Score)}] = string(b)
			}
		}
	}
	if len(adds) == 0 {
		return nil
	}
	byKey := map[string][]redis.Z{}
	for mk, member := range adds {
		byKey[mk.key] = append(byKey[mk.key], redis.Z{Score: float64(mk.score), Member: member})
	}
	total := 0
	for key, zs := range byKey {
		pipe := store.rdb.TxPipeline()
		pipe.ZAdd(store.ctx, key, zs...)
		pipe.Expire(store.ctx, key, 40*24*time.Hour)
		if _, err := pipe.Exec(store.ctx); err != nil {
			return fmt.Errorf("zadd %s: %w", key, err)
		}
		total += len(zs)
	}
	log.Printf("bms26 migrate series: точек перенесено: %d", total)
	return nil
}

// migrateBms26PG переносит строки bms_averages/enbms_averages в bms26_averages
// (values — старый *_averaged JSON совместим по ключам с bms26Snapshot).
func migrateBms26PG(pg *pgStore, antSer map[int]string, enSer string) error {
	// ANT: по каждому старому имени → serial по ёмкости.
	if rows, err := pg.pool.Query(pg.ctx, `SELECT DISTINCT name FROM sunreceiver.bms_averages`); err == nil {
		var names []string
		for rows.Next() {
			var n string
			if rows.Scan(&n) == nil {
				names = append(names, n)
			}
		}
		rows.Close()
		for _, name := range names {
			cap, ok := antCapFromName(name)
			serial := antSer[cap]
			if !ok || serial == "" {
				continue
			}
			if _, err := pg.pool.Exec(pg.ctx, `
INSERT INTO sunreceiver.bms26_averages (name, ts, values)
SELECT $1, ts, values FROM sunreceiver.bms_averages WHERE name=$2
ON CONFLICT (name, ts) DO NOTHING`, serial, name); err != nil {
				return fmt.Errorf("pg bms_averages %s: %w", name, err)
			}
		}
	} else {
		return fmt.Errorf("pg select bms_averages names: %w", err)
	}
	// EnBMS: единственный MAC → serial.
	if enSer != "" {
		if rows, err := pg.pool.Query(pg.ctx, `SELECT DISTINCT name FROM sunreceiver.enbms_averages`); err == nil {
			var names []string
			for rows.Next() {
				var n string
				if rows.Scan(&n) == nil {
					names = append(names, n)
				}
			}
			rows.Close()
			for _, name := range names {
				if _, err := pg.pool.Exec(pg.ctx, `
INSERT INTO sunreceiver.bms26_averages (name, ts, values)
SELECT $1, ts, values FROM sunreceiver.enbms_averages WHERE name=$2
ON CONFLICT (name, ts) DO NOTHING`, enSer, name); err != nil {
					return fmt.Errorf("pg enbms_averages %s: %w", name, err)
				}
			}
		} else {
			return fmt.Errorf("pg select enbms_averages names: %w", err)
		}
	}
	return nil
}

// pruneBms26Legacy удаляет старые данные BMS (Redis-ключи ANT/EnBMS и PG-таблицы
// bms_averages/enbms_averages) ПОСЛЕ успешной миграции и проверки. Вызывается
// отдельно (однократно), не в startup.
func pruneBms26Legacy(store *redisStore, pg *pgStore) {
	if store != nil {
		keys, _ := store.rdb.Keys(store.ctx, redisBMSSeriesPrefix+"*").Result()
		keys = append(keys, redisBMSKey, redisEnBmsCurrentKey)
		if e, _ := store.rdb.Keys(store.ctx, redisEnBmsSeriesPrefix+"*").Result(); len(e) > 0 {
			keys = append(keys, e...)
		}
		if len(keys) > 0 {
			if err := store.rdb.Del(store.ctx, keys...).Err(); err != nil {
				log.Printf("bms26 prune redis: %v", err)
			} else {
				log.Printf("bms26 prune redis: удалено ключей: %d", len(keys))
			}
		}
	}
	if pg != nil {
		for _, tbl := range []string{"bms_averages", "enbms_averages"} {
			if _, err := pg.pool.Exec(pg.ctx, "DELETE FROM sunreceiver."+tbl); err != nil {
				log.Printf("bms26 prune pg %s: %v", tbl, err)
			} else {
				log.Printf("bms26 prune pg: очищена таблица %s", tbl)
			}
		}
	}
}

// migrateBms26Errors переносит историю ошибок BMS в device_errors: строки
// kind='antbms' (device = старое имя) → kind='bms', device=serial (по ёмкости);
// kind='enbms' (device = MAC) → kind='bms', device=enbms-serial. Перенос «как есть»
// (INSERT ... SELECT ON CONFLICT DO NOTHING), затем удаление старых строк.
func migrateBms26Errors(pg *pgStore, antSer map[int]string, enSer string) error {
	type kindMap struct {
		kind string
		ser  func(device string) string
	}
	var maps []kindMap
	maps = append(maps, kindMap{"antbms", func(dev string) string {
		cap, ok := antCapFromName(dev)
		if !ok {
			return ""
		}
		return antSer[cap]
	}})
	if enSer != "" {
		maps = append(maps, kindMap{"enbms", func(string) string { return enSer }})
	}
	for _, km := range maps {
		rows, err := pg.pool.Query(pg.ctx,
			`SELECT DISTINCT device FROM sunreceiver.device_errors WHERE kind=$1`, km.kind)
		if err != nil {
			return fmt.Errorf("select device_errors %s: %w", km.kind, err)
		}
		var devs []string
		for rows.Next() {
			var d string
			if rows.Scan(&d) == nil {
				devs = append(devs, d)
			}
		}
		rows.Close()
		for _, dev := range devs {
			serial := km.ser(dev)
			if serial == "" {
				continue
			}
			if _, err := pg.pool.Exec(pg.ctx, `
INSERT INTO sunreceiver.device_errors (device, kind, code, msg, ts)
SELECT $1, 'bms', code, msg, ts FROM sunreceiver.device_errors
 WHERE device=$2 AND kind=$3
ON CONFLICT (device, kind, code, ts) DO NOTHING`, serial, dev, km.kind); err != nil {
				return fmt.Errorf("migrate device_errors %s=%s: %w", km.kind, dev, err)
			}
			if _, err := pg.pool.Exec(pg.ctx,
				`DELETE FROM sunreceiver.device_errors WHERE device=$1 AND kind=$2`, dev, km.kind); err != nil {
				return fmt.Errorf("delete device_errors %s=%s: %w", km.kind, dev, err)
			}
		}
	}
	return nil
}
