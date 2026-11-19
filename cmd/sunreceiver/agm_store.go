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
	"sort"
	"time"

	"github.com/redis/go-redis/v9"
)

// Ключи Redis модуля AGM: PZEM и BK300 разделены.
const (
	redisAGMPZEMCurrentKey = "sunreceiver:agm:pzem:current"
	redisAGMBKCurrentKey   = "sunreceiver:agm:bk300:current"
)

// SaveAGMPZEMCurrent пишет current PZEM.
func (s *redisStore) SaveAGMPZEMCurrent(snap agmSnapshot) error {
	b, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("marshal agm pzem current: %w", err)
	}
	return s.rdb.HSet(s.ctx, redisAGMPZEMCurrentKey, agmPZEMDeviceKey, b).Err()
}

// SaveAGMBKCurrent пишет current BK300.
func (s *redisStore) SaveAGMBKCurrent(snap agmSnapshot) error {
	b, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("marshal agm bk current: %w", err)
	}
	return s.rdb.HSet(s.ctx, redisAGMBKCurrentKey, agmBKDeviceKey, b).Err()
}

// saveAGMSeries кладёт полный снимок одного источника. Если хотя бы одна серия
// в бэкоффе — не пишем ничего (частичный снимок запрещён).
func (s *redisStore) saveAGMSeries(device string, snap agmSnapshot, ts time.Time) error {
	for tag := range snap.Values {
		if s.tsBackoffActive("agm", device, tag) {
			return fmt.Errorf("save agm series %s: серия %q в бэкоффе — снимок не записан", device, tag)
		}
	}
	pipe := s.rdb.TxPipeline()
	for tag, v := range snap.Values {
		s.tsAdd(pipe, "agm", device, snap.Name, "", "", "", tag, ts, v)
	}
	for tag := range snap.Values {
		if s.tsBackoffActive("agm", device, tag) {
			return fmt.Errorf("save agm series %s: серия %q в бэкоффе — снимок не записан", device, tag)
		}
	}
	if _, err := pipe.Exec(s.ctx); err != nil {
		del := s.rdb.Pipeline()
		for tag := range snap.Values {
			del.Do(s.ctx, "TS.DEL", tsKey("agm", device, tag), ts.UnixMilli(), ts.UnixMilli())
		}
		_, _ = del.Exec(s.ctx)
		return fmt.Errorf("save agm series %s: %w", device, err)
	}
	return nil
}

func (s *redisStore) SaveAGMPZEMSeries(snap agmSnapshot, ts time.Time) error {
	return s.saveAGMSeries(agmPZEMDeviceKey, snap, ts)
}
func (s *redisStore) SaveAGMBKSeries(snap agmSnapshot, ts time.Time) error {
	return s.saveAGMSeries(agmBKDeviceKey, snap, ts)
}

func agmCurrent(s *redisStore, key, device string) (*agmSnapshot, error) {
	b, err := s.rdb.HGet(s.ctx, key, device).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var snap agmSnapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

func (s *redisStore) AGMPZEMCurrent() (*agmSnapshot, error) {
	return agmCurrent(s, redisAGMPZEMCurrentKey, agmPZEMDeviceKey)
}
func (s *redisStore) AGMBKCurrent() (*agmSnapshot, error) {
	return agmCurrent(s, redisAGMBKCurrentKey, agmBKDeviceKey)
}

func (s *redisStore) queryAGMSeries(device string, start, end time.Time, agg string, bucketMs int64) ([]agmSnapshot, error) {
	if start.After(end) {
		return nil, errors.New("agm series: start after end")
	}
	filters := []string{"kind=agm", "device=" + device}
	var (
		series []tsSeries
		err    error
	)
	if agg == "" {
		series, err = s.tsMRange(start, end, filters...)
	} else {
		series, err = s.tsMRangeAgg(start, end, agg, bucketMs, filters...)
	}
	if err != nil {
		return nil, err
	}
	return assembleAGMSeries(series), nil
}

func (s *redisStore) QueryAGMPZEMSeries(start, end time.Time) ([]agmSnapshot, error) {
	return s.queryAGMSeries(agmPZEMDeviceKey, start, end, "", 0)
}
func (s *redisStore) QueryAGMPZEMSeriesAgg(start, end time.Time, bucketMs int64) ([]agmSnapshot, error) {
	return s.queryAGMSeries(agmPZEMDeviceKey, start, end, "avg", bucketMs)
}
func (s *redisStore) QueryAGMBKSeries(start, end time.Time) ([]agmSnapshot, error) {
	return s.queryAGMSeries(agmBKDeviceKey, start, end, "", 0)
}
func (s *redisStore) QueryAGMBKSeriesAgg(start, end time.Time, bucketMs int64) ([]agmSnapshot, error) {
	return s.queryAGMSeries(agmBKDeviceKey, start, end, "avg", bucketMs)
}

// assembleAGMSeries собирает снимки AGM из per-tag серий по (device, ms).
func assembleAGMSeries(series []tsSeries) []agmSnapshot {
	type snapKey struct {
		device string
		ms     int64
	}
	byKey := map[snapKey]*agmSnapshot{}
	for _, ser := range series {
		dev := tsLabelUnescape(ser.Labels["device"])
		name := tsLabelUnescape(ser.Labels["name"])
		tag := tsLabelUnescape(ser.Labels["tag"])
		for _, p := range ser.Points {
			k := snapKey{device: dev, ms: p.TS}
			sn := byKey[k]
			if sn == nil {
				sn = &agmSnapshot{
					Name:      name,
					Timestamp: time.UnixMilli(p.TS).Format(time.RFC3339Nano),
					Values:    map[string]float64{},
				}
				byKey[k] = sn
			}
			sn.Values[tag] = p.V
		}
	}
	type timedSnap struct {
		ms int64
		sn agmSnapshot
	}
	timed := make([]timedSnap, 0, len(byKey))
	for k, sn := range byKey {
		timed = append(timed, timedSnap{ms: k.ms, sn: *sn})
	}
	sort.Slice(timed, func(i, j int) bool { return timed[i].ms < timed[j].ms })
	out := make([]agmSnapshot, 0, len(timed))
	for _, t := range timed {
		out = append(out, t.sn)
	}
	return out
}
