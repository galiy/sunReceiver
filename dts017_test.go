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

// putLong пишет 32-битное значение (старшее слово первым) в срез регистров.
func putLong(regs []uint16, off int, v uint32) {
	regs[off] = uint16(v >> 16)
	regs[off+1] = uint16(v & 0xFFFF)
}

// putI16 пишет знаковое 16-битное значение.
func putI16(regs []uint16, off int, v int) {
	regs[off] = uint16(int16(v))
}

func TestDecodeDts017Telemetry(t *testing.T) {
	regs := make([]uint16, dts017TelemetryCount)
	regs[0x00] = 2366 // Ua 236.6
	regs[0x01] = 2395
	regs[0x02] = 2347
	regs[0x03] = 67 // Ia 0.67
	regs[0x04] = 16
	regs[0x05] = 909
	putI16(regs, 0x07, 2215) // ΣP
	putI16(regs, 0x08, -30)  // Pa знаковое
	putI16(regs, 0x0B, -50)  // ΣQ
	regs[0x0F] = 2332        // ΣS
	regs[0x13] = 954         // cosφ 0.954
	regs[0x17] = 4100        // Uab 410.0
	regs[0x1A] = 4998        // Fa 49.98
	v := decodeDts017Telemetry(regs)
	checks := map[string]float64{
		dts017VoltageA:      236.6,
		dts017CurrentA:      0.67,
		dts017CurrentC:      9.09,
		dts017ActivePower:   2215,
		dts017ActivePowerA:  -30,
		dts017ReactivePower: -50,
		dts017ApparentPower: 2332,
		dts017PowerFactor:   0.954,
		dts017VoltageAB:     410,
		dts017FrequencyA:    49.98,
	}
	for k, want := range checks {
		if got := v[k]; got != want {
			t.Errorf("%s = %v, want %v", k, got, want)
		}
	}
}

func TestDecodeDts017Energy(t *testing.T) {
	regs := make([]uint16, dts017EnergyCount)
	putLong(regs, dts017OffTotalActive, 1827649)   // 18276.49
	putLong(regs, dts017OffImportActive, 732794)   // 7327.94
	putLong(regs, dts017OffExportActive, 1094855)  // 10948.55
	putLong(regs, dts017OffTotalReactive, 280965)  // 2809.65
	putLong(regs, dts017OffImportReactive, 100000) // 1000.00
	putLong(regs, dts017OffExportReactive, 200000) // 2000.00
	en, r := decodeDts017Energy(regs)
	if en[dts017Total] != 18276.49 {
		t.Errorf("total = %v, want 18276.49", en[dts017Total])
	}
	if en[dts017Import] != 7327.94 {
		t.Errorf("import = %v, want 7327.94", en[dts017Import])
	}
	if en[dts017Export] != 10948.55 {
		t.Errorf("export = %v, want 10948.55", en[dts017Export])
	}
	if en[dts017ReactiveTotal] != 2809.65 {
		t.Errorf("reactive total = %v, want 2809.65", en[dts017ReactiveTotal])
	}
	if r.Import != 7327.94 || r.Export != 10948.55 {
		t.Errorf("readings = %+v", r)
	}
	// Целостность: прямая + обратная = общая (для активной энергии).
	if diff := en[dts017Import] + en[dts017Export] - en[dts017Total]; diff > 1e-6 || diff < -1e-6 {
		t.Errorf("import+export != total: %v + %v != %v",
			en[dts017Import], en[dts017Export], en[dts017Total])
	}
}

func TestAverageDts017CumulativeLast(t *testing.T) {
	t0 := time.Date(2026, 10, 5, 12, 0, 0, 0, time.Local)
	s1 := dts017Snapshot{Timestamp: t0.Format(time.RFC3339), Values: map[string]float64{
		dts017Import: 10, dts017VoltageA: 100,
	}}
	s2 := dts017Snapshot{Timestamp: t0.Add(time.Second).Format(time.RFC3339), Values: map[string]float64{
		dts017Import: 12, dts017VoltageA: 104,
	}}
	out := averageDts017([]dts017Snapshot{s1, s2})
	if out[dts017Import] != 12 {
		t.Errorf("cumulative import = %v, want last 12", out[dts017Import])
	}
	if out[dts017VoltageA] != 102 {
		t.Errorf("voltage avg = %v, want 102", out[dts017VoltageA])
	}
}

func TestDts017BcdRoundTrip(t *testing.T) {
	want := time.Date(2026, 10, 3, 17, 25, 17, 0, time.Local)
	regs := dts017BcdEncode(want)
	// Кадр из документации: 2026-10-03 17:25:17.
	if regs[0] != 0x1725 || regs[1] != 0x1703 || regs[2] != 0x1026 {
		t.Fatalf("encode = %04X %04X %04X", regs[0], regs[1], regs[2])
	}
	got, ok := dts017BcdDecode(regs)
	if !ok || !got.Equal(want) {
		t.Fatalf("decode = %v (ok=%v), want %v", got, ok, want)
	}
}

func TestDts017BcdDecodeInvalid(t *testing.T) {
	// Не-BCD ниббл.
	if _, ok := dts017BcdDecode([]uint16{0x1A25, 0x1703, 0x1026}); ok {
		t.Errorf("не-BCD должен быть отклонён")
	}
	// Недопустимый месяц (BCD 13).
	if _, ok := dts017BcdDecode([]uint16{0x0000, 0x0001, 0x1301}); ok {
		t.Errorf("месяц 13 должен быть отклонён")
	}
	// Недопустимый день (00).
	if _, ok := dts017BcdDecode([]uint16{0x0000, 0x0000, 0x0126}); ok {
		t.Errorf("день 0 должен быть отклонён")
	}
	// Короткий блок.
	if _, ok := dts017BcdDecode([]uint16{0x1725, 0x1703}); ok {
		t.Errorf("короткий блок должен быть отклонён")
	}
}

func TestDts017FromSection(t *testing.T) {
	// Секция отсутствует -> выключено.
	if c, err := dts017FromSection(nil); err != nil || c != nil {
		t.Fatalf("nil section: c=%v err=%v", c, err)
	}
	// disabled=true -> выключено.
	tr := true
	if c, err := dts017FromSection(&dts017Section{Disabled: &tr}); err != nil || c != nil {
		t.Fatalf("disabled: c=%v err=%v", c, err)
	}
	// Активная: дефолты port/unit/protocol/poll_interval.
	fa := false
	c, err := dts017FromSection(&dts017Section{Name: "DTS", IP: "192.168.0.41", Disabled: &fa})
	if err != nil {
		t.Fatalf("valid: %v", err)
	}
	if c.Port != 502 || c.Unit != 1 || c.Protocol != meterProtoRTU || !c.RTU || c.PollInterval != time.Second {
		t.Errorf("defaults: %+v", c)
	}
	// Недопустимый protocol.
	if _, err := dts017FromSection(&dts017Section{Name: "DTS", IP: "x", Protocol: "bogus", Disabled: &fa}); err == nil {
		t.Errorf("bogus protocol: expected error")
	}
	// Неполная активная секция.
	if _, err := dts017FromSection(&dts017Section{Disabled: &fa}); err == nil {
		t.Errorf("incomplete: expected error")
	}
}
