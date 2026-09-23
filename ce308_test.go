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
	"math"
	"testing"
)

// Данные ответов — из кампании 2026-09-22 (../ce308/docs/READ-RESULTS.md).
func TestValuesCE308(t *testing.T) {
	reads := ce308Reads{
		Volta:     []float64{240.186, 239.058, 238.953},
		Curre:     []float64{0.77701, 0.81489, 0.25237},
		ActiveP:   toWatts([]float64{-0.161307, -0.165584, 0.036134, 0.363025}),
		ReactiveP: toWatts([]float64{-0.087436, -0.09889, 0.043991, 0.230317}),
	}
	v := valuesCE308(reads)
	if got := v[ce308L1Voltage]; got != 240.2 {
		t.Errorf("ce308_l1_voltage = %v, want 240.2", got)
	}
	if got := v[ce308L3Current]; got != 0.3 {
		t.Errorf("ce308_l3_current = %v, want 0.3", got)
	}
	// Мощность: кВт → Вт, округление до 1 знака.
	if got := v[ce308L1ActiveP]; got != -161.3 {
		t.Errorf("ce308_l1_active_power = %v, want -161.3", got)
	}
	if got := v[ce308ActiveP]; got != 363.0 {
		t.Errorf("ce308_active_power = %v, want 363.0", got)
	}
	if got := v[ce308L3ReactP]; got != 44.0 {
		t.Errorf("ce308_l3_reactive_power = %v, want 44.0", got)
	}
	if got := v[ce308ReactP]; got != 230.3 {
		t.Errorf("ce308_reactive_power = %v, want 230.3", got)
	}
}

func TestParseCE308End(t *testing.T) {
	cases := []struct {
		cmd, resp         string
		day, night, total float64
	}{
		{"END01()", "END01(22.09.26,7345.73572)(189.53313)(7156.20259)", 189.53313, 7156.20259, 7345.73572},
		{"END02()", "END02(22.09.26,10688.65857)(10348.13133)(340.52724)", 10348.13133, 340.52724, 10688.65857},
		{"END03()", "END03(22.09.26,108.12524)(43.34151)(64.78373)", 43.34151, 64.78373, 108.12524},
		{"END04()", "END04(22.09.26,2893.96321)(1889.12944)(1004.83377)", 1889.12944, 1004.83377, 2893.96321},
	}
	for _, c := range cases {
		day, night, total, err := parseCE308End(c.cmd, c.resp)
		if err != nil {
			t.Fatalf("%s: %v", c.cmd, err)
		}
		if day != c.day || night != c.night || total != c.total {
			t.Errorf("%s: got (%v,%v,%v), want (%v,%v,%v)", c.cmd, day, night, total, c.day, c.night, c.total)
		}
	}
}

func TestParseCE308EndBad(t *testing.T) {
	if _, _, _, err := parseCE308End("END01()", "END01(ERR13)"); err == nil {
		t.Error("expected error for malformed END response, got nil")
	}
}

func TestCEC308ConfigFromSection(t *testing.T) {
	disabled := true
	// Отключён — nil, nil.
	if c, err := ce308ConfigFromSection(&ce308Section{Disabled: &disabled}); err != nil || c != nil {
		t.Errorf("disabled: got (%v,%v), want nil,nil", c, err)
	}
	// Включён, но без mac — ошибка.
	if _, err := ce308ConfigFromSection(&ce308Section{Name: "x", PIN: "324742", Disabled: boolPtr(false)}); err == nil {
		t.Error("expected error when mac missing")
	}
	// Включён, без pin — ошибка.
	if _, err := ce308ConfigFromSection(&ce308Section{Name: "x", MAC: "6C:B2:FD:70:BD:FF", Disabled: boolPtr(false)}); err == nil {
		t.Error("expected error when pin missing")
	}
	// Полный — ok.
	c, err := ce308ConfigFromSection(&ce308Section{Name: "CE308 #194232482", MAC: "6C:B2:FD:70:BD:FF", PIN: "324742", Disabled: boolPtr(false)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c == nil || c.Name != "CE308 #194232482" || c.MAC != "6C:B2:FD:70:BD:FF" || c.PIN != "324742" {
		t.Errorf("bad config: %+v", c)
	}
}

func boolPtr(b bool) *bool { return &b }

func TestCE308ReadsValid(t *testing.T) {
	good := ce308Reads{
		Volta:     []float64{240.186, 239.058, 238.953},
		Curre:     []float64{0.77701, 0.81489, 0.25237},
		ActiveP:   toWatts([]float64{-0.161307, -0.165584, 0.036134, 0.363025}),
		ReactiveP: toWatts([]float64{-0.087436, -0.09889, 0.043991, 0.230317}),
	}
	if !ce308ReadsValid(good) {
		t.Error("valid reads rejected")
	}

	// Недостающие фазы (обрыв кадра).
	short := good
	short.Volta = []float64{240.1, 238.9}
	if ce308ReadsValid(short) {
		t.Error("reads with missing phases accepted")
	}

	// NaN / Inf.
	nan := good
	nan.Curre = []float64{0.7, math.NaN(), 0.25}
	if ce308ReadsValid(nan) {
		t.Error("reads with NaN current accepted")
	}
	inf := good
	inf.ActiveP = []float64{math.Inf(1), 0, 0, 0}
	if ce308ReadsValid(inf) {
		t.Error("reads with Inf power accepted")
	}

	// Выход за физический диапазон.
	over := good
	over.Volta = []float64{900, 239, 238}
	if ce308ReadsValid(over) {
		t.Error("reads with absurd voltage accepted")
	}
	overN := good
	overN.Curre = []float64{-5, 0.8, 0.25}
	if ce308ReadsValid(overN) {
		t.Error("reads with negative current accepted")
	}
}
