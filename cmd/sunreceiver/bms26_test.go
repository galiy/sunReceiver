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

	"github.com/galiy/sunReceiver/internal/bmscontract"
)

func TestDecodeBms26Contract(t *testing.T) {
	var tm bmscontract.Telemetry
	tm.CellCount = 4
	tm.TempCount = 2
	tm.Flags = bmscontract.FlagChargeMOS | bmscontract.FlagDischargeMOS
	tm.CellsMV[0], tm.CellsMV[1], tm.CellsMV[2], tm.CellsMV[3] = 3350, 3360, 3345, 3370
	tm.TempsDC[0], tm.TempsDC[1] = 320, 305
	tm.CurrentDA = -2470 // заряд −
	tm.PowerW = -1324
	tm.SocDP = 560
	tm.SohDP = 1000
	tm.CapMAh = 160000
	tm.RemainingMAh = 89829
	tm.RatedMAh = 0
	tm.Cycles = 17
	tm.TotalCV = 5362
	tm.PortCV = 5364
	tm.BalanceMask = 0x5
	tm.Mode = bmscontract.ModeCharge
	tm.Alarm[0] = 0x0A

	s, ok := decodeBms26Contract(bmscontract.Encode(&tm))
	if !ok {
		t.Fatal("декод не удался")
	}
	if s.CellCount != 4 || s.TempCount != 2 {
		t.Fatalf("counts: %d/%d", s.CellCount, s.TempCount)
	}
	ap := func(name string, got, want float64) {
		if d := got - want; d > 1e-9 || d < -1e-9 {
			t.Errorf("%s=%v, want %v", name, got, want)
		}
	}
	ap("cell0", s.CellsV[0], 3.350)
	ap("temp0", s.TemperaturesC[0], 32.0)
	ap("current", s.CurrentA, -24.70)
	ap("power", s.PowerW, -1324)
	ap("soc", s.Soc, 56.0)
	ap("soh", s.Soh, 100.0)
	ap("cap", s.CapacityAh, 160.0)
	ap("remaining", s.RemainingAh, 89.829)
	ap("totalV", s.TotalV, 53.62)
	ap("portV", s.PortV, 53.64)
	if s.Mode != int(bmscontract.ModeCharge) {
		t.Fatalf("mode=%d", s.Mode)
	}
	if s.BalanceMask != 0x5 || s.Cycles != 17 {
		t.Fatalf("mask=%d cycles=%d", s.BalanceMask, s.Cycles)
	}
	if s.AlarmBytes[0] != 0x0A {
		t.Fatalf("alarm[0]=%d", s.AlarmBytes[0])
	}
	if s.MaxCellIdx != 4 || s.MinCellIdx != 3 { // 1-based: max=3370(idx4), min=3345(idx3)
		t.Fatalf("min/max idx: %d/%d", s.MinCellIdx, s.MaxCellIdx)
	}
}

func TestDecodeBms26ContractShort(t *testing.T) {
	if _, ok := decodeBms26Contract([]byte{0, 1, 2}); ok {
		t.Fatal("короткий буфер должен быть отвергнут")
	}
}
