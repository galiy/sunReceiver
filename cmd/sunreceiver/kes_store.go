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
	"strings"
	"time"
)

// Ключи Redis модуля КЭС (MPPT-контроллеры). Обособлены от инверторов/МАП/DDS238:
// собственные current-HASH и ряд RedisTimeSeries. Ключ устройства — devKey (`mppt-<serial>`).
// Читаются отдельными методами (KesCurrent/QueryKesSeries/KesAverages); в общие
// читалки (снят) НЕ подмешиваются.
const (
	redisKesCurrentKey = "sunreceiver:kes:current"
)

// isKesKey — ключ устройства КЭС (`mppt-<serial>`); отделяет их от инверторов/МАП.
func isKesKey(ip string) bool { return strings.HasPrefix(ip, "mppt-") }

// saveKesSnapshot пишет снимок КЭС в обособленные ключи (ip не персистим — ключ
// хранится в поле HASH/после чтения восстанавливается).
func (s *redisStore) saveKesSnapshot(snap deviceSnapshot, ts time.Time) error {
	key := snap.IP
	snap.IP = ""
	b, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("marshal kes snapshot: %w", err)
	}
	pipe := s.rdb.TxPipeline()
	pipe.HSet(s.ctx, redisKesCurrentKey, key, b)
	s.tsAddValueContract(pipe, "kes", key, snap.Name, "", snap.DeviceSN, ts, snap.Values)
	if _, err := pipe.Exec(s.ctx); err != nil {
		return fmt.Errorf("save kes %s: %w", key, err)
	}
	return nil
}

// KesCurrent возвращает текущие снимки КЭС (ip восстанавливается в ключ).
func (s *redisStore) KesCurrent() ([]deviceSnapshot, error) {
	m, err := s.rdb.HGetAll(s.ctx, redisKesCurrentKey).Result()
	if err != nil {
		return nil, fmt.Errorf("HGETALL %s: %w", redisKesCurrentKey, err)
	}
	out := make([]deviceSnapshot, 0, len(m))
	for key, v := range m {
		var snap deviceSnapshot
		if json.Unmarshal([]byte(v), &snap) != nil {
			continue
		}
		snap.IP = key
		out = append(out, snap)
	}
	return out, nil
}

// QueryKesSeries возвращает снимки КЭС за [start, end] (ip восстанавливается в ключ).
func (s *redisStore) QueryKesSeries(start, end time.Time) ([]deviceSnapshot, error) {
	if start.After(end) {
		return nil, errors.New("kes series: start after end")
	}
	series, err := s.tsMRange(start, end, "kind=kes")
	if err != nil {
		return nil, err
	}
	out := tsGroupSnapshots(series)
	for i := range out {
		out[i].IP = kesKeyByName(out[i].Name)
	}
	return out, nil
}

// kesKeyBySnap — ключ КЭС (вида `mppt-<serial>`) по снимку: по отображаемому
// имени "MPPT-<serial>" (в этом виде КЭС пишутся пулером).
func kesKeyBySnap(snap deviceSnapshot) string {
	return kesKeyByName(snap.Name)
}

// kesKeyByName — ключ КЭС по отображаемому имени "MPPT-<serial>".
func kesKeyByName(name string) string {
	return "mppt-" + strings.TrimPrefix(name, "MPPT-")
}
