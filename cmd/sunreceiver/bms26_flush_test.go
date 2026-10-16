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
	"math"
	"testing"
	"time"
)

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// Регресс (прод-баг 2026-10-16): бакет, закрытый на границе окна в add(), не должен
// теряться — раньше add() заменял бакет, а closed() его уже не находил.
func TestBms26AccumulatorFlushOnAdd(t *testing.T) {
	acc := newBms26Accumulator()
	base := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	s1 := bms26Snapshot{Serial: "S", CellsV: []float64{1, 2}, TemperaturesC: []float64{10, 20}, Soc: 50}
	s2 := bms26Snapshot{Serial: "S", CellsV: []float64{3, 5}, TemperaturesC: []float64{14, 24}, Soc: 60}
	acc.add(s1, base)
	acc.add(s2, base.Add(time.Minute)) // то же окно 12:00
	// Переход в следующее окно: старый бакет должен уйти в pending (не потерян).
	acc.add(bms26Snapshot{Serial: "S", CellsV: []float64{9}}, base.Add(5*time.Minute))
	got := acc.closed(base.Add(5 * time.Minute))
	if len(got) != 1 {
		t.Fatalf("закрытых бакетов=%d, want 1 (бакет 12:00 не потерян): %+v", len(got), got)
	}
	sn := got[0]
	if sn.Timestamp != base.Format(time.RFC3339) {
		t.Errorf("ts=%q, want %q", sn.Timestamp, base.Format(time.RFC3339))
	}
	if len(sn.CellsV) != 2 || !approx(sn.CellsV[0], 2) || !approx(sn.CellsV[1], 3.5) {
		t.Errorf("CellsV=%v, want [2 3.5]", sn.CellsV)
	}
	if !approx(sn.Soc, 55) {
		t.Errorf("Soc=%v, want 55", sn.Soc)
	}
}

// closed() возвращает и «старый» бакет, если add() больше не вызывался (сравнение по окну).
func TestBms26AccumulatorClosedOlder(t *testing.T) {
	acc := newBms26Accumulator()
	base := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	acc.add(bms26Snapshot{Serial: "S", Soc: 42}, base)
	got := acc.closed(base.Add(5 * time.Minute))
	if len(got) != 1 || !approx(got[0].Soc, 42) {
		t.Fatalf("closed=%+v, want один бакет Soc=42", got)
	}
	// Повторный вызов — пусто (бакет уже забран).
	if again := acc.closed(base.Add(10 * time.Minute)); len(again) != 0 {
		t.Fatalf("повторно closed=%d, want 0", len(again))
	}
}

// Per-index счётчики: при смене длины массива внутри окна «новые» индексы не
// размываются общим n.
func TestBms26AccumulatorPerIndexCount(t *testing.T) {
	acc := newBms26Accumulator()
	base := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	acc.add(bms26Snapshot{Serial: "S", CellsV: []float64{1, 1}}, base)
	acc.add(bms26Snapshot{Serial: "S", CellsV: []float64{3, 3, 8}}, base.Add(time.Minute))
	got := acc.closed(base.Add(5 * time.Minute))
	if len(got) != 1 {
		t.Fatalf("buckets=%d, want 1", len(got))
	}
	c := got[0].CellsV
	if len(c) != 3 {
		t.Fatalf("CellsV len=%d, want 3", len(c))
	}
	if !approx(c[0], 2) || !approx(c[1], 2) || !approx(c[2], 8) {
		t.Errorf("CellsV=%v, want [2 2 8] (индекс 2 — по своему счётчику)", c)
	}
}
