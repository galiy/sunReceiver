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