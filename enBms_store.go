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
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// Ключи Redis модуля EnBMS. Обособлены от ANT BMS (у того HASH
// sunreceiver:bms и ряд sunreceiver:bms:series:*).
const (
	// redisEnBmsCurrentKey — HASH текущего снимка EnBMS: поле = MAC,
	// значение = JSON enbmsSnapshot. Перезаписывается после каждого опроса.
	redisEnBmsCurrentKey = "sunreceiver:enbms:current"
	// redisEnBmsSeriesPrefix — месячный ряд 5-минутных усреднённых точек:
	// ключи sunreceiver:enbms:series:<YYYY-MM>, ZSET (score = Unix сек).
	redisEnBmsSeriesPrefix = "sunreceiver:enbms:series:"
)

// enbmsSeriesKey возвращает ключ месячного сегмента ряда EnBMS для ts.
func enbmsSeriesKey(ts time.Time) string {
	return redisEnBmsSeriesPrefix + ts.Format("2006-01")
}

// SaveEnBmsCurrent пишет текущий снимок EnBMS в HASH (поле = MAC).
func (s *redisStore) SaveEnBmsCurrent(snap enbmsSnapshot) error {
	b, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("marshal enbms current %s: %w", snap.MAC, err)
	}
	return s.rdb.HSet(s.ctx, redisEnBmsCurrentKey, snap.MAC, b).Err()
}

// SaveEnBmsSeries кладёт 5-минутную усреднённую точку в месячный ZSET ряда EnBMS
// (дедупликация по (имя, score) — как SaveBMSSeries).
func (s *redisStore) SaveEnBmsSeries(p enbmsSeriesPoint, ts time.Time) error {
	b, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("marshal enbms series %s: %w", p.Name, err)
	}
	key := enbmsSeriesKey(ts)
	score := strconv.FormatInt(ts.Unix(), 10)
	old, err := s.rdb.ZRangeByScore(s.ctx, key, &redis.ZRangeBy{Min: score, Max: score}).Result()
	if err != nil {
		return fmt.Errorf("enbms series dedup %s: %w", key, err)
	}
	var stale []any
	for _, m := range old {
		var q enbmsSeriesPoint
		if json.Unmarshal([]byte(m), &q) != nil {
			continue
		}
		if q.Name == p.Name {
			stale = append(stale, m)
		}
	}
	pipe := s.rdb.TxPipeline()
	if len(stale) > 0 {
		pipe.ZRem(s.ctx, key, stale...)
	}
	pipe.ZAdd(s.ctx, key, redis.Z{Score: float64(ts.Unix()), Member: string(b)})
	pipe.Expire(s.ctx, key, 40*24*time.Hour)
	if _, err := pipe.Exec(s.ctx); err != nil {
		return fmt.Errorf("save enbms series %s: %w", p.Name, err)
	}
	return nil
}

// QueryEnBmsSeries возвращает 5-минутные усреднённые точки одного устройства
// (по MAC) за период [start, end] включительно, по возрастанию времени.
func (s *redisStore) QueryEnBmsSeries(mac string, start, end time.Time) ([]enbmsSeriesPoint, error) {
	if start.After(end) {
		return nil, errors.New("start after end")
	}
	min := strconv.FormatInt(start.Unix(), 10)
	max := strconv.FormatInt(end.Unix(), 10)
	var all []enbmsSeriesPoint
	var queryErr error
	eachMonth(start, end, func(y int, m time.Month) bool {
		key := enbmsSeriesKey(time.Date(y, m, 1, 0, 0, 0, 0, time.Local))
		vals, err := s.rdb.ZRangeByScore(s.ctx, key, &redis.ZRangeBy{Min: min, Max: max}).Result()
		if err != nil {
			queryErr = err
			return false
		}
		for _, v := range vals {
			var p enbmsSeriesPoint
			if json.Unmarshal([]byte(v), &p) != nil {
				continue
			}
			if p.Name != mac {
				continue
			}
			all = append(all, p)
		}
		return true
	})
	if queryErr != nil {
		return nil, queryErr
	}
	return all, nil
}

// saveEnBmsClosedBuckets пишет готовые 5-минутные точки EnBMS в Redis (ряд,
// окно 40 суток) и в PG (вечно). partial=true (остановка) — только в Redis.
func saveEnBmsClosedBuckets(store *redisStore, pg *pgStore, pts []enbmsAvgPoint, partial bool) {
	type row struct {
		name    string
		display string
		start   time.Time
		avg     enbmsAveraged
	}
	var pgRows []row
	for _, p := range pts {
		if p.avg.Samples == 0 {
			continue
		}
		sp := enbmsSeriesPoint{Name: p.name, Display: p.display, Ts: p.start.Format(time.RFC3339)}
		sp.enbmsAveraged = p.avg
		if err := store.SaveEnBmsSeries(sp, p.start); err != nil {
			logEnBms("avg redis %s %s: %v", p.name, p.start.Format(time.RFC3339), err)
		}
		if pg != nil && !partial {
			pgRows = append(pgRows, row{name: p.name, display: p.display, start: p.start, avg: p.avg})
		}
		if partial {
			logEnBms("avg: дописан неполный 5-минутный промежуток %s %s (снимков: %d)",
				p.name, p.start.Format(time.RFC3339), p.avg.Samples)
		}
	}
	if pg != nil && len(pgRows) > 0 {
		if err := retryPg(func() error {
			return pg.withTx(func(q pgExecer) error {
				for _, r := range pgRows {
					if err := insertEnBmsAveragedExec(q, pg.ctx, r.name, r.start, r.avg); err != nil {
						return err
					}
				}
				return nil
			})
		}, 3); err != nil {
			logEnBms("avg pg: %v", err)
		}
	}
}
