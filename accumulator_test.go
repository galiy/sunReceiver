package main

import (
	"testing"
	"time"
)

// TestReadBucketWindowBoundary: точка ровно на границе end НЕ входит в окно
// [start, end) (принадлежит следующему бакету), точка за 1 с до end входит.
// Покрывает двойной счёт на границе 5-минутного бакета (п. 1.5).
func TestReadBucketWindowBoundary(t *testing.T) {
	s := testStore(t)
	cleanTestKeys(t, s)
	t.Cleanup(func() { cleanTestKeys(t, s) })

	// b — граница 5-минутного промежутка (кратна 5 мин).
	b := time.Now().Truncate(5 * time.Minute)
	start := b.Add(-5 * time.Minute)

	// Точка ровно на границе b и точка на 1 с до b.
	boundary := snap("A", "192.168.13.200", b, valuesContract{"ac_active_power": 1.0})
	inside := snap("A", "192.168.13.200", b.Add(-1*time.Second), valuesContract{"ac_active_power": 2.0})
	_ = s.SaveSnapshot(boundary, b)
	_ = s.SaveSnapshot(inside, b.Add(-1*time.Second))

	got, err := readBucketWindow(s, start, b)
	if err != nil {
		t.Fatalf("readBucketWindow: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("readBucketWindow len=%d, want 1 (только точка до границы): %+v", len(got), got)
	}
	if got[0].Timestamp != inside.Timestamp {
		t.Fatalf("readBucketWindow вернул точку на границе вместо точки внутри: %+v", got)
	}
}

// TestGroupForBackfill: backfill группирует по floorToStep в строгом окне
// [start, end): точка ts==start входит, точка ts==end (текущий незавершённый
// бакет) пропускается (п. 1.6).
func TestGroupForBackfill(t *testing.T) {
	// start = 10:00, end = 10:05 (оба — границы).
	start := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Minute)
	mk := func(ts time.Time) deviceSnapshot {
		return deviceSnapshot{IP: "192.168.13.1", Timestamp: ts.UTC().Format(time.RFC3339), Values: valuesContract{"ac_active_power": 1.0}}
	}
	snaps := []deviceSnapshot{
		mk(start),                       // ts==start — ВХОДИТ (бакет 10:00)
		mk(start.Add(2 * time.Minute)),  // внутри бакета 10:00
		mk(end),                         // ts==end — ПРОПУСКАЕТСЯ (текущий бакет 10:05)
		mk(end.Add(30 * time.Second)),   // за end — ПРОПУСКАЕТСЯ
		mk(start.Add(-10 * time.Second)), // до start — ПРОПУСКАЕТСЯ
	}
	groups := groupForBackfill(snaps, start, end)
	// Ожидается ровно один бакет (10:00) с двумя снимками.
	if len(groups) != 1 {
		t.Fatalf("groupForBackfill бакетов=%d, want 1: %+v", len(groups), groups)
	}
	k := accBucketKey{ip: "192.168.13.1", bts: start}
	if len(groups[k]) != 2 {
		t.Fatalf("бакет 10:00 снимков=%d, want 2 (start + внутри): %+v", len(groups[k]), groups[k])
	}
	if _, ok := groups[accBucketKey{ip: "192.168.13.1", bts: end}]; ok {
		t.Fatal("точка ts==end попала в текущий бакет — backfill не должен его трогать")
	}
}

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
