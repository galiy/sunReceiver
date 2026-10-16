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
	"strings"
	"time"
)

// selectBms26Range читает bms26_averages_r (+ дочерние cells/temps/alarm) за
// [start,end] и пересобирает bms26Snapshot с массивами контракта.
func (s *pgStore) selectBms26Range(keyFilter string, start, end time.Time) ([]bms26Snapshot, error) {
	sp := avgSpecByTable(bms26AvgTable)
	cols := append([]string{"name", "ts"}, append(append([]string{}, sp.meta...), sp.metrics...)...)
	colPos := map[string]int{}
	for i, c := range cols {
		colPos[c] = i
	}
	q := "SELECT " + strings.Join(cols, ", ") + " FROM " + sp.table + " WHERE ts >= $1 AND ts <= $2"
	args := []any{start.UTC(), end.UTC()}
	if keyFilter != "" {
		q += " AND name = $3"
		args = append(args, keyFilter)
	}
	q += " ORDER BY ts ASC"
	rows, err := s.pool.Query(s.ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("pg query %s: %w", sp.table, err)
	}
	type key struct {
		name string
		ts   time.Time
	}
	pos := map[key]int{}
	var out []bms26Snapshot
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			rows.Close()
			return nil, fmt.Errorf("pg scan %s: %w", sp.table, err)
		}
		name := asString(vals[colPos["name"]])
		ts, _ := vals[colPos["ts"]].(time.Time)
		sn := bms26Snapshot{Serial: name, Timestamp: ts.Format(time.RFC3339)}
		// Доступ по ИМЕНИ колонки (не позиционно) — устойчиво к правкам avgSpecs.
		if v := asString(vals[colPos["base"]]); v != "" {
			sn.Base = v
		}
		if v := asString(vals[colPos["serial"]]); v != "" {
			sn.Serial = v
		}
		if v := asString(vals[colPos["bms_type"]]); v != "" {
			sn.BmsType = v
		}
		if v := asString(vals[colPos["device_name"]]); v != "" {
			sn.DeviceName = v
		}
		f := func(n string) float64 { x, _ := asFloatPtr(vals[colPos[n]]); return x }
		sn.Soc = f("soc")
		sn.Soh = f("soh")
		sn.Mode = int(f("mode"))
		sn.Flags = int(f("flags"))
		sn.Cycles = int(f("cycles"))
		sn.PowerW = f("power_w")
		sn.RatedAh = f("rated_ah")
		sn.CurrentA = f("current_a")
		sn.AvgCellV = f("avg_cell_v")
		sn.CellCount = int(f("cell_count"))
		sn.MaxCellV = f("max_cell_v")
		sn.MinCellV = f("min_cell_v")
		sn.TempCount = int(f("temp_count"))
		sn.CapacityAh = f("capacity_ah")
		sn.BalanceMask = uint32(f("balance_mask"))
		sn.CellWarnMask = uint32(f("cell_warn_mask"))
		sn.CellProtMask = uint32(f("cell_prot_mask"))
		sn.TempWarnMask = uint32(f("temp_warn_mask"))
		sn.TempProtMask = uint32(f("temp_prot_mask"))
		sn.MaxCellIdx = int(f("max_cell_idx"))
		sn.MinCellIdx = int(f("min_cell_idx"))
		sn.RemainingAh = f("remaining_ah")
		sn.PortV = f("port_voltage_v")
		sn.TotalV = f("total_voltage_v")
		pos[key{name, ts}] = len(out)
		out = append(out, sn)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	cells := map[key][]float64{}
	temps := map[key][]float64{}
	alarm := map[key][]int{}
	loadFloat := func(table string, dst map[key][]float64) error {
		q := fmt.Sprintf("SELECT name, ts, idx, v FROM %s WHERE ts >= $1 AND ts <= $2", table)
		args := []any{start.UTC(), end.UTC()}
		if keyFilter != "" {
			q += " AND name = $3"
			args = append(args, keyFilter)
		}
		q += " ORDER BY name, ts, idx"
		cr, err := s.pool.Query(s.ctx, q, args...)
		if err != nil {
			return err
		}
		defer cr.Close()
		for cr.Next() {
			var nm string
			var ts time.Time
			var i int
			var v float64
			if err := cr.Scan(&nm, &ts, &i, &v); err != nil {
				return err
			}
			k := key{nm, ts}
			a := dst[k]
			if i >= len(a) {
				na := make([]float64, i+1)
				copy(na, a)
				a = na
			}
			a[i] = v
			dst[k] = a
		}
		return cr.Err()
	}
	loadInt := func(table string, dst map[key][]int) error {
		q := fmt.Sprintf("SELECT name, ts, idx, v FROM %s WHERE ts >= $1 AND ts <= $2", table)
		args := []any{start.UTC(), end.UTC()}
		if keyFilter != "" {
			q += " AND name = $3"
			args = append(args, keyFilter)
		}
		q += " ORDER BY name, ts, idx"
		cr, err := s.pool.Query(s.ctx, q, args...)
		if err != nil {
			return err
		}
		defer cr.Close()
		for cr.Next() {
			var nm string
			var ts time.Time
			var i, v int
			if err := cr.Scan(&nm, &ts, &i, &v); err != nil {
				return err
			}
			k := key{nm, ts}
			a := dst[k]
			if i >= len(a) {
				na := make([]int, i+1)
				copy(na, a)
				a = na
			}
			a[i] = v
			dst[k] = a
		}
		return cr.Err()
	}
	if err := loadFloat(sp.table+"_cells", cells); err != nil {
		return nil, err
	}
	if err := loadFloat(sp.table+"_temps", temps); err != nil {
		return nil, err
	}
	if err := loadInt(sp.table+"_alarm", alarm); err != nil {
		return nil, err
	}
	for k, i := range pos {
		out[i].CellsV = cells[k]
		out[i].TemperaturesC = temps[k]
		out[i].AlarmBytes = alarm[k]
	}
	return out, nil
}
