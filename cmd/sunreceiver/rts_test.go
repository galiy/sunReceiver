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

// tsGroupSnapshots собирает снимки из per-tag серий: должен проставить Kind,
// InverterSN (для инверторов), DeviceSN и склеить теги по (device, ts).
func TestTSGroupSnapshots(t *testing.T) {
	lbl := func(tag string) map[string]string {
		return map[string]string{"kind": "deye", "device": "SN1", "name": "N", "ip": "192.0.2.1", "device_sn": "LOG1", "tag": tag}
	}
	out := tsGroupSnapshots([]tsSeries{
		{Labels: lbl("ac_active_power"), Points: []tsPoint{{TS: 1700000000000, V: 5}}},
		{Labels: lbl("l1_voltage"), Points: []tsPoint{{TS: 1700000000000, V: 230}}},
	})
	if len(out) != 1 {
		t.Fatalf("снимков=%d, want 1 (теги одного устройства/ts)", len(out))
	}
	sn := out[0]
	if sn.Kind != "deye" {
		t.Errorf("Kind=%q, want deye", sn.Kind)
	}
	if sn.InverterSN != "SN1" {
		t.Errorf("InverterSN=%q, want SN1", sn.InverterSN)
	}
	if sn.DeviceSN != "LOG1" {
		t.Errorf("DeviceSN=%q, want LOG1", sn.DeviceSN)
	}
	if sn.Name != "N" || sn.IP != "192.0.2.1" {
		t.Errorf("Name/IP=%q/%q", sn.Name, sn.IP)
	}
	if len(sn.Values) != 2 || sn.Values["ac_active_power"].(float64) != 5 {
		t.Errorf("Values=%+v", sn.Values)
	}
}

// Разные устройства на одном ts -> два снимка; отсутствие метки device_sn не ломает сборку.
func TestTSGroupSnapshotsTwoDevices(t *testing.T) {
	mk := func(dev, tag string, v float64, withSN bool) tsSeries {
		l := map[string]string{"kind": "deye", "device": dev, "name": dev, "ip": "192.0.2.1", "tag": tag}
		if withSN {
			l["device_sn"] = "LOG"
		}
		return tsSeries{Labels: l, Points: []tsPoint{{TS: 1700000000000, V: v}}}
	}
	out := tsGroupSnapshots([]tsSeries{
		mk("A", "ac_active_power", 1, true),
		mk("B", "ac_active_power", 2, false),
	})
	if len(out) != 2 {
		t.Fatalf("снимков=%d, want 2 (разные device)", len(out))
	}
	for _, sn := range out {
		if sn.Kind != "deye" {
			t.Errorf("kind=%q", sn.Kind)
		}
	}
}
