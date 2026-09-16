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
	"testing"
	"time"
)

// TestBmsAveragedSamplesJSON: поле samples всегда сериализуется в jsonb values и
// читается обратно как int. На это поле опирается sample-count guard в
// InsertBMSAveraged (pg_store.go) — сравнение (values->>'samples')::int: если бы
// samples терялось при marshal, guard не смог бы отличить более полную запись.
func TestBmsAveragedSamplesJSON(t *testing.T) {
	avg := bmsAveraged{CurrentA: 1.5, PowerW: 51.2, Soc: 55, CellCount: 16, Samples: 120}
	b, err := json.Marshal(avg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// SQL-гвард читает values->>'samples' как jsonb-число: проверяем, что поле
	// присутствует и парсится в int.
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}
	raw, ok := m["samples"]
	if !ok {
		t.Fatalf("samples не сериализуется в values: %s", b)
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil || n != 120 {
		t.Fatalf("samples=%s → %d, want 120", raw, n)
	}
	var back bmsAveraged
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Samples != 120 || back.CurrentA != 1.5 {
		t.Fatalf("roundtrip: %+v", back)
	}
}

// TestBmsAccumulatorNameCollision: две одинаковые батареи (same DeviceName) с
// разными USB-портами дают РАЗНЫЕ ключи бакетов и, соответственно, разные
// 5-минутные точки (V1) — иначе они сливались бы в одну.
func TestBmsAccumulatorNameCollision(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.Local)
	a := newBmsAccumulator()
	dev1 := bmsDevice{DeviceName: "AntBms 320 A/h", Port: "/dev/ttyUSB0", CurrentA: 1, CellCount: 1, CellsV: []float64{3.3}}
	dev2 := bmsDevice{DeviceName: "AntBms 320 A/h", Port: "/dev/ttyUSB1", CurrentA: 2, CellCount: 1, CellsV: []float64{3.4}}
	// Ключи проставляются так же, как в pollAndSaveBMS (resolveBMSKey): при
	// коллизии двух одинаковых имён в коллекции — развод по порту.
	coll := map[string]int{dev1.DeviceName: 2}
	dev1.Key = resolveBMSKey(dev1, coll)
	dev2.Key = resolveBMSKey(dev2, coll)
	a.add(dev1, now)
	a.add(dev2, now)
	closed := a.closed(now.Add(5 * time.Minute))
	if len(closed) != 2 {
		t.Fatalf("closed = %d, want 2 (разные ключи): %+v", len(closed), closed)
	}
	seen := map[string]string{}
	for _, p := range closed {
		if p.display != "AntBms 320 A/h" {
			t.Fatalf("display = %q, want исходный DeviceName", p.display)
		}
		seen[p.name] = seen[p.name] + "|"
	}
	if _, ok := seen["AntBms 320 A/h@/dev/ttyUSB0"]; !ok {
		t.Fatalf("нет ключа @/dev/ttyUSB0: %v", seen)
	}
	if _, ok := seen["AntBms 320 A/h@/dev/ttyUSB1"]; !ok {
		t.Fatalf("нет ключа @/dev/ttyUSB1: %v", seen)
	}
}

func TestBmsBucketAverage(t *testing.T) {
	start := time.Date(2026, 9, 14, 12, 0, 0, 0, time.Local)
	b := newBmsBucket(start)
	// Три 1-секундных снимка: ток 1.0/2.0/3.0 A, ячейки 3.200/3.210 В, T1 25/27/28.
	for _, cur := range []float64{1.0, 2.0, 3.0} {
		b.add(bmsDevice{
			DeviceName:    "AntBms 320 A/h",
			CellCount:     2,
			CellsV:        []float64{3.200, 3.210},
			CurrentA:      cur,
			PowerW:        51.0,
			Soc:           50,
			CapacityAh:    320,
			RemainingAh:   160,
			MaxCellV:      3.210,
			MinCellV:      3.200,
			AvgCellV:      3.205,
			TemperaturesC: []float64{25, 26, 27, 28, 29, 30},
			ChargeMos:     1,
			DischargeMos:  0,
			Balancer:      1,
			Frames:        uint32(100 + int(cur)),
		})
	}
	a := b.avg()
	if a.Samples != 3 {
		t.Fatalf("samples = %d, want 3", a.Samples)
	}
	// Усреднение мгновенных параметров.
	if a.CurrentA != 2.0 {
		t.Errorf("current_a = %v, want 2.0 (среднее)", a.CurrentA)
	}
	if a.PowerW != 51.0 || a.Soc != 50 || a.CapacityAh != 320 || a.RemainingAh != 160 {
		t.Errorf("averaged scalars: %+v", a)
	}
	if a.CellsV[0] != 3.200 || a.CellsV[1] != 3.210 {
		t.Errorf("cells_v = %v, want [3.2 3.21]", a.CellsV)
	}
	// Среднее температур T1..T6: 25..30 → [25 26 27 28 29 30].
	if len(a.Temperatures) != 6 || a.Temperatures[0] != 25 || a.Temperatures[5] != 30 {
		t.Errorf("temperatures = %v", a.Temperatures)
	}
	// Флаги/счётчики — по последнему снимку (ток 3.0 → frames 103).
	if a.ChargeMos != 1 || a.Balancer != 1 || a.Frames != 103 {
		t.Errorf("last-value fields: %+v", a)
	}
	if a.CellCount != 2 {
		t.Errorf("cell_count = %d, want 2", a.CellCount)
	}
}

func TestBmsAccumulatorClosedAndDrain(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.Local)
	a := newBmsAccumulator()
	dev := bmsDevice{DeviceName: "B1", CurrentA: 1, CellCount: 1, CellsV: []float64{3.3}}
	// Снимок из промежутка [12:00, 12:05).
	a.add(dev, now)
	if got := a.closed(now); len(got) != 0 {
		t.Fatalf("closed(12:00) = %d, want 0 (промежуток не завершён)", len(got))
	}
	// Снимок из следующего промежутка [12:05, 12:10) — первый должен закрыться.
	a.add(dev, now.Add(5*time.Minute))
	closed := a.closed(now.Add(5 * time.Minute))
	if len(closed) != 1 || closed[0].name != "B1" || closed[0].avg.Samples != 1 {
		t.Fatalf("closed = %+v, want 1 точка B1 (samples=1)", closed)
	}
	// В аккумуляторе остался один промежуток [12:05, 12:10) → drain его отдаёт.
	drained := a.drain()
	if len(drained) != 1 || !drained[0].start.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("drain = %+v, want 1 пункт с start 12:05", drained)
	}
	if len(a.buckets) != 0 {
		t.Fatalf("после drain buckets = %d, want 0", len(a.buckets))
	}
}
