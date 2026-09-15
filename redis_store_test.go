package main

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// testStore создаёт тестовый Redis-клиент (предполагается запущенный локально).
func testStore(t *testing.T) *redisStore {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6379"})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Skipf("redis недоступен: %v", err)
	}
	t.Cleanup(func() { rdb.Close() })
	return &redisStore{rdb: rdb, ctx: context.Background()}
}

func snap(name string, ip string, ts time.Time, values valuesContract) deviceSnapshot {
	return deviceSnapshot{Name: name, IP: ip, Timestamp: ts.Format(time.RFC3339), Values: values}
}

func TestRedisStoreCurrentSorted(t *testing.T) {
	s := testStore(t)
	rdb := s.rdb
	rdb.FlushDB(s.ctx)
	defer rdb.FlushDB(s.ctx)

	now := time.Now()
	for _, sp := range []deviceSnapshot{
		snap("Zeta", "192.168.13.99", now, valuesContract{"ac_active_power": 10.0}),
		snap("Alpha", "192.168.13.98", now, valuesContract{"ac_active_power": 20.0}),
	} {
		if err := s.SaveSnapshot(sp, now); err != nil {
			t.Fatalf("SaveSnapshot: %v", err)
		}
	}

	cur, err := s.Current()
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if len(cur) != 2 {
		t.Fatalf("len(Current)=%d, want 2", len(cur))
	}
	if cur[0].Name != "Alpha" || cur[1].Name != "Zeta" {
		t.Fatalf("Current не отсортирован по имени: %+v", cur)
	}
}

func TestRedisStoreQuerySeriesPeriod(t *testing.T) {
	s := testStore(t)
	rdb := s.rdb
	rdb.FlushDB(s.ctx)
	defer rdb.FlushDB(s.ctx)

	// Три точки: 2 текущего месяца, 1 — пересечение, чтобы проверить сегментацию месяцев.
	base := time.Now()
	a := base.Add(-25 * 24 * time.Hour) // ~предыдущий месяц
	b := base.Add(-2 * time.Hour)
	c := base.Add(-1 * time.Hour)
	for _, ts := range []time.Time{a, b, c} {
		_ = s.SaveSnapshot(snap("A", "192.168.13.1", ts, valuesContract{"energy_total": 1.0}), ts)
	}

	got, err := s.QuerySeries(base.Add(-30*24*time.Hour), base)
	if err != nil {
		t.Fatalf("QuerySeries: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("QuerySeries len=%d, want 3", len(got))
	}
}

func TestRedisStoreBMSSeriesRoundtrip(t *testing.T) {
	s := testStore(t)
	rdb := s.rdb
	rdb.FlushDB(s.ctx)
	defer rdb.FlushDB(s.ctx)

	now := time.Now()
	ts1 := floorToStep(now.Add(-10 * time.Minute))
	ts2 := floorToStep(now.Add(-5 * time.Minute))
	p1 := bmsSeriesPoint{Name: "AntBms 320 A/h", Ts: ts1.Format(time.RFC3339)}
	p1.bmsAveraged = bmsAveraged{CurrentA: 1.0, RemainingAh: 100, Samples: 30}
	p2 := bmsSeriesPoint{Name: "AntBms 320 A/h", Ts: ts2.Format(time.RFC3339)}
	p2.bmsAveraged = bmsAveraged{CurrentA: 2.0, RemainingAh: 101, Samples: 30}
	p3 := bmsSeriesPoint{Name: "Other BMS", Ts: ts2.Format(time.RFC3339)}
	p3.bmsAveraged = bmsAveraged{CurrentA: 9.0, Samples: 30}

	for _, pp := range []struct {
		p  bmsSeriesPoint
		ts time.Time
	}{{p1, ts1}, {p2, ts2}, {p3, ts2}} {
		if err := s.SaveBMSSeries(pp.p, pp.ts); err != nil {
			t.Fatalf("SaveBMSSeries %s: %v", pp.p.Name, err)
		}
	}

	// Точный срез по имени: только 2 точки нашей BMS, без Other BMS.
	got, err := s.QueryBMSSeries("AntBms 320 A/h", now.Add(-30*time.Minute), now)
	if err != nil {
		t.Fatalf("QueryBMSSeries: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("QueryBMSSeries len=%d, want 2: %+v", len(got), got)
	}
	if got[0].CurrentA != 1.0 || got[1].CurrentA != 2.0 {
		t.Fatalf("порядок/значения: %+v", got)
	}

	// PurgeOld удаляет из BMS-ряда точки старше окна 2 календарных суток.
	old := ts1.AddDate(0, 0, -3)
	po := bmsSeriesPoint{Name: "AntBms 320 A/h", Ts: old.Format(time.RFC3339)}
	if err := s.SaveBMSSeries(po, old); err != nil {
		t.Fatalf("SaveBMSSeries old: %v", err)
	}
	s.PurgeOld(now)
	got2, err := s.QueryBMSSeries("AntBms 320 A/h", old, now)
	if err != nil {
		t.Fatalf("QueryBMSSeries after purge: %v", err)
	}
	if len(got2) != 2 {
		t.Fatalf("после PurgeOld len=%d, want 2 (старая точка удалена): %+v", len(got2), got2)
	}
}

// TestRedisStoreBMSSeriesReplace: повторная запись той же точки (устройство,
// 5-минутный промежуток) — ситуация «дрейн при остановке пулера + продолжение
// того же промежутка новым процессом» — должна ЗАМЕНЯТЬ старую точку, а не
// плодить второй member с тем же score (на графике это «ступенька» — два
// агрегата в одну засечку времени). Чужие устройства на том же score не
// затрагиваются.
func TestRedisStoreBMSSeriesReplace(t *testing.T) {
	s := testStore(t)
	rdb := s.rdb
	rdb.FlushDB(s.ctx)
	defer rdb.FlushDB(s.ctx)

	now := time.Now()
	ts := floorToStep(now)
	p1 := bmsSeriesPoint{Name: "B1", Ts: ts.Format(time.RFC3339)}
	p1.bmsAveraged = bmsAveraged{CurrentA: 1.0, Samples: 120}
	p2 := bmsSeriesPoint{Name: "B1", Ts: ts.Format(time.RFC3339)}
	p2.bmsAveraged = bmsAveraged{CurrentA: 2.0, Samples: 180}
	other := bmsSeriesPoint{Name: "B2", Ts: ts.Format(time.RFC3339)}
	other.bmsAveraged = bmsAveraged{CurrentA: 9.0, Samples: 300}

	for _, pp := range []bmsSeriesPoint{p1, other, p2} {
		if err := s.SaveBMSSeries(pp, ts); err != nil {
			t.Fatalf("SaveBMSSeries %s: %v", pp.Name, err)
		}
	}

	got, err := s.QueryBMSSeries("B1", ts, now)
	if err != nil {
		t.Fatalf("QueryBMSSeries B1: %v", err)
	}
	if len(got) != 1 || got[0].CurrentA != 2.0 || got[0].Samples != 180 {
		t.Fatalf("B1: %+v, want ровно 1 точка (новая версия current=2.0 samples=180)", got)
	}
	got2, err := s.QueryBMSSeries("B2", ts, now)
	if err != nil {
		t.Fatalf("QueryBMSSeries B2: %v", err)
	}
	if len(got2) != 1 || got2[0].CurrentA != 9.0 {
		t.Fatalf("B2 задета заменой: %+v, want 1 точка current=9.0", got2)
	}
}