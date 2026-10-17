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

func TestBmsKey(t *testing.T) {
	d := bmsDevice{DeviceName: "AntBms 320 A/h"}
	if bmsKey(d) != "AntBms 320 A/h" {
		t.Fatalf("bmsKey(DeviceName) = %q", bmsKey(d))
	}
	d.Key = "custom-key"
	if bmsKey(d) != "custom-key" {
		t.Fatalf("bmsKey(Key) = %q", bmsKey(d))
	}
}
