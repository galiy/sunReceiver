package main

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

const meterTestIP = "192.168.0.254"

// seedMeterSeries кладёт в Redis временной ряд счётчика testIP снимки в заданных
// точках и возвращает точные члены ZSET для последующей очистки.
func seedMeterSeries(t *testing.T, store *redisStore, cfg *meterConfig, pts []struct {
	ts  time.Time
	imp float64
}) [][]byte {
	t.Helper()
	var members [][]byte
	for _, p := range pts {
		snap := deviceSnapshot{
			Name: cfg.Name, IP: cfg.IP, Timestamp: p.ts.Format(time.RFC3339), Values: valuesContract{
				"meter_import": p.imp, "meter_export": p.imp / 10,
			},
		}
		if err := store.SaveSnapshot(snap, p.ts); err != nil {
			t.Fatalf("seed %v: %v", p.ts, err)
		}
		mem, _ := json.Marshal(snap)
		members = append(members, mem)
	}
	return members
}

// TestMeterBackfillNearest — интеграционный тест добора пропущенной границы:
// сеем в Redis ряд счётчика (уникальный тестовый IP) с показаниями вокруг
// границы, убеждаемся, что nearestMeterReading находит ближайшее к границе,
// а StoreMeterBoundary фиксирует его в daily_tariffs.
func TestMeterBackfillNearest(t *testing.T) {
	if os.Getenv("METER_PG_TEST") == "" {
		t.Skip("METER_PG_TEST not set")
	}
	pgc, err := openPG("postgres://localhost:5432/sunreceiver?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer pgc.Close()

	rdb, err := openRedis("127.0.0.1:6379")
	if err != nil {
		t.Fatal(err)
	}
	store := &redisStore{rdb: rdb, ctx: context.Background()}
	ctx := context.Background()

	cfg := &meterConfig{Name: "test", IP: meterTestIP, Port: 502, Unit: 1}
	loc := time.Local
	b := time.Date(2026, 9, 8, 7, 0, 0, 0, loc) // граница 07:00 сегодня
	day := time.Date(2026, 9, 8, 0, 0, 0, 0, loc)

	pgc.pool.Exec(ctx, `DELETE FROM sunreceiver.daily_tariffs WHERE day = $1`, day)
	rdb.HDel(ctx, redisCurrentKey, meterTestIP)
	members := seedMeterSeries(t, store, cfg, []struct {
		ts  time.Time
		imp float64
	}{
		{b.Add(-3 * time.Minute), 1000},
		{b.Add(-1 * time.Minute), 1010},
		{b.Add(2 * time.Second), 1012}, // ближайшее к границе
		{b.Add(5 * time.Minute), 1020},
	})
	defer func() {
		for _, m := range members {
			rdb.ZRem(ctx, redisSeriesKey(b), m)
		}
		rdb.HDel(ctx, redisCurrentKey, meterTestIP)
		pgc.pool.Exec(ctx, `DELETE FROM sunreceiver.daily_tariffs WHERE day = $1`, day)
	}()

	imp, exp, ok := nearestMeterReading(store, cfg, b)
	if !ok {
		t.Fatal("nearestMeterReading: ok=false")
	}
	if abs(imp-1012) > 0.001 {
		t.Errorf("imp = %v, want 1012 (ближайшее к границе)", imp)
	}
	if abs(exp-101.2) > 0.001 {
		t.Errorf("exp = %v, want 101.2", exp)
	}
	if err := pgc.StoreMeterBoundary(b, imp, exp); err != nil {
		t.Fatal(err)
	}
	if !pgc.meterBoundaryCaptured(b) {
		t.Fatal("meterBoundaryCaptured = false, ожидали захват границы")
	}
}

// TestMeterBackfillEndToEnd — полный сценарий добора: для тестового устройства
// сеем показания вокруг сегодняшней границы 07:00 (без живого захвата), запускаем
// backfillMeterBoundaries и проверяем, что граница дофиксирована ближайшим
// показанием из ряда.
func TestMeterBackfillEndToEnd(t *testing.T) {
	if os.Getenv("METER_PG_TEST") == "" {
		t.Skip("METER_PG_TEST not set")
	}
	pgc, err := openPG("postgres://localhost:5432/sunreceiver?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer pgc.Close()

	rdb, err := openRedis("127.0.0.1:6379")
	if err != nil {
		t.Fatal(err)
	}
	store := &redisStore{rdb: rdb, ctx: context.Background()}
	ctx := context.Background()

	cfg := &meterConfig{Name: "test", IP: meterTestIP, Port: 502, Unit: 1}
	loc := time.Local
	now := time.Now()
	y, mo, d := now.In(loc).Date()
	// Граница 07:00 «сегодня»: если сейчас позже 07:10 — она безопасно в прошлом
	// и подходит для добора; иначе берём ещё вчерашнюю (00:00), которая точно
	// безопасно в прошлом и попадает в окно Redis.
	boundary := time.Date(y, mo, d, meterDayStartH, 0, 0, 0, loc)
	if now.Sub(boundary) < meterBackfillMinAge {
		boundary = time.Date(y, mo, d, 0, 0, 0, 0, loc)
	}
	day := time.Date(y, mo, d, 0, 0, 0, 0, loc)

	pgc.pool.Exec(ctx, `DELETE FROM sunreceiver.daily_tariffs WHERE day = $1`, day)
	rdb.HDel(ctx, redisCurrentKey, meterTestIP)
	members := seedMeterSeries(t, store, cfg, []struct {
		ts  time.Time
		imp float64
	}{
		{boundary.Add(-2 * time.Minute), 2000},
		{boundary.Add(-30 * time.Second), 2010}, // ближайшее
		{boundary.Add(4 * time.Minute), 2030},
	})
	defer func() {
		for _, m := range members {
			rdb.ZRem(ctx, redisSeriesKey(boundary), m)
		}
		rdb.HDel(ctx, redisCurrentKey, meterTestIP)
		pgc.pool.Exec(ctx, `DELETE FROM sunreceiver.daily_tariffs WHERE day = $1`, day)
	}()

	backfillMeterBoundaries(store, pgc, cfg, now)

	if !pgc.meterBoundaryCaptured(boundary) {
		t.Fatalf("граница %s не дофиксирована добором", boundary.Format(time.RFC3339))
	}
	var imp *float64
	if err := pgc.pool.QueryRow(ctx,
		`SELECT "`+meterBoundaryImportCol(boundary.Hour())+`" FROM sunreceiver.daily_tariffs WHERE day=$1`,
		day).Scan(&imp); err != nil {
		t.Fatal(err)
	}
	if imp == nil || abs(*imp-2010) > 0.001 {
		t.Errorf("импортировано = %v, want 2010 (ближайшее)", imp)
	}
}