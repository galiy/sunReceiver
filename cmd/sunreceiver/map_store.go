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

// Ключи Redis модуля МАП (kindMAP). Обособлены от инверторов/MPPT/DDS238:
// собственные current-HASH и месячный ряд. Общие sunreceiver:current / :series:*
// для МАП больше не используются. Читаются отдельными методами
// (MapCurrent/QueryMapSeries/MapAverages); в общие читалки
// (снят) НЕ подмешиваются.
const (
	// redisMapCurrentKey — HASH текущего снимка МАП: поле = mapDevKey.
	redisMapCurrentKey = "sunreceiver:map:current"
	// redisMapSeriesPrefix — месячный ряд мгновенных показаний МАП.
	redisMapSeriesPrefix = "sunreceiver:map:series:"
)

func mapSeriesKey(ts time.Time) string {
	return redisMapSeriesPrefix + ts.Format("2006-01")
}

// saveMapSnapshot пишет снимок МАП (с merge окон батареи/сети) в обособленные
// ключи: current-HASH + месячный ряд.
func (s *redisStore) saveMapSnapshot(snap deviceSnapshot, ts time.Time) error {
	// merge окон заряда/разряда (mergeMAPSnap).
	prevMember := ""
	s.mapMu.Lock()
	if p, had := s.mapWin[mapDevKey]; had {
		prevMember = p.member
	}
	s.mapMu.Unlock()
	snap = mergeMAPSnap(snap, prevMember)
	snap.IP = "" // ip не персистим (обособленное устройство; ключ — name/mapDevKey)

	b, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("marshal map snapshot: %w", err)
	}
	key := mapSeriesKey(ts)
	score := strconv.FormatInt(ts.Unix(), 10)
	old, err := s.rdb.ZRangeByScore(s.ctx, key, &redis.ZRangeBy{Min: score, Max: score}).Result()
	if err != nil {
		return fmt.Errorf("map dedup %s: %w", key, err)
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
	pipe.HSet(s.ctx, redisMapCurrentKey, mapDevKey, b)
	if len(stale) > 0 {
		pipe.ZRem(s.ctx, key, stale...)
	}
	pipe.ZAdd(s.ctx, key, redis.Z{Score: float64(ts.Unix()), Member: string(b)})
	pipe.Expire(s.ctx, key, 40*24*time.Hour)
	if _, err := pipe.Exec(s.ctx); err != nil {
		return fmt.Errorf("save map %s: %w", snap.IP, err)
	}
	s.mapMu.Lock()
	if s.mapWin == nil {
		s.mapWin = map[string]mapWinMember{}
	}
	s.mapWin[mapDevKey] = mapWinMember{member: string(b)}
	s.mapMu.Unlock()
	return nil
}

// MapCurrent возвращает текущий снимок МАП; nil, если ещё не было.
func (s *redisStore) MapCurrent() (*deviceSnapshot, error) {
	b, err := s.rdb.HGet(s.ctx, redisMapCurrentKey, mapDevKey).Bytes()
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
	snap.IP = mapDevKey
	return &snap, nil
}

// QueryMapSeries возвращает мгновенные снимки МАП за [start, end] по месячным
// сегментам собственного ряда.
func (s *redisStore) QueryMapSeries(start, end time.Time) ([]deviceSnapshot, error) {
	if start.After(end) {
		return nil, errors.New("map series: start after end")
	}
	min := strconv.FormatInt(start.Unix(), 10)
	max := strconv.FormatInt(end.Unix(), 10)
	var all []deviceSnapshot
	var queryErr error
	eachMonth(start, end, func(y int, m time.Month) bool {
		key := mapSeriesKey(time.Date(y, m, 1, 0, 0, 0, 0, time.Local))
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
			snap.IP = mapDevKey
			all = append(all, snap)
		}
		return true
	})
	if queryErr != nil {
		return nil, queryErr
	}
	return all, nil
}
