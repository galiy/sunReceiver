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
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Ключи Redis модуля КЭС (MPPT-контроллеры). Обособлены от инверторов/МАП/DDS238:
// собственные current-HASH и месячный ряд. Ключ устройства — devKey (`mppt-<serial>`).
// Чтение (Current/QuerySeries) собирает КЭС из этих ключей прозрачно, поэтому
// дашборд/анимация видят снимок как раньше.
const (
	redisKesCurrentKey   = "sunreceiver:kes:current"
	redisKesSeriesPrefix = "sunreceiver:kes:series:"
)

func kesSeriesKey(ts time.Time) string {
	return redisKesSeriesPrefix + ts.Format("2006-01")
}

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
	zkey := kesSeriesKey(ts)
	score := strconv.FormatInt(ts.Unix(), 10)
	old, err := s.rdb.ZRangeByScore(s.ctx, zkey, &redis.ZRangeBy{Min: score, Max: score}).Result()
	if err != nil {
		return fmt.Errorf("kes dedup %s: %w", zkey, err)
	}
	var stale []any
	for _, m := range old {
		var q deviceSnapshot
		if json.Unmarshal([]byte(m), &q) != nil {
			continue
		}
		// member без ip: сравниваем по name (уникально для КЭС).
		if q.Name == snap.Name {
			stale = append(stale, m)
		}
	}
	pipe := s.rdb.TxPipeline()
	pipe.HSet(s.ctx, redisKesCurrentKey, key, b)
	if len(stale) > 0 {
		pipe.ZRem(s.ctx, zkey, stale...)
	}
	pipe.ZAdd(s.ctx, zkey, redis.Z{Score: float64(ts.Unix()), Member: string(b)})
	pipe.Expire(s.ctx, zkey, 40*24*time.Hour)
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
	min := strconv.FormatInt(start.Unix(), 10)
	max := strconv.FormatInt(end.Unix(), 10)
	var all []deviceSnapshot
	var queryErr error
	eachMonth(start, end, func(y int, m time.Month) bool {
		key := kesSeriesKey(time.Date(y, m, 1, 0, 0, 0, 0, time.Local))
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
			snap.IP = kesKeyBySnap(snap)
			all = append(all, snap)
		}
		return true
	})
	if queryErr != nil {
		return nil, queryErr
	}
	return all, nil
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
