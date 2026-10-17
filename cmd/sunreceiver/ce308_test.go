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
	"errors"
	"fmt"
	"testing"
	"time"
)

// Данные ответов — из кампании 2026-09-22 (../ce308/docs/READ-RESULTS.md).
func TestParseCE308End(t *testing.T) {
	cases := []struct {
		cmd, resp         string
		day, night, total float64
	}{
		{"END01()", "END01(22.09.26,7345.73572)(189.53313)(7156.20259)", 189.53313, 7156.20259, 7345.73572},
		{"END02()", "END02(22.09.26,10688.65857)(10348.13133)(340.52724)", 10348.13133, 340.52724, 10688.65857},
		{"END03()", "END03(22.09.26,108.12524)(43.34151)(64.78373)", 43.34151, 64.78373, 108.12524},
		{"END04()", "END04(22.09.26,2893.96321)(1889.12944)(1004.83377)", 1889.12944, 1004.83377, 2893.96321},
		// Третий тариф (T3) входит в итог.
		{"END01()", "END01(01.01.26,120)(50)(60)(10)", 50, 60, 120},
		// Счётчик вернул несколько суточных блоков (текущий + прошлые дни) —
		// берём первый (актуальный), дата следующего блока не должна ломать парсинг.
		{"END01()", "\x02END01(24.09.26,7349.30897)(191.29883)(7158.01014)(0.0)\r\n" +
			"END01(23.09.26,7348.28461)(191.13004)(7157.15457)(0.0)\r\n" +
			"END01(22.09.26,7346.41194)(190.10759)(7156.30435)(0.0)\r\n\x00",
			191.29883, 7158.01014, 7349.30897},
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
	// Нечисловой тариф (искажённый кадр) — ошибка, а не молчаливый 0.
	if _, _, _, err := parseCE308End("END01()", "END01(01.01.26,120)(abc)(60)"); err == nil {
		t.Error("expected error for non-numeric tariff, got nil")
	}
}

func TestCEC308ConfigFromSection(t *testing.T) {
	disabled := true
	// Отключён — nil, nil.
	if c, err := ce308ConfigFromSection(&ce308Section{Disabled: &disabled}); err != nil || c != nil {
		t.Errorf("disabled: got (%v,%v), want nil,nil", c, err)
	}
	// Включён, но без mac — ошибка.
	if _, err := ce308ConfigFromSection(&ce308Section{Name: "x", PIN: "000000", Disabled: boolPtr(false)}); err == nil {
		t.Error("expected error when mac missing")
	}
	// Включён, без pin — ошибка.
	if _, err := ce308ConfigFromSection(&ce308Section{Name: "x", MAC: "AA:BB:CC:DD:EE:FF", Disabled: boolPtr(false)}); err == nil {
		t.Error("expected error when pin missing")
	}
	// Полный — ok.
	c, err := ce308ConfigFromSection(&ce308Section{Name: "CE308 #000000000", MAC: "AA:BB:CC:DD:EE:FF", PIN: "000000", Disabled: boolPtr(false)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c == nil || c.Name != "CE308 #000000000" || c.MAC != "AA:BB:CC:DD:EE:FF" || c.PIN != "000000" {
		t.Errorf("bad config: %+v", c)
	}
}

func boolPtr(b bool) *bool { return &b }

func TestCE308IsReadTimeout(t *testing.T) {
	if !isCE308ReadTimeout(fmt.Errorf("POWEQ(): %w", errCE308ReadTimeout)) {
		t.Error("wrapped timeout not classified as timeout")
	}
	if !isCE308ReadTimeout(errCE308ReadTimeout) {
		t.Error("bare timeout not classified as timeout")
	}
	if isCE308ReadTimeout(fmt.Errorf("запись %q: %w", "VOLTA()", errors.New("ошибка записи"))) {
		t.Error("transport error misclassified as timeout")
	}
	if isCE308ReadTimeout(nil) {
		t.Error("nil classified as timeout")
	}
}

func TestCE308Backoff(t *testing.T) {
	d := ce308ReconnectDelay
	if got := ce308Backoff(d); got != 4*time.Second {
		t.Fatalf("ce308Backoff(%v)=%v", d, got)
	}
	if got := ce308Backoff(ce308ReconnectMaxDelay); got != ce308ReconnectMaxDelay {
		t.Fatalf("ce308Backoff(max)=%v", got)
	}
}

func TestCE308EnergyTrigger(t *testing.T) {
	ch := make(chan struct{}, 1)
	setCE308TriggerChan(ch)
	defer setCE308TriggerChan(nil)
	if !triggerCE308EnergySnapshot() {
		t.Fatal("первый сигнал должен быть принят")
	}
	if triggerCE308EnergySnapshot() {
		t.Fatal("второй сигнал при занятой очереди должен быть отвергнут")
	}
	<-ch
	setCE308TriggerChan(nil)
	if triggerCE308EnergySnapshot() {
		t.Fatal("без пулера сигнал должен быть отвергнут")
	}
}
