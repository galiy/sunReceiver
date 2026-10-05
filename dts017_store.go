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

// Ключи Redis модуля DTS017M. Полностью обособлены от инверторов/МАП/DDS238:
// счётчик имеет собственные current-HASH и месячный ряд. Общие
// sunreceiver:current / sunreceiver:series:* НЕ используются.
const (
	// redisDts017CurrentKey — HASH текущего снимка DTS017M: поле = ключ устройства
	// (dts017DeviceKey), значение = JSON dts017Snapshot. Перезаписывается опросом.
	redisDts017CurrentKey = "sunreceiver:dts017m:current"
	// redisDts017SeriesPrefix — месячный ряд мгновенных показаний DTS017M:
	// ключи sunreceiver:dts017m:series:<YYYY-MM>, ZSET (score = Unix-секунды,
	// member = JSON dts017Snapshot). Окно удержания — 2 календарных суток (PurgeOld).
	redisDts017SeriesPrefix = "sunreceiver:dts017m:series:"
)

// dts017SeriesKey возвращает ключ месячного сегмента ряда DTS017M для ts.
func dts017SeriesKey(ts time.Time) string {
	return redisDts017SeriesPrefix + ts.Format("2006-01")
}

// SaveDts017Current пишет текущий снимок DTS017M в собственный HASH current.
func (s *redisStore) SaveDts017Current(snap dts017Snapshot) error {
	b, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("marshal dts017 current %s: %w", snap.Name, err)
	}
	return s.rdb.HSet(s.ctx, redisDts017CurrentKey, dts017DeviceKey, b).Err()
}

// SaveDts017Series кладёт мгновенную точку в месячный ZSET ряда DTS017M
// (score = секунда снятия; дедупликация по (устройство, score)).
func (s *redisStore) SaveDts017Series(snap dts017Snapshot, ts time.Time) error {
	b, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("marshal dts017 series %s: %w", snap.Name, err)
	}
	key := dts017SeriesKey(ts)
	score := strconv.FormatInt(ts.Unix(), 10)
	old, err := s.rdb.ZRangeByScore(s.ctx, key, &redis.ZRangeBy{Min: score, Max: score}).Result()
	if err != nil {
		return fmt.Errorf("dts017 series dedup %s: %w", key, err)
	}
	var stale []any
	for _, m := range old {
		var q dts017Snapshot
		if json.Unmarshal([]byte(m), &q) != nil {
			continue
		}
		if q.Name == snap.Name {
			stale = append(stale, m)
		}
	}
	pipe := s.rdb.TxPipeline()
	if len(stale) > 0 {
		pipe.ZRem(s.ctx, key, stale...)
	}
	pipe.ZAdd(s.ctx, key, redis.Z{Score: float64(ts.Unix()), Member: string(b)})
	// Держим историю не меньше месяца; эффективное окно задаёт PurgeOld (2 суток).
	pipe.Expire(s.ctx, key, 40*24*time.Hour)
	if _, err := pipe.Exec(s.ctx); err != nil {
		return fmt.Errorf("save dts017 series %s: %w", snap.Name, err)
	}
	return nil
}

// Dts017Current возвращает текущий снимок DTS017M; nil, если ещё не было.
func (s *redisStore) Dts017Current() (*dts017Snapshot, error) {
	b, err := s.rdb.HGet(s.ctx, redisDts017CurrentKey, dts017DeviceKey).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var snap dts017Snapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

// QueryDts017Series возвращает мгновенные снимки DTS017M за период [start, end]
// включительно по месячным сегментам собственного ряда.
func (s *redisStore) QueryDts017Series(start, end time.Time) ([]dts017Snapshot, error) {
	if start.After(end) {
		return nil, errors.New("dts017 series: start after end")
	}
	min := strconv.FormatInt(start.Unix(), 10)
	max := strconv.FormatInt(end.Unix(), 10)
	var all []dts017Snapshot
	var queryErr error
	eachMonth(start, end, func(y int, m time.Month) bool {
		key := dts017SeriesKey(time.Date(y, m, 1, 0, 0, 0, 0, time.Local))
		vals, err := s.rdb.ZRangeByScore(s.ctx, key, &redis.ZRangeBy{Min: min, Max: max}).Result()
		if err != nil {
			queryErr = err
			return false
		}
		for _, v := range vals {
			var snap dts017Snapshot
			if json.Unmarshal([]byte(v), &snap) != nil {
				continue
			}
			all = append(all, snap)
		}
		return true
	})
	if queryErr != nil {
		return nil, queryErr
	}
	return all, nil
}
