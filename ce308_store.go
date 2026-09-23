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
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// Ключи Redis модуля CE308. Обособлены от общего ряда инверторов: у CE308 нет
// универсального контракта значений, его данные не участвуют в суммах дашборда
// (общий kindMAP-подход для МАП/счётчика не подходит), поэтому текущие значения,
// временной ряд и снимок энергии храним в собственных ключах.
const (
	// redisCE308CurrentKey — HASH текущего снимка CE308: поле = имя устройства,
	// значение = JSON deviceSnapshot. Перезаписывается после каждого успешного опроса.
	redisCE308CurrentKey = "sunreceiver:ce308:current"
	// redisCE308SeriesPrefix — временной ряд мгновенных значений CE308.
	// Ключи вида sunreceiver:ce308:series:<YYYY-MM>, каждый — ZSET: score = Unix
	// (сек.), member = JSON deviceSnapshot. Пишется после каждого успешного опроса
	// (~1 точка за 2 с); усреднение до 1 записи за 10 с — в PG (ce308_accumulator).
	redisCE308SeriesPrefix = "sunreceiver:ce308:series:"
	// redisCE308EnergyKey — разовый снимок накопленной электроэнергии CE308
	// (JSON ce308EnergySnapshot). Один ключ, история по энергии не ведётся.
	redisCE308EnergyKey = "sunreceiver:ce308:energy"
)

// ce308SeriesKey возвращает ключ месячного сегмента ряда CE308 для ts.
func ce308SeriesKey(ts time.Time) string {
	return redisCE308SeriesPrefix + ts.Format("2006-01")
}

// SaveCE308Current пишет ТОЛЬКО текущее значение CE308 (HASH current[имя]),
// перезаписывая предыдущий снимок. Используется программным способом в каждом
// успешном опросе. Ограничение удержания — у current нет TTL (последнее состояние
// всегда хранится, как у инверторов).
func (s *redisStore) SaveCE308Current(snap ce308Snapshot) error {
	b, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("marshal ce308 current %s: %w", snap.Name, err)
	}
	return s.rdb.HSet(s.ctx, redisCE308CurrentKey, snap.Name, b).Err()
}

// SaveCE308History кладёт точку во временной ряд CE308 (score = Unix-секунды).
// Перед записью удаляет предыдущую версию того же устройства на том же score,
// чтобы в ZSET не было дублей в один момент времени (аналог SaveSnapshot).
func (s *redisStore) SaveCE308History(snap ce308Snapshot, ts time.Time) error {
	b, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("marshal ce308 history %s: %w", snap.Name, err)
	}
	key := ce308SeriesKey(ts)
	score := strconv.FormatInt(ts.Unix(), 10)
	old, err := s.rdb.ZRangeByScore(s.ctx, key, &redis.ZRangeBy{Min: score, Max: score}).Result()
	if err != nil {
		return fmt.Errorf("ce308 history dedup %s: %w", snap.Name, err)
	}
	var stale []any
	for _, m := range old {
		var q ce308Snapshot
		if json.Unmarshal([]byte(m), &q) != nil {
			continue
		}
		if q.Name == snap.Name {
			stale = append(stale, m)
		}
	}
	pipe := s.rdb.TxPipeline()
	if len(stale) > 0 {
		pipe.ZRem(s.ctx, key, stale...)
	}
	pipe.ZAdd(s.ctx, key, redis.Z{Score: float64(ts.Unix()), Member: string(b)})
	pipe.Expire(s.ctx, key, 40*24*time.Hour)
	_, err = pipe.Exec(s.ctx)
	if err != nil {
		return fmt.Errorf("save ce308 history %s: %w", snap.Name, err)
	}
	return nil
}

// SaveCE308Energy сохраняет разовый снимок энергии (отдельный ключ).
func (s *redisStore) SaveCE308Energy(snap *ce308EnergySnapshot) error {
	b, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("marshal ce308 energy: %w", err)
	}
	return s.rdb.Set(s.ctx, redisCE308EnergyKey, b, 0).Err()
}

// CE308Current возвращает все текущие снимки CE308 из HASH (поле = имя).
func (s *redisStore) CE308Current() (map[string]ce308Snapshot, error) {
	m, err := s.rdb.HGetAll(s.ctx, redisCE308CurrentKey).Result()
	if err != nil {
		return nil, fmt.Errorf("HGETALL %s: %w", redisCE308CurrentKey, err)
	}
	out := make(map[string]ce308Snapshot, len(m))
	for k, v := range m {
		var snap ce308Snapshot
		if err := json.Unmarshal([]byte(v), &snap); err != nil {
			continue
		}
		out[k] = snap
	}
	return out, nil
}

// CE308Energy возвращает последний снимок энергии CE308; пусто, если ещё не было.
func (s *redisStore) CE308Energy() (*ce308EnergySnapshot, error) {
	b, err := s.rdb.Get(s.ctx, redisCE308EnergyKey).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var snap ce308EnergySnapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

// QueryCE308Series возвращает мгновенные снимки CE308 за период [start, end]
// включительно из временного ряда (по месячным сегментам, объединяя по времени).
func (s *redisStore) QueryCE308Series(start, end time.Time) ([]ce308Snapshot, error) {
	if start.After(end) {
		return nil, fmt.Errorf("ce308 series: start after end")
	}
	startScore := strconv.FormatInt(start.Unix(), 10)
	endScore := strconv.FormatInt(end.Unix(), 10)
	var out []ce308Snapshot
	for key := ce308SeriesKey(start); !keyAfter(key, end); key = nextCe308SeriesKey(key) {
		vals, err := s.rdb.ZRangeByScore(s.ctx, key, &redis.ZRangeBy{Min: startScore, Max: endScore}).Result()
		if err != nil {
			return nil, fmt.Errorf("ce308 series %s: %w", key, err)
		}
		for _, m := range vals {
			var snap ce308Snapshot
			if err := json.Unmarshal([]byte(m), &snap); err != nil {
				continue
			}
			out = append(out, snap)
		}
	}
	return out, nil
}

// keyAfter — true, если ключ месяца key строго позже time end (для выхода из цикла).
func keyAfter(key string, end time.Time) bool {
	var y, m int
	_, err := fmt.Sscanf(key[ce308KeyPrefixLen:], "%04d-%02d", &y, &m)
	if err != nil {
		return true
	}
	return (y > end.Year()) || (y == end.Year() && m > int(end.Month()))
}

// nextCe308SeriesKey возвращает ключ следующего месяца после key.
// Формат ключа: sunreceiver:ce308:series:YYYY-MM.
func nextCe308SeriesKey(key string) string {
	s := key[ce308KeyPrefixLen:]
	var y, m int
	_, _ = fmt.Sscanf(s, "%04d-%02d", &y, &m)
	m++
	if m > 12 {
		m = 1
		y++
	}
	return redisCE308SeriesPrefix + fmt.Sprintf("%04d-%02d", y, m)
}

// ce308KeyPrefixLen — длина префикса месячного ключа (по YYYY-MM).
const ce308KeyPrefixLen = len(redisCE308SeriesPrefix)
