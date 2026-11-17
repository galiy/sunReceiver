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

// Ключи Redis модуля AGM. Обособлены от инверторов/МАП/счётчиков: собственный
// HASH current и собственный ряд RedisTimeSeries.
const (
	// redisAGMCurrentKey — HASH текущего снимка сборки: поле = agmDeviceKey,
	// значение = JSON agmSnapshot. Перезаписывается только ПОЛНЫМ циклом.
	redisAGMCurrentKey = "sunreceiver:agm:current"
	// Серии — sunreceiver:ts:agm:agm:<tag> (kind=agm, device=agm).
)

// SaveAGMCurrent пишет текущий снимок в собственный HASH current.
func (s *redisStore) SaveAGMCurrent(snap agmSnapshot) error {
	b, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("marshal agm current: %w", err)
	}
	return s.rdb.HSet(s.ctx, redisAGMCurrentKey, agmDeviceKey, b).Err()
}

// SaveAGMSeries кладёт мгновенную точку каждого тега в ряд RedisTimeSeries
// (score = момент барьера цикла). Инвариант «только полный снимок»: если хотя бы
// для одного тега серия в негативном бэкоффе (TS недоступен), не пишем НИЧЕГО,
// чтобы в ряду не появился частичный снимок.
func (s *redisStore) SaveAGMSeries(snap agmSnapshot, ts time.Time) error {
	for tag := range snap.Values {
		if s.tsBackoffActive("agm", agmDeviceKey, tag) {
			return fmt.Errorf("save agm series: серия %q в бэкоффе — снимок не записан", tag)
		}
	}
	pipe := s.rdb.TxPipeline()
	for tag, v := range snap.Values {
		s.tsAdd(pipe, "agm", agmDeviceKey, snap.Name, "", "", "", tag, ts, v)
	}
	// Повторная проверка: tsAdd мог не создать серию (TS.CREATE упал) и уйти в
	// бэкофф уже во время сборки пайплайна — тогда не выполняем TS.ADD вовсе.
	for tag := range snap.Values {
		if s.tsBackoffActive("agm", agmDeviceKey, tag) {
			return fmt.Errorf("save agm series: серия %q в бэкоффе — снимок не записан", tag)
		}
	}
	if _, err := pipe.Exec(s.ctx); err != nil {
		// TxPipeline не откатывает успешные команды при ошибке отдельного
		// TS.ADD — best-effort удаляем точки снимка одним пайплайном (без
		// 18 последовательных ожиданий при недоступном Redis).
		del := s.rdb.Pipeline()
		for tag := range snap.Values {
			del.Do(s.ctx, "TS.DEL", tsKey("agm", agmDeviceKey, tag), ts.UnixMilli(), ts.UnixMilli())
		}
		_, _ = del.Exec(s.ctx)
		return fmt.Errorf("save agm series: %w", err)
	}
	return nil
}

// AGMCurrent возвращает текущий снимок сборки; nil, если ещё не было.
func (s *redisStore) AGMCurrent() (*agmSnapshot, error) {
	b, err := s.rdb.HGet(s.ctx, redisAGMCurrentKey, agmDeviceKey).Bytes()
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

// QueryAGMSeries возвращает мгновенные снимки за период [start, end] из серий
// RedisTimeSeries. Группировка — по паре (device, метка времени в миллисекундах):
// миллисекунды важны для правил «последнее значение окна» (несколько снимков в
// одну секунду должны различаться). Снимки упорядочены по возрастанию времени.
func (s *redisStore) QueryAGMSeries(start, end time.Time) ([]agmSnapshot, error) {
	if start.After(end) {
		return nil, errors.New("agm series: start after end")
	}
	series, err := s.tsMRange(start, end, "kind=agm")
	if err != nil {
		return nil, err
	}
	// Группировка — по стабильному device + ms (name — изменяемая метка:
	// переименование сборки не должно расщеплять снимок).
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
	return out, nil
}
