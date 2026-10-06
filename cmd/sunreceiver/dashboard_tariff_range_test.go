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

// TestParseTariffRange проверяет разбор from/to /api/tariffs (RFC3339,
// дата+время): выборка — по календарным дням в ЛОКАЛЬНОЙ зоне, время внутри
// суток не влияет; пустые/битые параметры — текущий календарный месяц.
func TestParseTariffRange(t *testing.T) {
	loc := time.Local
	now := time.Now()

	s, e := parseTariffRange("", "")
	wantStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)
	if !s.Equal(wantStart) {
		t.Fatalf("default from: got %v, want %v", s, wantStart)
	}
	if !e.Equal(wantStart.AddDate(0, 1, 0)) {
		t.Fatalf("default to: got %v, want %v", e, wantStart.AddDate(0, 1, 0))
	}

	// RFC3339 с чужим оффсетом (UTC): день берётся после перевода в локальную зону.
	for _, in := range []string{"2026-09-05T12:00:00Z", "2026-09-05T00:00:00Z", "2026-09-05T23:59:59Z"} {
		tt, err := time.Parse(time.RFC3339, in)
		if err != nil {
			t.Fatalf("test data: %v", err)
		}
		tl := tt.In(loc)
		wantDay := time.Date(tl.Year(), tl.Month(), tl.Day(), 0, 0, 0, 0, loc)
		s, e = parseTariffRange(in, in)
		if !s.Equal(wantDay) {
			t.Fatalf("from %q: got %v, want %v", in, s, wantDay)
		}
		if !e.Equal(wantDay.AddDate(0, 0, 1)) {
			t.Fatalf("to %q: got %v, want %v", in, e, wantDay.AddDate(0, 0, 1))
		}
	}

	// Ошибка парсинга — как пустой параметр.
	s, e = parseTariffRange("garbage", "")
	if !s.Equal(wantStart) || !e.Equal(wantStart.AddDate(0, 1, 0)) {
		t.Fatalf("bad from: got [%v, %v), want default month", s, e)
	}

	// to раньше from — фолбэк: месяц от from.
	s, e = parseTariffRange("2026-09-10T00:00:00Z", "2026-09-01T00:00:00Z")
	tt, _ := time.Parse(time.RFC3339, "2026-09-10T00:00:00Z")
	tl := tt.In(loc)
	wantFrom := time.Date(tl.Year(), tl.Month(), tl.Day(), 0, 0, 0, 0, loc)
	if !s.Equal(wantFrom) || !e.Equal(wantFrom.AddDate(0, 1, 0)) {
		t.Fatalf("to<from: got [%v, %v), want [%v, %v)", s, e, wantFrom, wantFrom.AddDate(0, 1, 0))
	}
}
