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
	"fmt"
	"log"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Хранилище временных рядов на RedisTimeSeries (RT). Одна серия — на пару
// (устройство, тег): ключ `sunreceiver:ts:<kind>:<device>:<tag>`, метки
// {kind, device, tag, name, ip}. Retention 2 суток (сырьевая гранулярность
// сохраняется). Выборка — TS.MRANGE/TS.RANGE с FILTER (устройство и/или теги,
// кросс-устройственные сводные). См. docs/research/redis-storage-optimization.md.
const (
	tsSeriesPrefix = "sunreceiver:ts:"
	tsRetentionMs  = 5184000000 // 60 суток (2 месяца) — глубина сырья в RedisTimeSeries
)

// tsPoint — точка серии: ts (Unix ms) и значение.
type tsPoint struct {
	TS int64
	V  float64
}

// tsSeries — серия RT: ключ, метки, точки.
type tsSeries struct {
	Key    string
	Labels map[string]string
	Points []tsPoint
}

// tsSlug делает компонент ключа безопасным (без ':', пробелов и скобок).
func tsSlug(s string) string {
	r := strings.NewReplacer(":", "_", " ", "_", "(", "_", ")", "_", "\n", "_")
	return r.Replace(s)
}

func tsKey(kind, device, tag string) string {
	return tsSeriesPrefix + tsSlug(kind) + ":" + tsSlug(device) + ":" + tsSlug(tag)
}

// tsLabelVal кодирует значение метки: RedisTimeSeries не принимает пробелы и
// служебные символы в LABELS (напр. имя «MAP (батарея/сеть)»). Percent-escape.
func tsLabelVal(s string) string { return url.QueryEscape(s) }

// tsLabelUnescape — обратная операция для значений меток.
func tsLabelUnescape(s string) string {
	if v, err := url.QueryUnescape(s); err == nil {
		return v
	}
	return s
}

// tsLabelArgs возвращает аргументы LABELS (пары ключ-значение) без пустых значений.
func tsLabelArgs(pairs [][2]string) []any {
	args := []any{"LABELS"}
	for _, kv := range pairs {
		if kv[1] == "" {
			continue
		}
		args = append(args, kv[0], tsLabelVal(kv[1]))
	}
	return args
}

// tsEnsure создаёт серию с метками (идемпотентно). Возвращает ключ. Пустые метки
// не добавляются. Метки НЕ «замораживаются»: при изменении (name/ip/device_sn)
// обновляются через TS.ALTER. При сбое TS.CREATE — негативный бэкофф (не спамим).
func (s *redisStore) tsEnsure(kind, device, name, ip, deviceSN, placement, tag string) string {
	key := tsKey(kind, device, tag)
	sig := strings.Join([]string{kind, device, name, ip, deviceSN, placement, tag}, "\x1f")
	pairs := [][2]string{{"kind", kind}, {"device", device}, {"tag", tag},
		{"name", name}, {"ip", ip}, {"device_sn", deviceSN}, {"placement", placement}}

	s.tsMu.Lock()
	cur, created := s.tsCreated[key]
	failUntil := s.tsFailUntil[key]
	s.tsMu.Unlock()

	if created {
		if cur == sig {
			return key
		}
		// Метки изменились — обновляем (перезапись указанных LABELS).
		args := append([]any{"TS.ALTER", key}, tsLabelArgs(pairs)...)
		if _, err := s.rdb.Do(s.ctx, args...).Result(); err == nil {
			s.tsMu.Lock()
			s.tsCreated[key] = sig
			s.tsMu.Unlock()
		}
		return key
	}
	if time.Now().Before(failUntil) {
		return key // недавно падало — не спамим TS.CREATE
	}
	args := append([]any{"TS.CREATE", key, "RETENTION", tsRetentionMs, "DUPLICATE_POLICY", "LAST"},
		tsLabelArgs(pairs)...)
	_, err := s.rdb.Do(s.ctx, args...).Result()
	exists := err != nil && strings.Contains(err.Error(), "already exists")
	if err == nil || exists {
		// На пути «уже существует» (напр., серия создана ранее/другим инстансом/
		// после рестарта) выравниваем метки через идемпотентный TS.ALTER.
		if exists {
			alt := append([]any{"TS.ALTER", key}, tsLabelArgs(pairs)...)
			if _, aerr := s.rdb.Do(s.ctx, alt...).Result(); aerr != nil {
				log.Printf("rt: TS.ALTER %s: %v", key, aerr)
			}
		}
		s.tsMu.Lock()
		if s.tsCreated == nil {
			s.tsCreated = map[string]string{}
		}
		s.tsCreated[key] = sig
		delete(s.tsFailUntil, key)
		s.tsMu.Unlock()
		return key
	}
	s.tsMu.Lock()
	if s.tsFailUntil == nil {
		s.tsFailUntil = map[string]time.Time{}
	}
	s.tsFailUntil[key] = time.Now().Add(30 * time.Second)
	s.tsMu.Unlock()
	log.Printf("rt: TS.CREATE %s: %v", key, err)
	return key
}

// tsAdd кладёт точку в серию (pipeline). Создаёт серию при необходимости.
func (s *redisStore) tsAdd(pipe redis.Pipeliner, kind, device, name, ip, deviceSN, placement, tag string, ts time.Time, v float64) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return
	}
	// При активном негативном бэкоффе (RT недоступен) не ставим TS.ADD в пайплайн —
	// иначе каждый снимок падает на несуществующей серии и плодит лог-спам.
	key := tsKey(kind, device, tag)
	s.tsMu.Lock()
	skip := time.Now().Before(s.tsFailUntil[key])
	s.tsMu.Unlock()
	if skip {
		return
	}
	key = s.tsEnsure(kind, device, name, ip, deviceSN, placement, tag)
	pipe.Do(s.ctx, "TS.ADD", key, ts.UnixMilli(), strconv.FormatFloat(v, 'g', -1, 64),
		"ON_DUPLICATE", "LAST")
}

// tsAddValueContract пишет все числовые теги контракта.
func (s *redisStore) tsAddValueContract(pipe redis.Pipeliner, kind, device, name, ip, deviceSN, placement string, ts time.Time, vc valuesContract) {
	for tag := range vc {
		if v, ok := vcFloat(vc, tag); ok {
			s.tsAdd(pipe, kind, device, name, ip, deviceSN, placement, tag, ts, v)
		}
	}
}

// tsMRange выполняет TS.MRANGE from..to WITHLABELS FILTER <filters...> и парсит ответ.
func (s *redisStore) tsMRange(from, to time.Time, filters ...string) ([]tsSeries, error) {
	args := []any{"TS.MRANGE", from.UnixMilli(), to.UnixMilli(), "WITHLABELS", "FILTER"}
	for _, f := range filters {
		args = append(args, f)
	}
	res, err := s.rdb.Do(s.ctx, args...).Result()
	if err != nil {
		return nil, fmt.Errorf("TS.MRANGE: %w", err)
	}
	return parseTSMRange(res), nil
}

// tsRange выполняет TS.RANGE по одному ключу.
func (s *redisStore) tsRange(key string, from, to time.Time) ([]tsPoint, error) {
	res, err := s.rdb.Do(s.ctx, "TS.RANGE", key, from.UnixMilli(), to.UnixMilli()).Result()
	if err != nil {
		return nil, fmt.Errorf("TS.RANGE %s: %w", key, err)
	}
	return parseTSRange(res), nil
}

// tsTagFilter формирует выражение FILTER по тегам (один — tag=x, много — tag=(x,y)).
func tsTagFilter(tags []string) string {
	if len(tags) == 1 {
		return "tag=" + tags[0]
	}
	return "tag=(" + strings.Join(tags, ",") + ")"
}

// tsMRangeAgg — TS.MRANGE с опциональной серверной агрегацией (AGGREGATION agg
// bucket, ALIGN 0) без компакт-серий и произвольными FILTER-ами (kind/device/tag).
// bucketMs=0 или agg="" — без агрегации (сырьё).
func (s *redisStore) tsMRangeAgg(from, to time.Time, agg string, bucketMs int64, filters ...string) ([]tsSeries, error) {
	args := []any{"TS.MRANGE", from.UnixMilli(), to.UnixMilli(), "WITHLABELS"}
	if bucketMs > 0 && agg != "" {
		args = append(args, "AGGREGATION", agg, bucketMs, "ALIGN", 0)
	}
	args = append(args, "FILTER")
	for _, f := range filters {
		args = append(args, f)
	}
	res, err := s.rdb.Do(s.ctx, args...).Result()
	if err != nil {
		return nil, fmt.Errorf("TS.MRANGE agg: %w", err)
	}
	return parseTSMRange(res), nil
}

// parseTSRange парсит ответ TS.RANGE/TS.MRANGE-внутренний: [[ts,val],...].
func parseTSRange(v any) []tsPoint {
	items, _ := v.([]any)
	out := make([]tsPoint, 0, len(items))
	for _, it := range items {
		pair, ok := it.([]any)
		if !ok || len(pair) < 2 {
			continue
		}
		out = append(out, tsPoint{TS: tsInt64(pair[0]), V: tsFloat(pair[1])})
	}
	return out
}

// parseTSMRange парсит ответ TS.MRANGE: [ [key, [labels...], [ [ts,val],... ] ], ... ].
func parseTSMRange(v any) []tsSeries {
	items, _ := v.([]any)
	out := make([]tsSeries, 0, len(items))
	for _, it := range items {
		row, ok := it.([]any)
		if !ok || len(row) < 3 {
			continue
		}
		ser := tsSeries{Key: tsString(row[0]), Labels: map[string]string{}}
		if lbls, ok := row[1].([]any); ok {
			for i := 0; i < len(lbls); i++ {
				if pair, ok := lbls[i].([]any); ok && len(pair) >= 2 {
					// RESP2 WITHLABELS: метки как массив пар [k,v].
					ser.Labels[tsString(pair[0])] = tsString(pair[1])
				} else if i+1 < len(lbls) {
					// плоский список k,v,k,v
					ser.Labels[tsString(lbls[i])] = tsString(lbls[i+1])
					i++
				}
			}
		}
		ser.Points = parseTSRange(row[2])
		out = append(out, ser)
	}
	return out
}

func tsString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []byte:
		return string(x)
	default:
		return fmt.Sprintf("%v", x)
	}
}

func tsInt64(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case float64:
		return int64(x)
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	case []byte:
		n, _ := strconv.ParseInt(string(x), 10, 64)
		return n
	}
	return 0
}

func tsFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case int64:
		return float64(x)
	case int:
		return float64(x)
	case string:
		f, _ := strconv.ParseFloat(x, 64)
		return f
	case []byte:
		f, _ := strconv.ParseFloat(string(x), 64)
		return f
	}
	return 0
}

// tsGroupSnapshots собирает из per-tag серий снимки deviceSnapshot по (device, ts):
// метки device/name/ip -> поля снимка, теги -> Values. Порядок — по ts.
func tsGroupSnapshots(series []tsSeries) []deviceSnapshot {
	type dk struct {
		device string
		ts     int64
	}
	byKey := map[dk]*deviceSnapshot{}
	for _, ser := range series {
		dev := tsLabelUnescape(ser.Labels["device"])
		name := tsLabelUnescape(ser.Labels["name"])
		ip := tsLabelUnescape(ser.Labels["ip"])
		kind := ser.Labels["kind"]
		deviceSN := tsLabelUnescape(ser.Labels["device_sn"])
		placement := tsLabelUnescape(ser.Labels["placement"])
		tag := tsLabelUnescape(ser.Labels["tag"])
		for _, p := range ser.Points {
			k := dk{dev, p.TS}
			sn := byKey[k]
			if sn == nil {
				sn = &deviceSnapshot{Name: name, IP: ip, Kind: kind, DeviceSN: deviceSN, Placement: placement, Timestamp: time.UnixMilli(p.TS).Format(time.RFC3339), Values: valuesContract{}}
				byKey[k] = sn
			}
			if sn.Name == "" {
				sn.Name = name
			}
			if sn.IP == "" {
				sn.IP = ip
			}
			if sn.Kind == "" {
				sn.Kind = kind
			}
			if sn.DeviceSN == "" {
				sn.DeviceSN = deviceSN
			}
			if sn.Placement == "" {
				sn.Placement = placement
			}
			if sn.InverterSN == "" && (kind == deyeBrand.name || kind == sofarBrand.name) {
				sn.InverterSN = dev
			}
			if sn.Values == nil {
				sn.Values = valuesContract{}
			}
			sn.Values[tag] = p.V
		}
	}
	out := make([]deviceSnapshot, 0, len(byKey))
	for _, sn := range byKey {
		out = append(out, *sn)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Timestamp != out[j].Timestamp {
			return out[i].Timestamp < out[j].Timestamp
		}
		return out[i].Name < out[j].Name
	})
	return out
}
