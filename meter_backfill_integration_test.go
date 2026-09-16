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
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

const meterTestIP = "192.168.0.254"

// meterSeededPoint — тестовая точка ряда: член ZSET + ключ месячного сегмента,
// в который она попала (для точной очистки; точка может лечь в сегмент другого
// месяца, чем ближайшая к ней граница).
type meterSeededPoint struct {
	key    string
	member []byte
}

// seedMeterSeries кладёт в Redis временной ряд счётчика testIP снимки в заданных
// точках и возвращает точные (ключ-сегмент, член) для последующей очистки.
func seedMeterSeries(t *testing.T, store *redisStore, cfg *meterConfig, pts []struct {
	ts  time.Time
	imp float64
}) []meterSeededPoint {
	t.Helper()
	var members []meterSeededPoint
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
		members = append(members, meterSeededPoint{key: redisSeriesKey(p.ts), member: mem})
	}
	return members
}

// cleanMeterSeeded удаляет тестовые точки ряда и текущий снимок testIP.
func cleanMeterSeeded(rdb *redis.Client, ctx context.Context, members []meterSeededPoint) {
	for _, m := range members {
		rdb.ZRem(ctx, m.key, m.member)
	}
	rdb.HDel(ctx, redisCurrentKey, meterTestIP)
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
		cleanMeterSeeded(rdb, ctx, members)
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
// сеем показания вокруг границы (сегодняшнее 07:00 или вчерашнее 23:00 —
// какая безопасно в прошлом), запускаем backfillMeterBoundaries и проверяем,
// что граница дофиксирована ближайшим показанием из ряда.
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
	ny, nmo, nd := now.In(loc).Date()
	// Граница: сегодняшнее 07:00, если оно уже безопасно в прошлом
	// (> meterBackfillMinAge); иначе — ВЧЕРАШНЕЕ 23:00 (всегда >10 мин в прошлом
	// и в окне Redis). Свое сегодняшнее 00:00 брать нельзя: в окне 00:00–00:10
	// оно младше meterBackfillMinAge, добор его пропускает и тест флейчил.
	var boundary time.Time
	if b := time.Date(ny, nmo, nd, meterDayStartH, 0, 0, 0, loc); now.Sub(b) >= meterBackfillMinAge {
		boundary = b
	} else {
		boundary = time.Date(ny, nmo, nd, meterDayEndH, 0, 0, 0, loc).AddDate(0, 0, -1)
	}
	by, bmo, bd := boundary.In(loc).Date()
	day := time.Date(by, bmo, bd, 0, 0, 0, 0, loc)

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
		cleanMeterSeeded(rdb, ctx, members)
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
