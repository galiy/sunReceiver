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
	"testing"
	"time"
)

// snapPow строит снимок инвертора с ac_active_power.
func snapPow(ip string, t time.Time, v float64) deviceSnapshot {
	return deviceSnapshot{
		Timestamp: t.Format(time.RFC3339),
		Name:      ip,
		IP:        ip,
		Values:    valuesContract{"ac_active_power": v},
	}
}

func TestSumActiveStaleness(t *testing.T) {
	base := time.Date(2026, 9, 8, 10, 0, 0, 0, time.Local)

	// a — инвертор, сообщающий каждые 5 минут на всём протяжении (мощность 100).
	// b — инвертор, замолчавший сразу после старта (единственная точка, мощность 200).
	var snaps []deviceSnapshot
	for i := 0; i <= 5; i++ {
		snaps = append(snaps, snapPow("a", base.Add(time.Duration(i)*5*time.Minute), 100))
	}
	snaps = append(snaps, snapPow("b", base, 200))

	out := sumActive(snaps)

	// Первая точка (на ней сообщили и a, и b): 100+200=300.
	if len(out) == 0 || out[0].V != 300 {
		t.Fatalf("first point: want 300, got %v", vals(out))
	}
	// Пока b не «протух» (возраст ≤ 20 мин) — сумма 300.
	distinct := distinctVals(out)
	if distinct[0] != 300 {
		t.Fatalf("early points: want 300, got %v", distinct)
	}
	// В точке 10:25 возраст точки b = 25 мин > 20 — b исключается, остаётся 100.
	last := out[len(out)-1]
	if last.V != 100 {
		t.Fatalf("last point: want 100 (stale b excluded), got %v", last.V)
	}
}

func distinctVals(out []seriesPoint) []float64 {
	var vs []float64
	seen := map[float64]bool{}
	for _, p := range out {
		if !seen[p.V] {
			seen[p.V] = true
			vs = append(vs, p.V)
		}
	}
	return vs
}

func vals(out []seriesPoint) []float64 {
	var vs []float64
	for _, pp := range out {
		vs = append(vs, pp.V)
	}
	return vs
}

// TestPlacementOrder проверяет порядок размещений для рамки «Мощности инверторов»:
// только сетевые инверторы (Deye/Sofar), по первому появлению в конфиге, пустое
// размещение приводится к «Дом», а MPPT (КЭС) и МАП в список не попадают.
func TestPlacementOrder(t *testing.T) {
	targets := []invTarget{
		{Name: "Sofar A", Kind: kindSofar, Placement: "Гараж"},
		{Name: "MAP", Kind: kindMAP},
		{Name: "MPPT 0", Kind: kindMPPT, IP: "host#mppt0"},
		{Name: "Deye B", Kind: kindDeyeString, Placement: "Гараж"},
		{Name: "Sofar C", Kind: kindSofar, Placement: ""}, // пусто → «Дом»
	}
	got := placementOrder(targets)
	want := []string{"Гараж", "Дом"}
	if len(got) != len(want) {
		t.Fatalf("placementOrder: want %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("placementOrder[%d]: want %q, got %q (full %v)", i, want[i], got[i], got)
		}
	}
}

// TestPlacementOrderDefaultHome — все инверторы без размещения сводятся к единственному
// размещению «Дом».
func TestPlacementOrderDefaultHome(t *testing.T) {
	targets := []invTarget{
		{Name: "A", Kind: kindDeyeString},
		{Name: "B", Kind: kindSofar},
	}
	got := placementOrder(targets)
	if len(got) != 1 || got[0] != "Дом" {
		t.Fatalf("placementOrder: want [Дом], got %v", got)
	}
}
