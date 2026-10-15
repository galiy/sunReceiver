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

func TestNewMPPTKeyFromLegacy(t *testing.T) {
	cases := []struct {
		old, want string
		ok        bool
	}{
		{"192.168.0.60#mppt-623", "mppt-623", true},
		{"192.168.0.60#mppt-1097", "mppt-1097", true},
		{"host#mppt2", "mppt-2", true},
		{"mppt-623", "", false},
		{"192.168.0.10", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := newMPPTKeyFromLegacy(c.old)
		if ok != c.ok || got != c.want {
			t.Errorf("newMPPTKeyFromLegacy(%q) = (%q,%v), want (%q,%v)", c.old, got, ok, c.want, c.ok)
		}
	}
}

func TestIsLegacyMPPTKey(t *testing.T) {
	if !isLegacyMPPTKey("192.168.0.60#mppt-623") {
		t.Fatal("старый ключ должен определяться как legacy")
	}
	if isLegacyMPPTKey("mppt-623") || isLegacyMPPTKey("192.168.0.10") {
		t.Fatal("новый ключ/IP не должны считаться legacy")
	}
}
