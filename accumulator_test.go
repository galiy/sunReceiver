package main

import (
	"testing"
	"time"
)

func TestFloorToStep(t *testing.T) {
	// Граница сохраняется как есть.
	b := time.Date(2026, 9, 15, 10, 5, 0, 0, time.UTC)
	if got := floorToStep(b); !got.Equal(b) {
		t.Fatalf("floorToStep(граница)=%s, want %s", got, b)
	}
	// Внутри промежутка — начало промежутка.
	in := time.Date(2026, 9, 15, 10, 7, 30, 0, time.UTC)
	want := time.Date(2026, 9, 15, 10, 5, 0, 0, time.UTC)
	if got := floorToStep(in); !got.Equal(want) {
		t.Fatalf("floorToStep(%s)=%s, want %s", in, got, want)
	}
}

func TestNextBoundary(t *testing.T) {
	// Строго после: ровно на границе — следующая граница.
	b := time.Date(2026, 9, 15, 10, 5, 0, 0, time.UTC)
	want := b.Add(5 * time.Minute)
	if got := nextBoundary(b); !got.Equal(want) {
		t.Fatalf("nextBoundary(граница)=%s, want %s", got, want)
	}
	// Внутри промежутка — конец промежутка.
	in := time.Date(2026, 9, 15, 10, 5, 1, 0, time.UTC)
	if got := nextBoundary(in); !got.Equal(want) {
		t.Fatalf("nextBoundary(%s)=%s, want %s", in, got, want)
	}
}

func TestAverageValues(t *testing.T) {
	snaps := []deviceSnapshot{
		{Timestamp: "2026-09-15T10:00:10Z", Values: valuesContract{"ac_active_power": 100.0, "energy_total": 1000.0, "note": "x"}},
		{Timestamp: "2026-09-15T10:00:20Z", Values: valuesContract{"ac_active_power": 200.0, "energy_total": 2000.0, "note": "y"}},
		{Timestamp: "2026-09-15T10:00:30Z", Values: valuesContract{"ac_active_power": 300.0, "energy_total": 3000.0}},
	}
	got := averageValues(snaps)
	// Обычные теги — среднее окна.
	if got["ac_active_power"] != 200.0 {
		t.Fatalf("ac_active_power=%v, want 200.0 (среднее)", got["ac_active_power"])
	}
	// Накопительные — последнее по времени значение.
	if got["energy_total"] != 3000.0 {
		t.Fatalf("energy_total=%v, want 3000.0 (последнее)", got["energy_total"])
	}
	// Нечисловые теги в результат не попадают.
	if _, ok := got["note"]; ok {
		t.Fatalf("нечисловой тег note в результате")
	}
}

func TestAverageValuesAccumulatorOutOfOrder(t *testing.T) {
	// Снимки в срезе НЕ по возрастанию ts: побеждает значение с наибольшим
	// timestamp, а не последнее в срезе.
	snaps := []deviceSnapshot{
		{Timestamp: "2026-09-15T10:00:30Z", Values: valuesContract{"energy_total": 3000.0}},
		{Timestamp: "2026-09-15T10:00:10Z", Values: valuesContract{"energy_total": 1000.0}},
	}
	got := averageValues(snaps)
	if got["energy_total"] != 3000.0 {
		t.Fatalf("energy_total=%v, want 3000.0 (max ts)", got["energy_total"])
	}
}
