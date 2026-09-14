package main

import (
	"testing"
	"time"
)

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
	closed := a.closed(now.Add(5*time.Minute))
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
