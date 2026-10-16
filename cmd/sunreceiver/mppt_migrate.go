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
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// mpptMigrateMarkerKey — маркер однократной миграции ключей КЭС на новый формат
// (без host). Пока маркер есть — миграция пропускается.
const mpptMigrateMarkerKey = "sunreceiver:migrated:mppt-key"

// isLegacyMPPTKey — старый формат ключа КЭС: "<host>#mppt-<uid>" (или вырожденный
// "<host>#mppt<slot>"). Новый формат — "mppt-<serial>" (см. devKey).
func isLegacyMPPTKey(ip string) bool {
	return strings.Contains(ip, "#mppt")
}

// newMPPTKeyFromLegacy переводит старый ключ КЭС в новый.
//
//	"<host>#mppt-<uid>" → "mppt-<uid>"
//	"<host>#mppt<slot>" → "mppt-<slot>"
func newMPPTKeyFromLegacy(old string) (string, bool) {
	i := strings.Index(old, "#mppt")
	if i < 0 {
		return "", false
	}
	suffix := old[i+1:] // "mppt-<uid>" либо "mppt<slot>"
	if strings.HasPrefix(suffix, "mppt-") {
		return suffix, true
	}
	return "mppt-" + strings.TrimPrefix(suffix, "mppt"), true
}

// migrateMPPTDeviceKeys — однократная миграция ключей КЭС с "<host>#mppt-<uid>" на
// "mppt-<serial>" (без host), чтобы история не срезалась при смене формата. Как в
// migrateMeterDeviceKey:
//   - current-HASH: переводит свежайший снимок каждого КЭС под новый ключ, старые поля удаляет;
//   - временной ряд Redis: переписывает IP снимков КЭС на новый ключ, сохраняя score;
//   - PG averages: переводит исторические строки (ip LIKE '%#mppt%').
//
// Идемпотентна (маркер ставится только после полного успеха), выполняется при старте
// ДО запуска пулеров/аккумулятора.
func migrateMPPTDeviceKeys(store *redisStore, pg *pgStore, now time.Time) {
	if store == nil {
		return
	}
	if ok, err := store.rdb.Exists(store.ctx, mpptMigrateMarkerKey).Result(); err != nil {
		log.Printf("mppt migrate: проверка маркера: %v", err)
		return
	} else if ok > 0 {
		return
	}
	log.Printf("mppt migrate: ключи КЭС <host>#mppt-<uid> → mppt-<serial>")
	if err := migrateMPPTCurrent(store); err != nil {
		log.Printf("mppt migrate: current: %v — маркер не ставлю, повтор при следующем старте", err)
		return
	}
	if err := migrateMPPTSeries(store, now); err != nil {
		log.Printf("mppt migrate: series: %v — маркер не ставлю, повтор при следующем старте", err)
		return
	}
	if pg == nil {
		log.Printf("mppt migrate: PG недоступен — маркер не ставлю, миграция повторится")
		return
	}
	if err := migrateMPPTPGAverages(pg); err != nil {
		log.Printf("mppt migrate: pg averages: %v — маркер не ставлю, повтор при следующем старте", err)
		return
	}
	if err := store.rdb.Set(store.ctx, mpptMigrateMarkerKey, "1", 0).Err(); err != nil {
		log.Printf("mppt migrate: запись маркера: %v", err)
	}
}

// migrateMPPTCurrent переводит свежайший снимок каждого КЭС на новый ключ.
func migrateMPPTCurrent(store *redisStore) error {
	cur, err := store.rdb.HGetAll(store.ctx, redisCurrentKey).Result()
	if err != nil {
		return err
	}
	type grp struct {
		newest deviceSnapshot
		found  bool
		olds   []string
	}
	groups := map[string]*grp{}
	for field, raw := range cur {
		var snap deviceSnapshot
		if json.Unmarshal([]byte(raw), &snap) != nil {
			continue
		}
		var newKey string
		isOld := false
		if isLegacyMPPTKey(field) {
			newKey, isOld = newMPPTKeyFromLegacy(field)
		} else if strings.HasPrefix(field, "mppt-") {
			newKey = field
		} else {
			continue
		}
		g := groups[newKey]
		if g == nil {
			g = &grp{}
			groups[newKey] = g
		}
		if isOld {
			g.olds = append(g.olds, field)
		}
		if !g.found || snap.Timestamp > g.newest.Timestamp {
			g.newest, g.found = snap, true
		}
	}
	moved := 0
	pipe := store.rdb.TxPipeline()
	for newKey, g := range groups {
		if len(g.olds) == 0 || !g.found {
			continue
		}
		g.newest.IP = newKey
		b, merr := json.Marshal(g.newest)
		if merr != nil {
			continue
		}
		pipe.HSet(store.ctx, redisCurrentKey, newKey, string(b))
		pipe.HDel(store.ctx, redisCurrentKey, g.olds...)
		moved++
	}
	if moved == 0 {
		return nil
	}
	if _, err := pipe.Exec(store.ctx); err != nil {
		return err
	}
	log.Printf("mppt migrate current: перенесено устройств: %d", moved)
	return nil
}

// migrateMPPTSeries переписывает IP снимков КЭС во временном ряду Redis на новый ключ.
func migrateMPPTSeries(store *redisStore, now time.Time) error {
	start := recentCutoff(now)
	min := strconv.FormatInt(start.Unix(), 10)
	max := strconv.FormatInt(now.Unix(), 10)
	total := 0
	var migErr error
	eachMonth(start, now, func(y int, m time.Month) bool {
		key := redisSeriesKey(time.Date(y, m, 1, 0, 0, 0, 0, time.Local))
		zs, err := store.rdb.ZRangeByScoreWithScores(store.ctx, key, &redis.ZRangeBy{Min: min, Max: max}).Result()
		if err != nil {
			migErr = fmt.Errorf("series %s: %w", key, err)
			return false
		}
		var stale []any
		var adds []redis.Z
		for _, z := range zs {
			member, _ := z.Member.(string)
			var snap deviceSnapshot
			if json.Unmarshal([]byte(member), &snap) != nil || !isLegacyMPPTKey(snap.IP) {
				continue
			}
			newKey, ok := newMPPTKeyFromLegacy(snap.IP)
			if !ok {
				continue
			}
			snap.IP = newKey
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
			migErr = fmt.Errorf("series write %s: %w", key, err)
			return false
		}
		total += len(adds)
		return true
	})
	if migErr != nil {
		return migErr
	}
	if total > 0 {
		log.Printf("mppt migrate series: точек КЭС перенесено: %d", total)
	}
	return nil
}

// migrateMPPTPGAverages переводит исторические строки КЭС в sunreceiver.averages на
// новый ключ. Кандидаты — строки с ip LIKE '%#mppt%'; каждому считаем новый ключ и
// обновляем адресно (guard по тегу pv1_power — защита от смены назначения ip).
func migrateMPPTPGAverages(pg *pgStore) error {
	rows, err := pg.pool.Query(pg.ctx, `SELECT DISTINCT ip FROM sunreceiver.averages WHERE ip LIKE '%#mppt%'`)
	if err != nil {
		return err
	}
	var oldIPs []string
	for rows.Next() {
		var ip string
		if err := rows.Scan(&ip); err != nil {
			rows.Close()
			return err
		}
		oldIPs = append(oldIPs, ip)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(oldIPs) == 0 {
		return nil
	}
	updates := map[string]string{} // old → new
	for _, old := range oldIPs {
		if nk, ok := newMPPTKeyFromLegacy(old); ok {
			updates[old] = nk
		}
	}
	if len(updates) == 0 {
		return nil
	}
	var total int64
	err = pg.withTx(func(q pgExecer) error {
		if _, err := q.Exec(pg.ctx, `SET LOCAL statement_timeout = 0`); err != nil {
			return err
		}
		for old, nk := range updates {
			// PK averages = (ip, ts): если под новым ключом точка с тем же ts уже
			// есть (пишется с момента переключения), старую строку удаляем, иначе
			// UPDATE упрётся в нарушение уникальности. Удаляем только «проигравшие»
			// дубли, остальную историю переносим.
			//
			// Guard по MPPT-тегам: у старых усреднённых строк КЭС набор тегов
			// варьируется (исторически — только energy_today/ac_active_power, позже
			// добавился pv1_power), поэтому проверяем любой из характерных тегов.
			if _, err := q.Exec(pg.ctx,
				`DELETE FROM sunreceiver.averages o USING sunreceiver.averages n
				 WHERE o.ip=$1 AND n.ip=$2 AND o.ts=n.ts
				   AND (o.values ? 'pv1_power' OR o.values ? 'ac_active_power' OR o.values ? 'energy_today')`,
				old, nk); err != nil {
				return err
			}
			tag, err := q.Exec(pg.ctx,
				`UPDATE sunreceiver.averages SET ip=$1 WHERE ip=$2
				   AND (values ? 'pv1_power' OR values ? 'ac_active_power' OR values ? 'energy_today')`,
				nk, old)
			if err != nil {
				return err
			}
			total += tag.RowsAffected()
		}
		return nil
	})
	if err != nil {
		return err
	}
	if total > 0 {
		log.Printf("mppt migrate pg averages: строк КЭС перенесено: %d", total)
	}
	return nil
}
