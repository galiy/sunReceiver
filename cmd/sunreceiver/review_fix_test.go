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

import "testing"

func TestNormalizeInverterSN(t *testing.T) {
	cases := []struct{ kind, in, want string }{
		{"sofar", "TEST-SOFAR-1055", "TEST-SOFAR-1055"},
		{"sofar", "TEST-SOFAR-1055V480", "TEST-SOFAR-1055"},
		{"sofar", "TEST-SOFAR-1055V480V100", "TEST-SOFAR-1055"}, // ≥2 хвостовых группы
		{"sofar", "V480", "V480"},                               // одна буква+цифры — не трогаем (i<=0)
		{"deye", "TEST-SOFAR-1055V480", "TEST-SOFAR-1055V480"},  // не sofar — как есть
		{"sofar", "", ""},
		{"sofar", "ABC", "ABC"},
	}
	for _, c := range cases {
		if got := normalizeInverterSN(c.kind, c.in); got != c.want {
			t.Errorf("normalizeInverterSN(%q,%q)=%q, want %q", c.kind, c.in, got, c.want)
		}
	}
	// Идемпотентность: повторная нормализация не меняет результат.
	for _, c := range cases {
		if got := normalizeInverterSN(c.kind, normalizeInverterSN(c.kind, c.in)); got != normalizeInverterSN(c.kind, c.in) {
			t.Errorf("normalizeInverterSN не идемпотентна для %q: %q", c.in, got)
		}
	}
}

func TestAccBucketKeyOf(t *testing.T) {
	// Инвертор: префикс марки (защита от совпадения серийников deye/sofar).
	if got := accBucketKeyOf(deviceSnapshot{Kind: "deye", InverterSN: "SN1"}); got != "deye\x00SN1" {
		t.Errorf("deye: got %q, want %q", got, "deye\x00SN1")
	}
	if got := accBucketKeyOf(deviceSnapshot{Kind: "sofar", InverterSN: "SN1"}); got != "sofar\x00SN1" {
		t.Errorf("sofar: got %q, want %q", got, "sofar\x00SN1")
	}
	// Обособленные: без префикса — живой Kind=kes и восстановленный Kind="" дают
	// ОДИН ключ (иначе один аппарат расщеплялся бы на две группы).
	if got := accBucketKeyOf(deviceSnapshot{Kind: "kes", IP: "mppt-1"}); got != "mppt-1" {
		t.Errorf("kes: got %q, want %q", got, "mppt-1")
	}
	if got := accBucketKeyOf(deviceSnapshot{Kind: "", IP: "mppt-1"}); got != "mppt-1" {
		t.Errorf("kes restored: got %q, want %q", got, "mppt-1")
	}
	if got := accBucketKeyOf(deviceSnapshot{Kind: "", IP: "dds238"}); got != "dds238" {
		t.Errorf("dds238: got %q, want %q", got, "dds238")
	}
}

// TestGroupInverterSeries — регресс на дубль ряда: два устройства с одинаковым
// именем, но разными ключами (серийниками), должны дать ДВА ряда (а не один,
// выведенный дважды); МАП и КЭС исключаются (по тегам/ключу), несмотря на
// наличие ac_active_power.
func TestGroupInverterSeries(t *testing.T) {
	snaps := []deviceSnapshot{
		{Name: "Sofar Test", IP: "192.0.2.76", InverterSN: "A1", Kind: "sofar",
			Timestamp: "2026-10-16T10:00:00+03:00", Values: valuesContract{"ac_active_power": 100.0}},
		{Name: "Sofar Test", IP: "192.0.2.76", InverterSN: "A2", Kind: "sofar",
			Timestamp: "2026-10-16T10:00:00+03:00", Values: valuesContract{"ac_active_power": 100.0}},
		// МАП: маркер isMAPDevice (battery_voltage) + ac_active_power — исключается.
		{Name: "MAP", IP: "192.0.2.74", Kind: "map",
			Timestamp: "2026-10-16T10:00:00+03:00", Values: valuesContract{"battery_voltage": 52.0, "ac_active_power": 5.0}},
		// КЭС: ключ mppt- (isMPPTKey) + ac_active_power — исключается.
		{Name: "MPPT-1", IP: "mppt-1", Kind: "kes",
			Timestamp: "2026-10-16T10:00:00+03:00", Values: valuesContract{"ac_active_power": 7.0}},
	}
	got := groupInverterSeries(snaps)
	if len(got) != 2 {
		t.Fatalf("рядов=%d, want 2 (два ключа + исключение МАП/КЭС): %+v", len(got), got)
	}
	for _, ds := range got {
		if ds.Name != "Sofar Test" {
			t.Errorf("неожиданный ряд %q", ds.Name)
		}
		if len(ds.Points) != 1 {
			t.Errorf("точек=%d, want 1", len(ds.Points))
		}
	}
}
