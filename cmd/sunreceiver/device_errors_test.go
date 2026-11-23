package main

import (
	"testing"
	"time"
)

func TestSplitDeviceErrMsg(t *testing.T) {
	cases := []struct {
		msg   string
		base  string
		count int
	}{
		{"нет связи", "нет связи", 1},
		{"нет связи (повторов за сутки: 1)", "нет связи", 1},
		{"нет связи (повторов за сутки: 17)", "нет связи", 17},
		{"ошибка (скобки) в тексте", "ошибка (скобки) в тексте", 1},
		{"текст (повторов за сутки: x)", "текст (повторов за сутки: x)", 1},
	}
	for _, c := range cases {
		base, n := splitDeviceErrMsg(c.msg)
		if base != c.base || n != c.count {
			t.Fatalf("%q: got (%q,%d), want (%q,%d)", c.msg, base, n, c.base, c.count)
		}
	}
}

func TestDeviceErrDayStartMSK(t *testing.T) {
	ts := time.Date(2026, 11, 23, 0, 30, 0, 0, deviceErrMSK)
	got := deviceErrDayStart(ts)
	want := time.Date(2026, 11, 23, 0, 0, 0, 0, deviceErrMSK).UTC()
	if !got.Equal(want) {
		t.Fatalf("day start = %s, want %s", got, want)
	}
}
