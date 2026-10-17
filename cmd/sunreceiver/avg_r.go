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
	"fmt"
	"log"
	"strings"
	"time"
)

// Колоночные партиционированные таблицы 5-минутных средних: <orig>_averages_r.
// Партиционирование RANGE(ts) помесячно, PK (ключ, ts); метрики — double precision
// (NULL если тега нет), метаданные — text NOT NULL DEFAULT ”. Спецификации —
// единственный источник истины для DDL, писателя и читателя. BMS (bms26) —
// отдельная таблица с дочерними (cells/temps/alarm), агрегация по бинарному контракту.
type avgChild struct {
	suffix  string // суффикс дочерней таблицы: <parent>_<suffix>
	val     string // имя колонки значения
	valType string // double precision | integer
}

type avgSpec struct {
	table    string // sunreceiver.<orig>_averages_r
	key      string // "serial" | "name"
	meta     []string
	metrics  []string
	children []avgChild // только для bms26
}

var avgSpecs = []avgSpec{
	{"sunreceiver.deye_averages_r", "serial", []string{"name", "device_sn", "ip"}, []string{
		"ac_active_power", "ac_reactive_power", "energy_today", "energy_total",
		"grid_frequency", "l1_current", "l1_voltage", "l2_current", "l2_voltage",
		"l3_current", "l3_voltage", "pv1_current", "pv1_power", "pv1_voltage",
		"pv2_current", "pv2_power", "pv2_voltage"}, nil},
	{"sunreceiver.sofar_averages_r", "serial", []string{"name", "device_sn", "ip"}, []string{
		"ac_active_power", "ac_reactive_power", "energy_today", "energy_total",
		"grid_frequency", "l1_current", "l1_voltage", "l2_current", "l2_voltage",
		"l3_current", "l3_voltage", "pv1_current", "pv1_power", "pv1_voltage",
		"pv2_current", "pv2_power", "pv2_voltage"}, nil},
	{"sunreceiver.dds238_averages_r", "name", []string{"device_sn"}, []string{
		"meter_active_power", "meter_current", "meter_export", "meter_frequency",
		"meter_import", "meter_power_factor", "meter_reactive_power", "meter_total",
		"meter_voltage"}, nil},
	{"sunreceiver.map_averages_r", "name", []string{"device_sn"}, []string{
		"battery_current", "battery_power", "battery_voltage", "grid_frequency",
		"grid_power", "grid_voltage", "l1_voltage"}, nil},
	{"sunreceiver.kes_averages_r", "name", []string{"device_sn"}, []string{
		"ac_active_power", "energy_today", "l1_current", "l1_voltage",
		"pv1_current", "pv1_power", "pv1_voltage"}, nil},
	{"sunreceiver.dts017m_averages_r", "name", []string{"device_sn"}, []string{
		"dts017_active_power", "dts017_active_power_a", "dts017_active_power_b",
		"dts017_active_power_c", "dts017_apparent_power", "dts017_apparent_power_a",
		"dts017_apparent_power_b", "dts017_apparent_power_c", "dts017_current_a",
		"dts017_current_b", "dts017_current_c", "dts017_export", "dts017_export_reactive",
		"dts017_frequency_a", "dts017_frequency_b", "dts017_frequency_c", "dts017_import",
		"dts017_import_reactive", "dts017_power_factor", "dts017_power_factor_a",
		"dts017_power_factor_b", "dts017_power_factor_c", "dts017_reactive_power",
		"dts017_reactive_power_a", "dts017_reactive_power_b", "dts017_reactive_power_c",
		"dts017_reactive_total", "dts017_total", "dts017_voltage_a", "dts017_voltage_ab",
		"dts017_voltage_b", "dts017_voltage_bc", "dts017_voltage_c", "dts017_voltage_ca"}, nil},
	{"sunreceiver.bms26_averages_r", "name",
		[]string{"base", "serial", "bms_type", "device_name"}, []string{
			"soc", "soh", "mode", "flags", "cycles", "power_w", "rated_ah", "current_a",
			"avg_cell_v", "cell_count", "max_cell_v", "min_cell_v", "temp_count",
			"capacity_ah", "balance_mask", "cell_warn_mask", "cell_prot_mask",
			"temp_warn_mask", "temp_prot_mask", "max_cell_idx", "min_cell_idx",
			"remaining_ah", "port_voltage_v", "total_voltage_v"},
		[]avgChild{{"cells", "v", "double precision"}, {"temps", "v", "double precision"}, {"alarm", "v", "integer"}}},
}

const bms26AvgTable = "sunreceiver.bms26_averages_r"

// avgObsolete — колонки, снятые из spec (реконсиляция DROP COLUMN IF EXISTS).
// Оставлены как страховка для БД, где старые `*_r` не были пересозданы.
var avgObsolete = map[string][]string{
	bms26AvgTable: {"source", "samples", "framing"},
}

func avgSpecByTable(table string) *avgSpec {
	for i := range avgSpecs {
		if avgSpecs[i].table == table {
			return &avgSpecs[i]
		}
	}
	return nil
}

// ensureAvgFunction создаёт функцию помесячного авто-создания партиций (идемпотентно).
func ensureAvgFunction(pg *pgStore) error {
	_, err := pg.pool.Exec(pg.ctx, `
CREATE OR REPLACE FUNCTION sunreceiver.ensure_month_partition(p_parent regclass, p_ts timestamptz)
RETURNS void LANGUAGE plpgsql AS $$
DECLARE
  v_from timestamptz := date_trunc('month', p_ts);
  v_to   timestamptz := v_from + interval '1 month';
  v_name text := format('%s_p%s', p_parent::text, to_char(v_from, 'YYYYMM'));
BEGIN
  IF to_regclass(v_name) IS NULL THEN
    EXECUTE format('CREATE TABLE %s PARTITION OF %s FOR VALUES FROM (%L) TO (%L)',
                   v_name, p_parent, v_from, v_to);
  END IF;
END $$;`)
	return err
}

// ensureAvgSchema создаёт все *_r (партиционированные) и дочерние таблицы BMS,
// индексы. Идемпотентно (CREATE IF NOT EXISTS).
func (s *pgStore) ensureAvgSchema() error {
	if err := ensureAvgFunction(s); err != nil {
		return fmt.Errorf("ensure_month_partition: %w", err)
	}
	for i := range avgSpecs {
		sp := &avgSpecs[i]
		cols := []string{fmt.Sprintf("%s text NOT NULL", sp.key), "ts timestamptz NOT NULL"}
		for _, m := range sp.meta {
			cols = append(cols, fmt.Sprintf("%s text NOT NULL DEFAULT ''", m))
		}
		for _, m := range sp.metrics {
			cols = append(cols, fmt.Sprintf("%s double precision", m))
		}
		cols = append(cols, fmt.Sprintf("PRIMARY KEY (%s, ts)", sp.key))
		ddl := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (\n\t%s\n) PARTITION BY RANGE (ts);",
			sp.table, strings.Join(cols, ",\n\t"))
		if _, err := s.pool.Exec(s.ctx, ddl); err != nil {
			return fmt.Errorf("ddl %s: %w", sp.table, err)
		}
		if _, err := s.pool.Exec(s.ctx, fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s_ts_idx ON %s (ts);",
			baseName(sp.table), sp.table)); err != nil {
			return err
		}
		// Реконсиляция схемы существующей таблицы: ADD COLUMN IF NOT EXISTS для
		// всех meta/метрик (идемпотентно; покрывает дрейф после смены spec).
		for _, m := range sp.meta {
			if _, err := s.pool.Exec(s.ctx, fmt.Sprintf(
				"ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s text NOT NULL DEFAULT ''", sp.table, m)); err != nil {
				return err
			}
		}
		for _, m := range sp.metrics {
			if _, err := s.pool.Exec(s.ctx, fmt.Sprintf(
				"ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s double precision", sp.table, m)); err != nil {
				return err
			}
		}
		// Снятие устаревших колонок (BMS: source/samples/framing больше не в spec).
		for _, m := range avgObsolete[sp.table] {
			if _, err := s.pool.Exec(s.ctx, fmt.Sprintf(
				"ALTER TABLE %s DROP COLUMN IF EXISTS %s", sp.table, m)); err != nil {
				return err
			}
		}
		for _, ch := range sp.children {
			child := sp.table + "_" + ch.suffix
			cddl := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
	name text NOT NULL, ts timestamptz NOT NULL, idx integer NOT NULL, %s %s,
	PRIMARY KEY (name, ts, idx)) PARTITION BY RANGE (ts);`, child, ch.val, ch.valType)
			if _, err := s.pool.Exec(s.ctx, cddl); err != nil {
				return err
			}
			if _, err := s.pool.Exec(s.ctx, fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s_ts_idx ON %s (ts);",
				baseName(child), child)); err != nil {
				return err
			}
		}
	}
	return nil
}

// ensureAvgPartitions создаёт партиции на месяцы −1…+2 для всех *_r (parent +
// дочерние). Вызывается при старте и каждые 12 ч (runAvgPartitionMaintenance).
func (s *pgStore) ensureAvgPartitions() error {
	var tables []string
	for i := range avgSpecs {
		sp := &avgSpecs[i]
		tables = append(tables, sp.table)
		for _, ch := range sp.children {
			tables = append(tables, sp.table+"_"+ch.suffix)
		}
	}
	for _, t := range tables {
		// Прошлый месяц (окно backfill/restore recentCutoff заходит в него),
		// текущий и 2 вперёд (сервис может работать без перезапуска месяцами —
		// горизонт поддерживается и в рантайме, см. runAvgPartitionMaintenance).
		for off := -1; off <= 2; off++ {
			if _, err := s.pool.Exec(s.ctx,
				"SELECT sunreceiver.ensure_month_partition($1::regclass, date_trunc('month', now()) + ($2 || ' month')::interval)",
				t, fmt.Sprintf("%d", off)); err != nil {
				return fmt.Errorf("partition %s %+d: %w", t, off, err)
			}
		}
	}
	return nil
}

func baseName(table string) string {
	if i := strings.LastIndexByte(table, '.'); i >= 0 {
		return table[i+1:]
	}
	return table
}

// runAvgPartitionMaintenance держит горизонт помесячных партиций *_r: создаёт
// месяцы −1…+2 при старте и повторяет каждые 12 часов, чтобы границы
// месяца не оставались без партиции (без cron/pg_partman).
func runAvgPartitionMaintenance(pg *pgStore, ctx context.Context) {
	if pg == nil {
		return
	}
	ensure := func() {
		if err := pg.ensureAvgPartitions(); err != nil {
			log.Printf("avg partitions: %v", err)
		}
	}
	ensure()
	t := time.NewTicker(12 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			ensure()
		case <-ctx.Done():
			return
		}
	}
}

// insertAvgRow пишет одну колоночную строку среднего в таблицу sp.table (без детей;
// для bms26 parent-часть пишется здесь, дети — отдельно). Идемпотентно по (key, ts).
func insertAvgRow(q pgExecer, ctx context.Context, sp *avgSpec, key string, ts time.Time, meta map[string]string, vc valuesContract) error {
	cols := []string{sp.key, "ts"}
	args := []any{key, ts.UTC()}
	for _, m := range sp.meta {
		cols = append(cols, m)
		args = append(args, meta[m])
	}
	for _, c := range sp.metrics {
		cols = append(cols, c)
		if f, ok := vcFloat(vc, c); ok {
			args = append(args, f)
		} else {
			args = append(args, nil)
		}
	}
	ph := make([]string, len(cols))
	for i := range cols {
		ph[i] = fmt.Sprintf("$%d", i+1)
	}
	upd := make([]string, 0, len(sp.meta)+len(sp.metrics))
	for _, m := range sp.meta {
		// Пустое значение не затирает непустое метаданное (напр. при миграции).
		upd = append(upd, fmt.Sprintf("%s = COALESCE(NULLIF(EXCLUDED.%s, ''), t.%s)", m, m, m))
	}
	for _, m := range sp.metrics {
		// COALESCE: повторная запись без части тегов не должна обнулять ранее
		// записанные значения (важно при перезаписи/миграции).
		upd = append(upd, fmt.Sprintf("%s = COALESCE(EXCLUDED.%s, t.%s)", m, m, m))
	}
	sql := fmt.Sprintf("INSERT INTO %s AS t (%s) VALUES (%s) ON CONFLICT (%s, ts) DO UPDATE SET %s",
		sp.table, strings.Join(cols, ", "), strings.Join(ph, ", "), sp.key, strings.Join(upd, ", "))
	if _, err := q.Exec(ctx, sql, args...); err != nil {
		return fmt.Errorf("pg insert %s %s: %w", sp.table, key, err)
	}
	return nil
}

// insertAvgRowNoClobber — как insertAvgRow, но ON CONFLICT DO NOTHING: для миграции
// истории (не затирает уже записанные новые точки).
func insertAvgRowNoClobber(q pgExecer, ctx context.Context, sp *avgSpec, key string, ts time.Time, meta map[string]string, vc valuesContract) error {
	cols := []string{sp.key, "ts"}
	args := []any{key, ts.UTC()}
	for _, m := range sp.meta {
		cols = append(cols, m)
		args = append(args, meta[m])
	}
	for _, c := range sp.metrics {
		cols = append(cols, c)
		if f, ok := vcFloat(vc, c); ok {
			args = append(args, f)
		} else {
			args = append(args, nil)
		}
	}
	ph := make([]string, len(cols))
	for i := range cols {
		ph[i] = fmt.Sprintf("$%d", i+1)
	}
	sql := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) ON CONFLICT (%s, ts) DO NOTHING",
		sp.table, strings.Join(cols, ", "), strings.Join(ph, ", "), sp.key)
	if _, err := q.Exec(ctx, sql, args...); err != nil {
		return fmt.Errorf("pg insert %s %s: %w", sp.table, key, err)
	}
	return nil
}

// selectAvgRows читает колоночные строки sp.table за [start,end] в deviceSnapshot
// (Values — только непустые метрики). keyFilter != "" — только это устройство
// (WHERE по ключу, вместо фильтрации в Go).
func (s *pgStore) selectAvgRows(sp *avgSpec, keyFilter string, start, end time.Time) ([]deviceSnapshot, error) {
	cols := append([]string{sp.key, "ts"}, sp.meta...)
	cols = append(cols, sp.metrics...)
	q := fmt.Sprintf("SELECT %s FROM %s WHERE ts >= $1 AND ts <= $2",
		strings.Join(cols, ", "), sp.table)
	args := []any{start.UTC(), end.UTC()}
	if keyFilter != "" {
		q += fmt.Sprintf(" AND %s = $3", sp.key)
		args = append(args, keyFilter)
	}
	q += " ORDER BY ts ASC"
	rows, err := s.pool.Query(s.ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("pg query %s: %w", sp.table, err)
	}
	defer rows.Close()
	var out []deviceSnapshot
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, fmt.Errorf("pg scan %s: %w", sp.table, err)
		}
		key := asString(vals[0])
		ts, _ := vals[1].(time.Time)
		sn := deviceSnapshot{Timestamp: ts.Format(time.RFC3339)}
		if sp.key == "serial" {
			sn.InverterSN = key
		} else {
			sn.Name = key // таблицы с ключом-именем: key = name устройства
		}
		mi := 2
		for _, m := range sp.meta {
			v := asString(vals[mi])
			mi++
			switch m {
			case "name":
				sn.Name = v
			case "device_sn":
				sn.DeviceSN = v
			case "ip":
				sn.IP = v
			}
		}
		vc := valuesContract{}
		for _, m := range sp.metrics {
			if f, ok := asFloatPtr(vals[mi]); ok {
				vc[m] = f
			}
			mi++
		}
		sn.Values = vc
		out = append(out, sn)
	}
	return out, rows.Err()
}

func asString(v any) string {
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

func asFloatPtr(v any) (float64, bool) {
	switch x := v.(type) {
	case nil:
		return 0, false
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case []byte:
		var f float64
		if _, err := fmt.Sscanf(string(x), "%g", &f); err == nil {
			return f, true
		}
	}
	return 0, false
}

// vcFloat извлекает числовое значение из универсального контракта (valuesContract
// = map[string]any): числа приходят как float64/float32/int; прочее (строки,
// массивы BMS) — не метрика.
func vcFloat(vc valuesContract, key string) (float64, bool) {
	switch x := vc[key].(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint64:
		return float64(x), true
	}
	return 0, false
}

// vcToFloatMap конвертирует контракт в плоскую map[string]float64 (для модулей,
// у которых Values — именно такая map, напр. DTS017M).
func vcToFloatMap(vc valuesContract) map[string]float64 {
	out := make(map[string]float64, len(vc))
	for k := range vc {
		if f, ok := vcFloat(vc, k); ok {
			out[k] = f
		}
	}
	return out
}
