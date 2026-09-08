package main

import (
	"testing"
	"time"
)

func TestDecodeMeterRegs(t *testing.T) {
	// Регистры из живого опроса DDS238 (192.168.13.77).
	regs := []uint16{
		0x015A, 0xA467, 0, 0, 0, 0, 0, 0, // 0-7: Total(0-1)
		0x0036, 0x7AB0, // 8-9 Export
		0x0124, 0x29B7, // 10-11 Import
		2368,     // 12 Voltage
		738,      // 13 Current
		0xF970,   // 14 ActivePower (signed -1680)
		0xFE38,   // 15 ReactivePower (signed -456)
		964,      // 16 PF
		4999,     // 17 Freq
		0, 0, 0, 0, 0, 0, 0, 0, 0, // 18-26
	}
	r := decodeMeterRegs(regs)

	if want := 227175.43; abs(r.Total-want) > 0.01 {
		t.Errorf("Total = %v, want ~%v", r.Total, want)
	}
	if want := 35703.52; abs(r.Export-want) > 0.01 {
		t.Errorf("Export = %v, want ~%v", r.Export, want)
	}
	if want := 191471.91; abs(r.Import-want) > 0.01 {
		t.Errorf("Import = %v, want ~%v", r.Import, want)
	}
	if want := 236.8; abs(r.Voltage-want) > 0.01 {
		t.Errorf("Voltage = %v, want %v", r.Voltage, want)
	}
	if want := 7.38; abs(r.Current-want) > 0.01 {
		t.Errorf("Current = %v, want %v", r.Current, want)
	}
	if r.ActiveP != -1680 {
		t.Errorf("ActiveP = %v, want -1680", r.ActiveP)
	}
	if r.ReactP != -456 {
		t.Errorf("ReactP = %v, want -456", r.ReactP)
	}
	if want := 0.964; abs(r.PF-want) > 0.001 {
		t.Errorf("PF = %v, want %v", r.PF, want)
	}
	if want := 49.99; abs(r.Freq-want) > 0.01 {
		t.Errorf("Freq = %v, want %v", r.Freq, want)
	}
}

func TestMeterBoundaryTimes(t *testing.T) {
	loc := time.Local
	cases := []struct {
		now      time.Time
		wantPrev int // час границы
		wantNext int
	}{
		{time.Date(2026, 9, 8, 0, 0, 0, 0, loc), 0, 7},     // ровно 00:00
		{time.Date(2026, 9, 8, 2, 30, 0, 0, loc), 0, 7},    // ночь
		{time.Date(2026, 9, 8, 7, 0, 0, 0, loc), 7, 23},    // ровно 07:00
		{time.Date(2026, 9, 8, 12, 0, 0, 0, loc), 7, 23},   // день
		{time.Date(2026, 9, 8, 23, 0, 0, 0, loc), 23, 0},   // ровно 23:00 (next — завтра 00:00)
		{time.Date(2026, 9, 8, 23, 59, 59, 0, loc), 23, 0}, // конец ночи
	}
	for _, c := range cases {
		prev, next := meterBoundaryTimes(c.now)
		if prev.Hour() != c.wantPrev {
			t.Errorf("now=%v: prev.Hour()=%d, want %d", c.now, prev.Hour(), c.wantPrev)
		}
		if next.Hour() != c.wantNext {
			t.Errorf("now=%v: next.Hour()=%d, want %d", c.now, next.Hour(), c.wantNext)
		}
	}
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}