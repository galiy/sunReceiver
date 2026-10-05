package main

import (
	"testing"
	"time"
)

// TestLoadMercuryConfig проверяет разбор раздела mercury: сортировку по времени
// и ошибку на неверный taken_at.
func TestLoadMercuryConfig(t *testing.T) {
	secs := []mercurySection{
		{TakenAt: "2026-10-06T07:00:00+03:00", ImportDay: 2},
		{TakenAt: "2026-10-05T23:00:00+03:00", ImportDay: 1, ImportNight: 11, ExportDay: 12, ExportNight: 13},
	}
	if err := loadMercuryConfig(secs); err != nil {
		t.Fatalf("loadMercuryConfig: %v", err)
	}
	if len(mercuryReadings) != 2 {
		t.Fatalf("точек: %d, want 2", len(mercuryReadings))
	}
	if !mercuryReadings[0].TakenAt.Equal(time.Date(2026, 10, 5, 23, 0, 0, 0, time.FixedZone("", 3*3600))) {
		t.Fatalf("первая точка не самая ранняя: %v", mercuryReadings[0].TakenAt)
	}
	if mercuryReadings[0].ExportNight != 13 {
		t.Fatalf("ExportNight=%v, want 13", mercuryReadings[0].ExportNight)
	}

	// Пустой раздел — допустим.
	if err := loadMercuryConfig(nil); err != nil || mercuryReadings != nil {
		t.Fatalf("пустой раздел: err=%v readings=%v", err, mercuryReadings)
	}

	// Неверный формат времени — ошибка.
	if err := loadMercuryConfig([]mercurySection{{TakenAt: "05.10.2026 23:00"}}); err == nil {
		t.Fatalf("ожидали ошибку на неверный taken_at")
	}
}
