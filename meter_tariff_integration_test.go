package main

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestMeterTariffPGIntegration(t *testing.T) {
	if os.Getenv("METER_PG_TEST") == "" {
		t.Skip("METER_PG_TEST not set")
	}
	pgc, err := openPG("postgres://localhost:5432/sunreceiver?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer pgc.Close()
	ctx := context.Background()
	loc := time.Local
	// Синтетический день 2026-09-01
	day := time.Date(2026, 9, 1, 0, 0, 0, 0, loc)
	// Чистим только синтетические строки: 2026-08-31 (prev-day имплицитно от 00:00 09-01),
	// 2026-09-01 и 2026-09-02.
	clean := func() {
		for _, d := range []time.Time{
			time.Date(2026, 8, 31, 0, 0, 0, 0, loc),
			time.Date(2026, 9, 1, 0, 0, 0, 0, loc),
			time.Date(2026, 9, 2, 0, 0, 0, 0, loc),
		} {
			pgc.pool.Exec(ctx, `DELETE FROM sunreceiver.daily_tariffs WHERE day = $1`, d)
		}
	}
	clean()
	defer clean()

	// 00:00 D
	pgc.StoreMeterBoundary(time.Date(2026, 9, 1, 0, 0, 0, 0, loc), 1000.00, 100.00)
	// 07:00 D
	pgc.StoreMeterBoundary(time.Date(2026, 9, 1, 7, 0, 0, 0, loc), 1010.00, 102.00)
	// 23:00 D
	pgc.StoreMeterBoundary(time.Date(2026, 9, 1, 23, 0, 0, 0, loc), 1030.00, 104.00)
	// 00:00 D+1
	pgc.StoreMeterBoundary(time.Date(2026, 9, 2, 0, 0, 0, 0, loc), 1032.00, 105.00)

	var impDay, impNight, expDay, expNight float64
	var finalized *time.Time
	if err := pgc.pool.QueryRow(ctx, `
SELECT import_day, import_night, export_day, export_night, finalized
FROM sunreceiver.daily_tariffs WHERE day = $1`, day).Scan(&impDay, &impNight, &expDay, &expNight, &finalized); err != nil {
		t.Fatalf("select: %v", err)
	}
	if finalized == nil {
		t.Fatal("finalized = NULL, ожидали финальный день")
	}
	const eps = 0.001
	if abs(impDay-20) > eps {
		t.Errorf("import_day = %v, want 20", impDay)
	}
	if abs(impNight-12) > eps {
		t.Errorf("import_night = %v, want 12", impNight)
	}
	if abs(expDay-2) > eps {
		t.Errorf("export_day = %v, want 2", expDay)
	}
	if abs(expNight-3) > eps {
		t.Errorf("export_night = %v, want 3", expNight)
	}
}