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

// TestMeterBackfillMaxDist — порог расстояния для добора границы: ближайшее
// показание, отстоящее от границы более чем на meterBackfillMaxDist (30 мин),
// НЕ подставляется (окно поиска ±6 ч остаётся поисковым, результат принимается
// только в пределах порога).
func TestMeterBackfillMaxDist(t *testing.T) {
	s := testStore(t)
	cleanTestKeys(t, s)
	cfg := &meterConfig{Name: "meter", IP: "10.0.0.99"}
	b := time.Now().Truncate(time.Minute)

	save := func(at time.Time, imp float64) {
		snap := deviceSnapshot{IP: cfg.IP, Name: "meter", Timestamp: at.Format(time.RFC3339)}
		snap.Values = valuesContract{"meter_import": imp, "meter_export": imp + 1}
		if err := s.SaveSnapshot(snap, at); err != nil {
			t.Fatalf("save snapshot @%v: %v", at, err)
		}
	}

	// Далеко (> порога), но в пределах поискового окна ±6 ч: границу не заполняем.
	far := b.Add(-2 * time.Hour)
	save(far, 100)
	if imp, exp, ok := nearestMeterReading(s, cfg, b); ok {
		t.Fatalf("ближайшей точки нет в пределах %v (имп=%v exp=%v, delta=%v) — граница не должна заполняться",
			meterBackfillMaxDist, imp, exp, b.Sub(far))
	}

	// В пределах порога (5 мин до границы): показатель дофиксируется.
	near := b.Add(-5 * time.Minute)
	save(near, 500)
	imp, exp, ok := nearestMeterReading(s, cfg, b)
	if !ok {
		t.Fatal("есть точка в пределах порога — должны дофиксировать границу")
	}
	if imp != 500 || exp != 501 {
		t.Fatalf("imp=%v exp=%v, want 500/501", imp, exp)
	}
}
