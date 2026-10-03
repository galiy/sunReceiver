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
	"testing"
	"time"
)

// TestMigrateMeterDeviceKey проверяет однократный перенос идентификатора счётчика
// на единый ключ: current-поле, точки временного ряда, маркер и идемпотентность.
func TestMigrateMeterDeviceKey(t *testing.T) {
	s := testStore(t)
	cleanTestKeys(t, s)
	t.Cleanup(func() { cleanTestKeys(t, s) })

	now := time.Now()
	meterVals := valuesContract{"meter_voltage": 230.0, "meter_active_power": -100.0}
	invVals := valuesContract{"ac_active_power": 42.0}

	// Старый снимок счётчика под IP .77 и обычный инвертор (не должен трогаться).
	if err := s.SaveSnapshot(snap("M", "192.0.2.77", now.Add(-2*time.Second), meterVals), now.Add(-2*time.Second)); err != nil {
		t.Fatalf("SaveSnapshot meter: %v", err)
	}
	if err := s.SaveSnapshot(snap("Inv", "192.0.2.5", now, invVals), now); err != nil {
		t.Fatalf("SaveSnapshot inv: %v", err)
	}

	migrateMeterDeviceKey(s, nil, now)

	// current: поле счётчика под новым ключом, старого IP нет.
	h, err := s.rdb.HGetAll(s.ctx, redisCurrentKey).Result()
	if err != nil {
		t.Fatalf("HGETALL: %v", err)
	}
	if _, ok := h[meterDeviceKey]; !ok {
		t.Fatalf("current: нет поля %q: %v", meterDeviceKey, h)
	}
	if _, ok := h["192.0.2.77"]; ok {
		t.Fatalf("current: старое поле счётчика не удалено: %v", h)
	}
	if _, ok := h["192.0.2.5"]; !ok {
		t.Fatalf("current: инвертор потерян: %v", h)
	}

	// Ряд: точка счётчика переписана на новый ключ, инвертор не тронут.
	got, err := s.QuerySeries(now.Add(-time.Hour), now)
	if err != nil {
		t.Fatalf("QuerySeries: %v", err)
	}
	var meterSeen, invSeen bool
	for _, sn := range got {
		if isMeterDevice(sn.Values) {
			meterSeen = true
			if sn.IP != meterDeviceKey {
				t.Errorf("ряд: счётчик под IP %q, want %q", sn.IP, meterDeviceKey)
			}
		} else if sn.IP == "192.0.2.5" {
			invSeen = true
		}
	}
	if !meterSeen || !invSeen {
		t.Fatalf("ряд: meterSeen=%v invSeen=%v (got %d точек)", meterSeen, invSeen, len(got))
	}

	// Маркер выставлен; повторный вызов — no-op (идемпотентность).
	migrateMeterDeviceKey(s, nil, now)
	if n, _ := s.rdb.HLen(s.ctx, redisCurrentKey).Result(); n != 2 {
		t.Fatalf("после повторной миграции полей current=%d, want 2", n)
	}
}
