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
	// redisEnBmsSeriesPrefix — месячный ряд МГНОВЕННЫХ (каждое снятое показание)
	// точек EnBMS: ключи sunreceiver:enbms:series:<YYYY-MM>, ZSET (score = Unix
	// сек. снятия, member = JSON enbmsSeriesPoint, samples=1). 5-минутные средние
	// для PG считает enBms_accumulator.go.
	redisEnBmsSeriesPrefix = "sunreceiver:enbms:series:"
)

// enbmsSeriesKey возвращает ключ месячного сегмента ряда EnBMS для ts.
func enbmsSeriesKey(ts time.Time) string {
	return redisEnBmsSeriesPrefix + ts.Format("2006-01")
}

// SaveEnBmsCurrent пишет текущий снимок EnBMS в HASH (поле = MAC).
func (s *redisStore) SaveEnBmsCurrent(snap enbmsSnapshot) error {
	// КОСТЫЛЬ: старый EnBMS-пулер пишет в ряды bms26 (единый EnBMS-serial).
	serial := legacyEnbmsSerial()
	if serial == "" {
		return nil
	}
	b, err := json.Marshal(enbmsSnapshotToBms26(snap, serial))
	if err != nil {
		return fmt.Errorf("marshal enbms→bms26 %s: %w", snap.MAC, err)
	}
	return s.SaveBms26Current(map[string]string{serial: string(b)})
}

// SaveEnBmsSeries кладёт МГНОВЕННУЮ (сырую) точку в месячный ZSET ряда EnBMS
// (score = секунда снятия; дедупликация по (имя, score) — как SaveBMSSeries).
func (s *redisStore) SaveEnBmsSeries(p enbmsSeriesPoint, ts time.Time) error {
	// КОСТЫЛЬ: старый EnBMS-пулер пишет точки в ряд bms26 (единый EnBMS-serial).
	sn, ok := enbmsAveragedToBms26(p.enbmsAveraged, p.Ts)
	if !ok {
		return nil
	}
	return s.SaveBms26Series(sn, ts)
}

// EnBmsCurrent возвращает текущие снимки EnBMS из HASH (поле = MAC).
func (s *redisStore) EnBmsCurrent() (map[string]enbmsSnapshot, error) {
	m, err := s.rdb.HGetAll(s.ctx, redisEnBmsCurrentKey).Result()
	if err != nil {
		return nil, fmt.Errorf("HGETALL %s: %w", redisEnBmsCurrentKey, err)
	}
	out := make(map[string]enbmsSnapshot, len(m))
	for k, v := range m {
		var snap enbmsSnapshot
		if err := json.Unmarshal([]byte(v), &snap); err != nil {
			continue
		}
		out[k] = snap
	}
	return out, nil
}

// EnBmsOne возвращает текущий снимок одного EnBMS по MAC; nil, если устройства нет.
func (s *redisStore) EnBmsOne(mac string) (*enbmsSnapshot, error) {
	b, err := s.rdb.HGet(s.ctx, redisEnBmsCurrentKey, mac).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var snap enbmsSnapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

// QueryEnBmsSeries возвращает сырые показания одного устройства (по MAC) за
// период [start, end] включительно, по возрастанию времени.
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

// saveEnBmsClosedBuckets пишет готовые 5-минутные усреднённые точки EnBMS в PG
// (sunreceiver.enbms_averages) — гранулярность PG = 1 запись / 5 минут. В
// Redis-ряд 5-минутные средние НЕ пишутся: там сырые показания (saveEnBmsReading).
func saveEnBmsClosedBuckets(pg *pgStore, pts []enbmsAvgPoint) {
	if pg == nil {
		return
	}
	type row struct {
		name  string
		start time.Time
		avg   enbmsAveraged
	}
	pgRows := make([]row, 0, len(pts))
	for _, p := range pts {
		if p.avg.Samples == 0 {
			continue
		}
		pgRows = append(pgRows, row{name: p.name, start: p.start, avg: p.avg})
	}
	if len(pgRows) == 0 {
		return
	}
	if err := retryPg(func() error {
		return pg.withTx(func(q pgExecer) error {
			for _, r := range pgRows {
				sn, ok := enbmsAveragedToBms26(r.avg, r.start.Format(time.RFC3339))
				if !ok {
					continue
				}
				if err := insertBms26AveragedExec(q, pg.ctx, sn.Serial, r.start, sn); err != nil {
					return err
				}
			}
			return nil
		})
	}, 3); err != nil {
		logEnBms("avg pg: %v", err)
	}
}
