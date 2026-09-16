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

// TestMeterTariffBoundaryZeroAtomic: граница 00:00 должна записать ОБЕ колонки
// (import_0000 дня D И import_next дня D−1) одной транзакцией; повторный вызов —
// идемпотентен. meterBoundaryCaptured для часа 0 — true только когда непусты обе.
func TestMeterTariffBoundaryZeroAtomic(t *testing.T) {
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
	days := []time.Time{
		time.Date(2026, 9, 1, 0, 0, 0, 0, loc),
		time.Date(2026, 9, 2, 0, 0, 0, 0, loc),
		time.Date(2026, 9, 3, 0, 0, 0, 0, loc),
	}
	clean := func() {
		for _, d := range days {
			pgc.pool.Exec(ctx, `DELETE FROM sunreceiver.daily_tariffs WHERE day = $1`, d)
		}
	}
	clean()
	defer clean()

	// Граница 00:00 дня D=2026-09-02.
	b := time.Date(2026, 9, 2, 0, 0, 0, 0, loc)
	if err := pgc.StoreMeterBoundary(b, 500.0, 50.0); err != nil {
		t.Fatalf("StoreMeterBoundary(00:00): %v", err)
	}
	// Обе колонки записаны: import_0000[D] и import_next[D−1].
	var imp0000, importNext *float64
	if err := pgc.pool.QueryRow(ctx, `SELECT "import_0000" FROM sunreceiver.daily_tariffs WHERE day=$1`,
		time.Date(2026, 9, 2, 0, 0, 0, 0, loc)).Scan(&imp0000); err != nil || imp0000 == nil || *imp0000 != 500 {
		t.Fatalf("import_0000[D] = %v (err %v), want 500", imp0000, err)
	}
	if err := pgc.pool.QueryRow(ctx, `SELECT "import_next" FROM sunreceiver.daily_tariffs WHERE day=$1`,
		time.Date(2026, 9, 1, 0, 0, 0, 0, loc)).Scan(&importNext); err != nil || importNext == nil || *importNext != 500 {
		t.Fatalf("import_next[D-1] = %v (err %v), want 500", importNext, err)
	}
	// Граница 00:00 «захвачена» — обе колонки непусты.
	if !pgc.meterBoundaryCaptured(b) {
		t.Fatal("meterBoundaryCaptured(00:00)=false, want true (обе колонки непусты)")
	}

	// Идемпотентный повтор с теми же значениями: значения не меняются.
	if err := pgc.StoreMeterBoundary(b, 500.0, 50.0); err != nil {
		t.Fatalf("StoreMeterBoundary идемпотентный повтор: %v", err)
	}
	if err := pgc.pool.QueryRow(ctx, `SELECT "import_0000" FROM sunreceiver.daily_tariffs WHERE day=$1`,
		time.Date(2026, 9, 2, 0, 0, 0, 0, loc)).Scan(&imp0000); err != nil || *imp0000 != 500 {
		t.Fatalf("import_0000 после идемпотентного повтора = %v, want 500 (значения не изменились)", imp0000)
	}
	// Повтор с ДРУГИМИ значениями — DO UPDATE (override): более поздняя запись
	// того же (день, граница) побеждает (last-write-wins), а не игнорируется.
	if err := pgc.StoreMeterBoundary(b, 999.0, 99.0); err != nil {
		t.Fatalf("StoreMeterBoundary повтор (override): %v", err)
	}
	if err := pgc.pool.QueryRow(ctx, `SELECT "import_0000" FROM sunreceiver.daily_tariffs WHERE day=$1`,
		time.Date(2026, 9, 2, 0, 0, 0, 0, loc)).Scan(&imp0000); err != nil || *imp0000 != 999 {
		t.Fatalf("import_0000 после override = %v, want 999 (do update)", imp0000)
	}

	// Самовосстановление: имитируем частичную запись (только import_0000[D],
	// import_next[D−1] пропущен — транзиентный сбой PG в старом неатомарном коде).
	// meterBoundaryCaptured для часа 0 должен вернуть false → добор повторит запись.
	pgc.pool.Exec(ctx, `UPDATE sunreceiver.daily_tariffs SET "import_next"=NULL WHERE day=$1`,
		time.Date(2026, 9, 1, 0, 0, 0, 0, loc))
	if pgc.meterBoundaryCaptured(b) {
		t.Fatal("meterBoundaryCaptured(00:00)=true при частичной записи, want false (импорт_next[D-1] NULL)")
	}
}