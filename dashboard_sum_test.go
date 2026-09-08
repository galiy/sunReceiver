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