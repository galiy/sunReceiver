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

// TestDdsTariffPartialUpTo проверяет выбор ближайшей ПРЕДШЕСТВУЮЩЕЙ тарифной
// границы: до 07:00 прирост нулевой (граница — 00:00), днём ночь накоплена, к
// концу дня добавляется день. Регресс: раньше до 07:00 возвращалась вся ночная
// разница [00:00,07:00] (занижало прогноз «Меркурия»).
func TestDdsTariffPartialUpTo(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	b := &meterBoundaryRow{
		Import0000: f(100), Import0700: f(110), Import2300: f(130),
		Export0000: f(200), Export0700: f(205), Export2300: f(215),
	}
	loc := time.Local
	at := func(h, m int) time.Time { return time.Date(2026, 10, 5, h, m, 0, 0, loc) }

	// 00:00–06:59 — предшествующая граница 00:00: прироста нет.
	if d, n, _, _ := ddsTariffPartialUpTo(at(3, 0), b); d != 0 || n != 0 {
		t.Fatalf("03:00: impDay=%v impNight=%v, want 0/0", d, n)
	}
	// 07:00 — ночь [00:00,07:00] накоплена, день ещё нет.
	if d, n, _, en := ddsTariffPartialUpTo(at(7, 0), b); d != 0 || n != 10 || en != 5 {
		t.Fatalf("07:00: impDay=%v impNight=%v expNight=%v, want 0/10/5", d, n, en)
	}
	// 12:00 — ночь накоплена, день нет.
	if d, n, _, _ := ddsTariffPartialUpTo(at(12, 0), b); d != 0 || n != 10 {
		t.Fatalf("12:00: impDay=%v impNight=%v, want 0/10", d, n)
	}
	// 23:00 — день [07:00,23:00] полон, ночь (первая часть) накоплена.
	if d, n, ed, en := ddsTariffPartialUpTo(at(23, 0), b); d != 20 || n != 10 || ed != 10 || en != 5 {
		t.Fatalf("23:00: impDay=%v impNight=%v expDay=%v expNight=%v, want 20/10/10/5", d, n, ed, en)
	}
	// 23:30 — то же (прирост [23:00,at] не атрибуцируется).
	if d, n, _, _ := ddsTariffPartialUpTo(at(23, 30), b); d != 20 || n != 10 {
		t.Fatalf("23:30: impDay=%v impNight=%v, want 20/10", d, n)
	}
}
