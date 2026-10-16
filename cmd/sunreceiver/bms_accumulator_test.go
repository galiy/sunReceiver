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
	"encoding/json"
	"testing"
)

// TestBmsAveragedSamplesJSON: поле samples всегда сериализуется в jsonb values и
// читается обратно как int. На это поле опирается sample-count guard в
// InsertBMSAveraged (pg_store.go) — сравнение (values->>'samples')::int: если бы
// samples терялось при marshal, guard не смог бы отличить более полную запись.
func TestBmsAveragedSamplesJSON(t *testing.T) {
	avg := bmsAveraged{CurrentA: 1.5, PowerW: 51.2, Soc: 55, CellCount: 16, Samples: 120}
	b, err := json.Marshal(avg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// SQL-гвард читает values->>'samples' как jsonb-число: проверяем, что поле
	// присутствует и парсится в int.
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}
	raw, ok := m["samples"]
	if !ok {
		t.Fatalf("samples не сериализуется в values: %s", b)
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil || n != 120 {
		t.Fatalf("samples=%s → %d, want 120", raw, n)
	}
	var back bmsAveraged
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Samples != 120 || back.CurrentA != 1.5 {
		t.Fatalf("roundtrip: %+v", back)
	}
}

// TestBmsAccumulatorNameCollision: две одинаковые батареи (same DeviceName) с
// разными USB-портами дают РАЗНЫЕ ключи бакетов и, соответственно, разные
// 5-минутные точки (V1) — иначе они сливались бы в одну.
