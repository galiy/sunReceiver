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
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// testStore создаёт тестовый Redis-клиент (предполагается запущенный локально).
func testStore(t *testing.T) *redisStore {
	t.Helper()
	// Отдельная БД (15), чтобы тесты не чистили ключи приложения в DB 0 на
	// машине разработки с живым локальным Redis.
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6379", DB: 15})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Skipf("redis недоступен: %v", err)
	}
	t.Cleanup(func() { rdb.Close() })
	return &redisStore{rdb: rdb, ctx: context.Background()}
}

// cleanTestKeys удаляет только ключи приложения (sunreceiver:*), а не всю
// БД (FlushDB): на машине разработки с живым локальным Redis, хранящим
// реальные данные, `go test ./...` не должен стирать их.
func cleanTestKeys(t *testing.T, s *redisStore) {
	t.Helper()
	var cursor uint64
	for {
		keys, cur, err := s.rdb.Scan(s.ctx, cursor, "sunreceiver:*", 100).Result()
		if err != nil {
			t.Fatalf("scan sunreceiver:*: %v", err)
		}
		if len(keys) > 0 {
			if err := s.rdb.Del(s.ctx, keys...).Err(); err != nil {
				t.Fatalf("del sunreceiver:*: %v", err)
			}
		}
		if cur == 0 {
			return
		}
		cursor = cur
	}
}

func snap(name string, ip string, ts time.Time, values valuesContract) deviceSnapshot {
	return deviceSnapshot{Name: name, IP: ip, Timestamp: ts.Format(time.RFC3339), Values: values}
}

func TestRedisStoreCurrentSorted(t *testing.T) {
	s := testStore(t)
	cleanTestKeys(t, s)
	t.Cleanup(func() { cleanTestKeys(t, s) })

	now := time.Now()
	for _, sp := range []deviceSnapshot{
		snap("Zeta", "192.0.2.99", now, valuesContract{"ac_active_power": 10.0}),
		snap("Alpha", "192.0.2.98", now, valuesContract{"ac_active_power": 20.0}),
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
	cleanTestKeys(t, s)
	t.Cleanup(func() { cleanTestKeys(t, s) })

	// Три точки: 2 текущего месяца, 1 — пересечение, чтобы проверить сегментацию месяцев.
	base := time.Now()
	a := base.Add(-25 * 24 * time.Hour) // ~предыдущий месяц
	b := base.Add(-2 * time.Hour)
	c := base.Add(-1 * time.Hour)
	for _, ts := range []time.Time{a, b, c} {
		_ = s.SaveSnapshot(snap("A", "192.0.2.1", ts, valuesContract{"energy_total": 1.0}), ts)
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
	cleanTestKeys(t, s)
	t.Cleanup(func() { cleanTestKeys(t, s) })

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
	cleanTestKeys(t, s)
	t.Cleanup(func() { cleanTestKeys(t, s) })

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

// TestSaveSnapshotMAPPerReading: SaveSnapshotMAP пишет КАЖДОЕ показание (каждая
// секунда — отдельный member ZSET); повтор в ту же секунду заменяет (dedup), а
// недостающие МАП-теги дополняются из предыдущего снимка того же устройства.
func TestSaveSnapshotMAPPerReading(t *testing.T) {
	s := testStore(t)
	cleanTestKeys(t, s)
	t.Cleanup(func() { cleanTestKeys(t, s) })

	now := time.Now()
	t1 := now.Add(-6 * time.Second).Truncate(time.Second)
	t2 := t1.Add(3 * time.Second)
	ip := "192.0.2.74"
	// isMAPDevice требует battery_voltage — иначе merge не включается.
	sp1 := snap("MAP", ip, t1, valuesContract{"battery_voltage": 52.0, "grid_voltage": 230.0})
	sp2 := snap("MAP", ip, t2, valuesContract{"battery_voltage": 52.1, "grid_voltage": 231.0})

	if err := s.SaveSnapshotMAP(sp1, t1); err != nil {
		t.Fatalf("SaveSnapshotMAP #1: %v", err)
	}
	if err := s.SaveSnapshotMAP(sp2, t2); err != nil {
		t.Fatalf("SaveSnapshotMAP #2: %v", err)
	}
	key := redisSeriesKey(t1)
	countAt := func(ts time.Time) int {
		members, err := s.rdb.ZRangeByScore(s.ctx, key, &redis.ZRangeBy{
			Min: strconv.FormatInt(ts.Unix(), 10), Max: strconv.FormatInt(ts.Unix(), 10)}).Result()
		if err != nil {
			t.Fatalf("ZRangeByScore: %v", err)
		}
		return len(members)
	}
	// Каждое показание сохранено отдельно (не схлопнуто в окно).
	if n := countAt(t1); n != 1 {
		t.Fatalf("members на t1=%d, want 1", n)
	}
	if n := countAt(t2); n != 1 {
		t.Fatalf("members на t2=%d, want 1", n)
	}
	// Повторная запись в ту же секунду — замена, не дубль.
	if err := s.SaveSnapshotMAP(snap("MAP", ip, t1, valuesContract{"battery_voltage": 52.2}), t1); err != nil {
		t.Fatalf("SaveSnapshotMAP повтор: %v", err)
	}
	if n := countAt(t1); n != 1 {
		t.Fatalf("после повтора members на t1=%d, want 1", n)
	}
	// MAP-merge: снимок без grid_voltage получает последнее известное значение.
	t3 := t2.Add(3 * time.Second)
	if err := s.SaveSnapshotMAP(snap("MAP", ip, t3, valuesContract{"battery_voltage": 52.3}), t3); err != nil {
		t.Fatalf("SaveSnapshotMAP #3: %v", err)
	}
	raw, err := s.rdb.HGet(s.ctx, redisCurrentKey, ip).Result()
	if err != nil {
		t.Fatalf("HGET current: %v", err)
	}
	var got deviceSnapshot
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := got.Values["grid_voltage"]; !ok {
		t.Fatalf("merge не дополнил grid_voltage: %+v", got.Values)
	}
}

// TestRedisStoreSnapshotDedup: две записи одного устройства в одну и ту же
// секунду — в ZSET ряда остаётся ровно ОДИН member со score этой секунды
// (вторая запись заменяет первую). Чужие устройства на том же score не
// затрагиваются.
func TestRedisStoreSnapshotDedup(t *testing.T) {
	s := testStore(t)
	cleanTestKeys(t, s)
	t.Cleanup(func() { cleanTestKeys(t, s) })

	now := time.Now()
	ts := now.Truncate(time.Second)
	ip := "192.0.2.77"
	s1 := snap("A", ip, ts, valuesContract{"ac_active_power": 1.0})
	s2 := snap("A", ip, ts.Add(500*time.Millisecond), valuesContract{"ac_active_power": 2.0})
	other := snap("B", "192.0.2.78", ts, valuesContract{"ac_active_power": 9.0})

	for _, sp := range []deviceSnapshot{s1, other, s2} {
		if err := s.SaveSnapshot(sp, ts); err != nil {
			t.Fatalf("SaveSnapshot %s: %v", sp.IP, err)
		}
	}

	key := redisSeriesKey(ts)
	score := strconv.FormatInt(ts.Unix(), 10)
	members, err := s.rdb.ZRangeByScore(s.ctx, key, &redis.ZRangeBy{Min: score, Max: score}).Result()
	if err != nil {
		t.Fatalf("ZRangeByScore: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("members со score секунды=%d, want 2 (A заменён на 1, B без изменений): %v", len(members), members)
	}
	got, err := s.QuerySeries(ts, ts)
	if err != nil {
		t.Fatalf("QuerySeries: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("точки за секунду: %+v, want 2 (A заменён на свежую, B без изменений)", got)
	}
	var a *deviceSnapshot
	for i := range got {
		if got[i].IP == ip {
			a = &got[i]
		}
	}
	if a == nil {
		t.Fatalf("устройство %s не найдено: %+v", ip, got)
	}
	if a.Values["ac_active_power"] != 2.0 {
		t.Fatalf("не заменена на свежую: %+v", a.Values)
	}
}
