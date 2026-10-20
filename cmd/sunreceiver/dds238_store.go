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

// Ключи Redis модуля DDS238 (электросчётчик). Обособлены от инверторов/МАП/DTS017M:
// у счётчика собственные current-HASH и ряд RedisTimeSeries. Общие
// sunreceiver:current / sunreceiver:series:* БОЛЬШЕ не используются для DDS238.
// Читаются отдельными методами (Dds238Current/QueryDds238Series/Dds238Averages);
// в общие читалки (снят) НЕ подмешиваются.
const (
	// redisDds238CurrentKey — HASH текущего снимка DDS238: поле = meterDeviceKey,
	// значение = JSON deviceSnapshot.
	redisDds238CurrentKey = "sunreceiver:dds238:current"
	// sunreceiver:ts:dds238:* (серии RedisTimeSeries).
)

// SaveDds238Current пишет текущий снимок DDS238 в собственный HASH current.
func (s *redisStore) SaveDds238Current(snap deviceSnapshot) error {
	snap.IP = "" // ip не персистим (обособленное устройство; ключ — name/meterDeviceKey)
	b, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("marshal dds238 current: %w", err)
	}
	return s.rdb.HSet(s.ctx, redisDds238CurrentKey, meterDeviceKey, b).Err()
}

// SaveDds238Series кладёт мгновенную точку в ряд RedisTimeSeries ряда DDS238
// (дедуп по score).
func (s *redisStore) SaveDds238Series(snap deviceSnapshot, ts time.Time) error {
	pipe := s.rdb.TxPipeline()
	s.tsAddValueContract(pipe, "dds238", meterDeviceKey, snap.Name, "", snap.DeviceSN, "", ts, snap.Values)
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
// сериям RedisTimeSeries собственного ряда.
func (s *redisStore) QueryDds238Series(start, end time.Time) ([]deviceSnapshot, error) {
	if start.After(end) {
		return nil, errors.New("dds238 series: start after end")
	}
	series, err := s.tsMRange(start, end, "kind=dds238")
	if err != nil {
		return nil, err
	}
	out := tsGroupSnapshots(series)
	for i := range out {
		out[i].IP = meterDeviceKey
	}
	return out, nil
}
