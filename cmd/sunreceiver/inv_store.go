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
}

var (
	deyeBrand  = invBrand{"deye", "sunreceiver:deye:current", "sunreceiver:deye:series:"}
	sofarBrand = invBrand{"sofar", "sunreceiver:sofar:current", "sunreceiver:sofar:series:"}
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

// spec — колоночная партиционированная таблица марки (deye/sofar).
func (b invBrand) spec() *avgSpec {
	return avgSpecByTable("sunreceiver." + b.name + "_averages_r")
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

// saveInvSnapshot пишет снимок инвертора: current HASH (последнее состояние) +
// точки в RedisTimeSeries (по тегам, серия на (марка,серийник,тег)).
func (s *redisStore) saveInvSnapshot(b invBrand, snap deviceSnapshot, ts time.Time) error {
	key := snap.InverterSN
	if key == "" {
		return fmt.Errorf("inv %s: пустой серийник инвертора (ip=%s)", b.name, snap.IP)
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("marshal inv %s %s: %w", b.name, key, err)
	}
	pipe := s.rdb.TxPipeline()
	pipe.HSet(s.ctx, b.curKey, key, raw)
	s.tsAddValueContract(pipe, b.name, key, snap.Name, snap.IP, snap.DeviceSN, ts, snap.Values)
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
	series, err := s.tsMRange(start, end, "kind="+b.name)
	if err != nil {
		return nil, err
	}
	return tsGroupSnapshots(series), nil
}

// (Схема таблиц 5-минутных средних инверторов — партиционированные колоночные
// `<марка>_averages_r`, см. avg_r.go/ensureAvgSchema. Старые jsonb-таблицы
// deye_averages_r/sofar_averages_r — история до этапа миграции, в коде не создаются.)

// insertInvAveragedExec пишет одну 5-минутную точку инвертора марки в
// партиционированную колоночную таблицу <марка>_averages_r. Идемпотентно по (serial, ts).
func insertInvAveragedExec(q pgExecer, ctx context.Context, b invBrand, serial, ip, name string, ts time.Time, deviceSN string, vc valuesContract) error {
	return insertAvgRow(q, ctx, b.spec(), serial, ts,
		map[string]string{"name": name, "device_sn": deviceSN, "ip": ip}, vc)
}

// invAverages возвращает 5-минутные точки инверторов марки за [start, end].
func (s *pgStore) invAverages(b invBrand, start, end time.Time) ([]deviceSnapshot, error) {
	return s.selectAvgRows(b.spec(), "", start, end)
}
