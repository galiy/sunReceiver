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

// Ключи Redis модуля DTS017M. Полностью обособлены от инверторов/МАП/DDS238:
// счётчик имеет собственные current-HASH и ряд RedisTimeSeries. Общие
// sunreceiver:current / sunreceiver:series:* НЕ используются.
const (
	// redisDts017CurrentKey — HASH текущего снимка DTS017M: поле = ключ устройства
	// (dts017DeviceKey), значение = JSON dts017Snapshot. Перезаписывается опросом.
	redisDts017CurrentKey = "sunreceiver:dts017m:current"
	// ключи sunreceiver:ts:dts017m:* (серии RedisTimeSeries; score = Unix-секунды,
	// member = JSON dts017Snapshot). Окно удержания — 2 календарных суток (PurgeOld).
)

// SaveDts017Current пишет текущий снимок DTS017M в собственный HASH current.
func (s *redisStore) SaveDts017Current(snap dts017Snapshot) error {
	b, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("marshal dts017 current %s: %w", snap.Name, err)
	}
	return s.rdb.HSet(s.ctx, redisDts017CurrentKey, dts017DeviceKey, b).Err()
}

// SaveDts017Series кладёт мгновенную точку в ряд RedisTimeSeries ряда DTS017M
// (score = секунда снятия; дедупликация по (устройство, score)).
func (s *redisStore) SaveDts017Series(snap dts017Snapshot, ts time.Time) error {
	pipe := s.rdb.TxPipeline()
	for tag, v := range snap.Values {
		s.tsAdd(pipe, "dts017m", snap.Name, snap.Name, "", "", tag, ts, v)
	}
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
// включительно по сериям RedisTimeSeries собственного ряда.
func (s *redisStore) QueryDts017Series(start, end time.Time) ([]dts017Snapshot, error) {
	if start.After(end) {
		return nil, errors.New("dts017 series: start after end")
	}
	series, err := s.tsMRange(start, end, "kind=dts017m")
	if err != nil {
		return nil, err
	}
	snaps := tsGroupSnapshots(series)
	out := make([]dts017Snapshot, 0, len(snaps))
	for _, sn := range snaps {
		out = append(out, dts017Snapshot{Name: sn.Name, Timestamp: sn.Timestamp, Values: vcToFloatMap(sn.Values)})
	}
	return out, nil
}
