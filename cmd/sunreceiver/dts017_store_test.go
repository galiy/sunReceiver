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

import "testing"

// mergeDts017Backfill: накопительные теги берутся из last-серий, остальные — из avg.
func TestMergeDts017Backfill(t *testing.T) {
	avg := []tsSeries{
		{Key: "k-import", Labels: map[string]string{"tag": dts017Import}, Points: []tsPoint{{TS: 1, V: 100}, {TS: 2, V: 110}}},
		{Key: "k-power", Labels: map[string]string{"tag": dts017ActivePower}, Points: []tsPoint{{TS: 1, V: 5}, {TS: 2, V: 6}}},
	}
	last := []tsSeries{
		{Key: "k-import", Labels: map[string]string{"tag": dts017Import}, Points: []tsPoint{{TS: 1, V: 105}, {TS: 2, V: 115}}},
	}
	merged := mergeDts017Backfill(avg, last)
	if len(merged) != 2 {
		t.Fatalf("ожидали 2 серии, got %d", len(merged))
	}
	for _, s := range merged {
		switch s.Key {
		case "k-import":
			if s.Points[1].V != 115 {
				t.Fatalf("накопительный тег должен брать last: %v", s.Points)
			}
		case "k-power":
			if s.Points[1].V != 6 {
				t.Fatalf("обычный тег должен брать avg: %v", s.Points)
			}
		}
	}
}
