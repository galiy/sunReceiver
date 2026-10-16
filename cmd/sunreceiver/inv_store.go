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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// normalizeInverterSN приводит аппаратный серийник к стабильному виду, чтобы один
// аппарат не распадался на два ключа хранилища. Sofar иногда отдаёт серийник с
// хвостовым суффиксом версии/партии вида "...SN1234V480" — срезаем его.
func normalizeInverterSN(kind, sn string) string {
	if sn == "" || kind != sofarBrand.name {
		return sn
	}
	// Срезаем ВСЕ хвостовые группы "V<цифры>" (идемпотентно: повторный вызов
	// ничего не меняет), иначе ключ «уползал» бы по одной группе за опрос.
	for {
		i := strings.LastIndexByte(sn, 'V')
		if i <= 0 {
			break
		}
		tail := sn[i+1:]
		if tail == "" || strings.Trim(tail, "0123456789") != "" {
			break
		}
		sn = sn[:i]
	}
	return sn
}

// Обособленное хранилище сетевых инверторов, раздельно по марке (deye/sofar):
// контракты (наборы тегов) у марок разные, поэтому и ряды/таблицы раздельные.
//
// КЛЮЧ УСТРОЙСТВА — серийный номер инвертора (deviceSnapshot.InverterSN), а не IP:
// IP остаётся в снимке как информационное поле («откуда получено»). Поле HASH,
// идентичность точки в ZSET и PK строки PG — серийник.
//
// Общие ключи sunreceiver:current / :series:* и PG averages выведены из
// эксплуатации (см. BACKLOG): сюда переехали инверторы, всё остальное давно
// обособлено (DDS238/МАП/КЭС/DTS017M/CE308/BMS).
type invBrand struct {
	name      string // "deye"/"sofar"
	curKey    string // HASH текущих снимков марки (поле = серийник)
	seriesPfx string // префикс месячного ряда ZSET
	table     string // таблица PG 5-мин средних
}

var (
	deyeBrand  = invBrand{"deye", "sunreceiver:deye:current", "sunreceiver:deye:series:", "deye_averages"}
	sofarBrand = invBrand{"sofar", "sunreceiver:sofar:current", "sunreceiver:sofar:series:", "sofar_averages"}
)

// invBrandByKind возвращает марку хранилища по deviceSnapshot.Kind ("deye"/"sofar")
// или nil, если снимок не сетевой инвертор этих марок.
func invBrandByKind(kind string) *invBrand {
	switch kind {
	case deyeBrand.name:
		return &deyeBrand
	case sofarBrand.name:
		return &sofarBrand
	}
	return nil
}

func (b invBrand) seriesKey(ts time.Time) string {
	return b.seriesPfx + ts.Format("2006-01")
}

// snapKey — ключ устройства в обособленном хранилище. Для сетевых инверторов это
// серийник инвертора (InverterSN); для прочих (DDS238/МАП/КЭС) — их логический
// ключ, лежащий в поле IP (`dds238`/`map`/`mppt-<serial>`). Используется
// группировкой накопителя/дашборда (замена прежнего «IP как ключ»).
func snapKey(sn deviceSnapshot) string {
	if sn.InverterSN != "" {
		return sn.InverterSN
	}
	return sn.IP
}

// saveInvSnapshot пишет снимок инвертора в обособленные ключи марки одной
// транзакцией (current HASH + месячный ZSET), зеркально прочим модулям: хранится
// КАЖДОЕ показание, точка того же устройства на той же секунде перезаписывается.
func (s *redisStore) saveInvSnapshot(b invBrand, snap deviceSnapshot, ts time.Time) error {
	key := snap.InverterSN
	if key == "" {
		return fmt.Errorf("inv %s: пустой серийник инвертора (ip=%s)", b.name, snap.IP)
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("marshal inv %s %s: %w", b.name, key, err)
	}
	zkey := b.seriesKey(ts)
	score := strconv.FormatInt(ts.Unix(), 10)
	old, err := s.rdb.ZRangeByScore(s.ctx, zkey, &redis.ZRangeBy{Min: score, Max: score}).Result()
	if err != nil {
		return fmt.Errorf("inv %s dedup %s: %w", b.name, zkey, err)
	}
	var stale []any
	for _, m := range old {
		var q deviceSnapshot
		if json.Unmarshal([]byte(m), &q) != nil {
			continue
		}
		if q.InverterSN == key {
			stale = append(stale, m)
		}
	}
	pipe := s.rdb.TxPipeline()
	pipe.HSet(s.ctx, b.curKey, key, raw)
	if len(stale) > 0 {
		pipe.ZRem(s.ctx, zkey, stale...)
	}
	pipe.ZAdd(s.ctx, zkey, redis.Z{Score: float64(ts.Unix()), Member: string(raw)})
	pipe.Expire(s.ctx, zkey, 40*24*time.Hour)
	if _, err := pipe.Exec(s.ctx); err != nil {
		return fmt.Errorf("save inv %s %s: %w", b.name, key, err)
	}
	return nil
}

// invCurrent возвращает текущие снимки инверторов марки (HASH current).
func (s *redisStore) invCurrent(b invBrand) ([]deviceSnapshot, error) {
	m, err := s.rdb.HGetAll(s.ctx, b.curKey).Result()
	if err != nil {
		return nil, fmt.Errorf("HGETALL %s: %w", b.curKey, err)
	}
	out := make([]deviceSnapshot, 0, len(m))
	for _, v := range m {
		var snap deviceSnapshot
		if json.Unmarshal([]byte(v), &snap) != nil {
			continue
		}
		out = append(out, snap)
	}
	// Стабильный порядок по имени (дашборд сортирует и сам, но детерминизм полезен).
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// invSnapshotByIP возвращает текущий снимок инвертора марки с данным IP (поиск по
// HASH current; записей немного — марка). Нужен для донаса серийника оффлайн-
// инвертору: если опрос не отдал HW-серийник, берём его из прошлого снимка.
func (s *redisStore) invSnapshotByIP(b invBrand, ip string) (deviceSnapshot, error) {
	m, err := s.rdb.HGetAll(s.ctx, b.curKey).Result()
	if err != nil {
		return deviceSnapshot{}, err
	}
	for _, v := range m {
		var snap deviceSnapshot
		if json.Unmarshal([]byte(v), &snap) != nil {
			continue
		}
		if snap.IP == ip {
			return snap, nil
		}
	}
	return deviceSnapshot{}, redis.Nil
}

// queryInvSeries возвращает мгновенные снимки инверторов марки за [start, end] по
// месячным сегментам собственного ряда.
func (s *redisStore) queryInvSeries(b invBrand, start, end time.Time) ([]deviceSnapshot, error) {
	if start.After(end) {
		return nil, errors.New("inv series: start after end")
	}
	min := strconv.FormatInt(start.Unix(), 10)
	max := strconv.FormatInt(end.Unix(), 10)
	var all []deviceSnapshot
	var queryErr error
	eachMonth(start, end, func(y int, m time.Month) bool {
		key := b.seriesKey(time.Date(y, m, 1, 0, 0, 0, 0, time.Local))
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
			all = append(all, snap)
		}
		return true
	})
	if queryErr != nil {
		return nil, queryErr
	}
	return all, nil
}

// ensureInvAveragesSchema создаёт таблицы 5-минутных средних инверторов по марке.
// PK (serial, ts): ключ — серийник инвертора. Колонка ip — информационная («откуда
// получено»; нужна дашборду для привязки размещения placeByIP, как было в общей
// таблице), ключом не является.
func ensureInvAveragesSchema(s *pgStore) error {
	for _, b := range []invBrand{deyeBrand, sofarBrand} {
		if _, err := s.pool.Exec(s.ctx, fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS sunreceiver.%s (
	serial    text        NOT NULL,
	ip        text        NOT NULL DEFAULT '',
	name      text        NOT NULL,
	ts        timestamptz NOT NULL,
	device_sn text        NOT NULL DEFAULT '',
	values    jsonb       NOT NULL DEFAULT '{}'::jsonb,
	PRIMARY KEY (serial, ts)
);
CREATE INDEX IF NOT EXISTS %s_ts_idx ON sunreceiver.%s (ts);`, b.table, b.table, b.table)); err != nil {
			return fmt.Errorf("pg %s schema: %w", b.table, err)
		}
	}
	return nil
}

// insertInvAveragedExec пишет одну 5-минутную точку инвертора марки (в таблицу b.table).
// Идемпотентна по (serial, ts); поздняя запись переопределяет раннюю.
func insertInvAveragedExec(q pgExecer, ctx context.Context, b invBrand, serial, ip, name string, ts time.Time, deviceSN string, vc valuesContract) error {
	vals, err := json.Marshal(vc)
	if err != nil {
		return fmt.Errorf("pg marshal %s %s: %w", b.table, serial, err)
	}
	_, err = q.Exec(ctx, fmt.Sprintf(`
INSERT INTO sunreceiver.%s (serial, ip, name, ts, device_sn, values)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (serial, ts) DO UPDATE
  SET ip = EXCLUDED.ip,
      name = EXCLUDED.name,
      device_sn = EXCLUDED.device_sn,
      values = EXCLUDED.values`, b.table),
		serial, ip, name, ts.UTC(), deviceSN, vals)
	if err != nil {
		return fmt.Errorf("pg insert %s %s: %w", b.table, serial, err)
	}
	return nil
}

// invAverages возвращает 5-минутные точки инверторов марки за [start, end].
func (s *pgStore) invAverages(b invBrand, start, end time.Time) ([]deviceSnapshot, error) {
	rows, err := s.pool.Query(s.ctx, fmt.Sprintf(`
SELECT serial, ip, name, ts, device_sn, values
FROM sunreceiver.%s
WHERE ts >= $1 AND ts <= $2
ORDER BY ts ASC`, b.table), start.UTC(), end.UTC())
	if err != nil {
		return nil, fmt.Errorf("pg query %s: %w", b.table, err)
	}
	defer rows.Close()
	var out []deviceSnapshot
	for rows.Next() {
		var serial, ip, name, deviceSN string
		var ts time.Time
		var vals json.RawMessage
		if err := rows.Scan(&serial, &ip, &name, &ts, &deviceSN, &vals); err != nil {
			return nil, fmt.Errorf("pg scan %s: %w", b.table, err)
		}
		var vc valuesContract
		if len(vals) > 0 {
			if err := json.Unmarshal(vals, &vc); err != nil {
				continue
			}
		}
		out = append(out, deviceSnapshot{
			Name:       name,
			IP:         ip,
			InverterSN: serial,
			Timestamp:  ts.Format(time.RFC3339),
			DeviceSN:   deviceSN,
			Values:     vc,
		})
	}
	return out, rows.Err()
}
