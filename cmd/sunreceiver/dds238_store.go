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

// Ключи Redis модуля DDS238 (электросчётчик). Обособлены от инверторов/МАП/DTS017M:
// у счётчика собственные current-HASH и месячный ряд. Общие
// sunreceiver:current / sunreceiver:series:* БОЛЬШЕ не используются для DDS238;
// однако чтение (Current/QuerySeries) собирает DDS238 из этих ключей прозрачно,
// поэтому остальной код/дашборд видят снимок как раньше.
const (
	// redisDds238CurrentKey — HASH текущего снимка DDS238: поле = meterDeviceKey,
	// значение = JSON deviceSnapshot.
	redisDds238CurrentKey = "sunreceiver:dds238:current"
	// redisDds238SeriesPrefix — месячный ряд мгновенных показаний DDS238:
	// sunreceiver:dds238:series:<YYYY-MM>, ZSET (score = Unix-сек, member = JSON).
	redisDds238SeriesPrefix = "sunreceiver:dds238:series:"
)

func dds238SeriesKey(ts time.Time) string {
	return redisDds238SeriesPrefix + ts.Format("2006-01")
}

// SaveDds238Current пишет текущий снимок DDS238 в собственный HASH current.
func (s *redisStore) SaveDds238Current(snap deviceSnapshot) error {
	snap.IP = "" // ip не персистим (обособленное устройство; ключ — name/meterDeviceKey)
	b, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("marshal dds238 current: %w", err)
	}
	return s.rdb.HSet(s.ctx, redisDds238CurrentKey, meterDeviceKey, b).Err()
}

// SaveDds238Series кладёт мгновенную точку в месячный ZSET ряда DDS238
// (дедуп по score).
func (s *redisStore) SaveDds238Series(snap deviceSnapshot, ts time.Time) error {
	snap.IP = "" // ip не персистим
	b, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("marshal dds238 series: %w", err)
	}
	key := dds238SeriesKey(ts)
	pipe := s.rdb.TxPipeline()
	// Убираем прежнюю точку на этой секунде (та же секунда — перезапись).
	if old, err := s.rdb.ZRangeByScore(s.ctx, key, &redis.ZRangeBy{
		Min: strconv.FormatInt(ts.Unix(), 10), Max: strconv.FormatInt(ts.Unix(), 10)}).Result(); err == nil {
		for _, m := range old {
			var q deviceSnapshot
			if json.Unmarshal([]byte(m), &q) == nil && q.IP == snap.IP {
				pipe.ZRem(s.ctx, key, m)
			}
		}
	}
	pipe.ZAdd(s.ctx, key, redis.Z{Score: float64(ts.Unix()), Member: string(b)})
	pipe.Expire(s.ctx, key, 40*24*time.Hour)
	if _, err := pipe.Exec(s.ctx); err != nil {
		return fmt.Errorf("save dds238 series: %w", err)
	}
	return nil
}

// Dds238Current возвращает текущий снимок DDS238; nil, если ещё не было.
func (s *redisStore) Dds238Current() (*deviceSnapshot, error) {
	b, err := s.rdb.HGet(s.ctx, redisDds238CurrentKey, meterDeviceKey).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var snap deviceSnapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return nil, err
	}
	snap.IP = meterDeviceKey
	return &snap, nil
}

// QueryDds238Series возвращает мгновенные снимки DDS238 за [start, end] по
// месячным сегментам собственного ряда.
func (s *redisStore) QueryDds238Series(start, end time.Time) ([]deviceSnapshot, error) {
	if start.After(end) {
		return nil, errors.New("dds238 series: start after end")
	}
	min := strconv.FormatInt(start.Unix(), 10)
	max := strconv.FormatInt(end.Unix(), 10)
	var all []deviceSnapshot
	var queryErr error
	eachMonth(start, end, func(y int, m time.Month) bool {
		key := dds238SeriesKey(time.Date(y, m, 1, 0, 0, 0, 0, time.Local))
		vals, err := s.rdb.ZRangeByScore(s.ctx, key, &redis.ZRangeBy{Min: min, Max: max}).Result()
		if err != nil {
			queryErr = err
			return false
		}
		for _, v := range vals {
			var snap deviceSnapshot
			if json.Unmarshal([]byte(v), &snap) != nil {
				continue
			}
			snap.IP = meterDeviceKey
			all = append(all, snap)
		}
		return true
	})
	if queryErr != nil {
		return nil, queryErr
	}
	return all, nil
}
