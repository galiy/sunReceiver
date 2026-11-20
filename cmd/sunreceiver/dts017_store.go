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
		s.tsAdd(pipe, "dts017m", snap.Name, snap.Name, "", "", "", tag, ts, v)
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

// QueryDts017SeriesBackfill читает окно удержания Redis для стартового добора
// 5-минутных средних БЕЗ выгрузки сырья: два серверных TS.MRANGE с агрегацией
// по 5-минутным бакетам — avg для обычных тегов и last для накопительных
// (энергии). Сырьё за 1.5–2 суток блокировало Redis на 5–6 с.
func (s *redisStore) QueryDts017SeriesBackfill(start, end time.Time) ([]dts017Snapshot, error) {
	bucket := int64(avgStep / time.Millisecond)
	avgSeries, err := s.tsMRangeAgg(start, end, "avg", bucket, "kind=dts017m")
	if err != nil {
		return nil, err
	}
	lastSeries, err := s.tsMRangeAgg(start, end, "last", bucket, "kind=dts017m")
	if err != nil {
		return nil, err
	}
	snaps := tsGroupSnapshots(mergeDts017Backfill(avgSeries, lastSeries))
	out := make([]dts017Snapshot, 0, len(snaps))
	for _, sn := range snaps {
		out = append(out, dts017Snapshot{Name: sn.Name, Timestamp: sn.Timestamp, Values: vcToFloatMap(sn.Values)})
	}
	return out, nil
}

// mergeDts017Backfill заменяет значения накопительных тегов на серии last
// (по ключу серии), оставляя avg для остальных.
func mergeDts017Backfill(avgSeries, lastSeries []tsSeries) []tsSeries {
	lastByKey := make(map[string]tsSeries, len(lastSeries))
	for _, ser := range lastSeries {
		lastByKey[ser.Key] = ser
	}
	out := make([]tsSeries, 0, len(avgSeries))
	for _, ser := range avgSeries {
		if dts017CumulativeTags[tsLabelUnescape(ser.Labels["tag"])] {
			if ls, ok := lastByKey[ser.Key]; ok {
				out = append(out, ls)
				continue
			}
		}
		out = append(out, ser)
	}
	return out
}

// QueryDts017SeriesAgg — как QueryDts017Series, но с серверной агрегацией avg
// (bucketMs; 0 — сырьё) для графиков. Единая с остальными рядами агрегация.
func (s *redisStore) QueryDts017SeriesAgg(start, end time.Time, bucketMs int64) ([]dts017Snapshot, error) {
	if start.After(end) {
		return nil, errors.New("dts017 series agg: start after end")
	}
	series, err := s.tsMRangeAgg(start, end, "avg", bucketMs, "kind=dts017m")
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
