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
	"time"

	"github.com/redis/go-redis/v9"
)

// Ключи Redis модуля МАП (kindMAP). Обособлены от инверторов/MPPT/DDS238:
// собственные current-HASH и ряд RedisTimeSeries. Общие sunreceiver:current / :series:*
// для МАП больше не используются. Читаются отдельными методами
// (MapCurrent/QueryMapSeries/MapAverages); в общие читалки
// (снят) НЕ подмешиваются.
const (
	// redisMapCurrentKey — HASH текущего снимка МАП: поле = mapDevKey.
	redisMapCurrentKey = "sunreceiver:map:current"
)

// saveMapSnapshot пишет снимок МАП (с merge окон батареи/сети) в обособленные
// ключи: current-HASH + ряд RedisTimeSeries.
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
	pipe := s.rdb.TxPipeline()
	pipe.HSet(s.ctx, redisMapCurrentKey, mapDevKey, b)
	s.tsAddValueContract(pipe, "map", mapDevKey, snap.Name, "", snap.DeviceSN, ts, snap.Values)
	if _, err := pipe.Exec(s.ctx); err != nil {
		return fmt.Errorf("save map: %w", err)
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
	series, err := s.tsMRange(start, end, "kind=map")
	if err != nil {
		return nil, err
	}
	out := tsGroupSnapshots(series)
	for i := range out {
		out[i].IP = mapDevKey
	}
	return out, nil
}
