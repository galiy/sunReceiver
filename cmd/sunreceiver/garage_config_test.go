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
	"strings"
	"testing"
	"time"
)

func validGarageSection() garageSection {
	f := false
	return garageSection{
		Disabled: &f,
		IP:       "192.0.2.59",
		Port:     5000,

		PollIntervalSec:      1,
		ReconcileIntervalSec: 60,
		TCPKeepaliveSec:      30,
		MonitorIntervalSec:   60,

		InverterStaleSec: 30,
		PVOnVoltage:      100,
		Relay1HoldSec:    300,
		Relay1OffHoldSec: 300,

		MeterStaleSec:       5,
		MeterStaleGraceSec:  10,
		PhaseFaultVoltage:   100,
		PhaseRecoverSec:     30,
		StateGraceSec:       60,
		HostReadyTimeoutSec: 60,

		DirectVoltageMin:    215,
		DirectVoltageMax:    235,
		DirectReturnMin:     218,
		DirectReturnMax:     230,
		DirectReturnHoldSec: 600,

		Overvoltage235HoldSec:  300,
		Overvoltage240HoldSec:  30,
		OvervoltageInstantVolt: 245,

		Undervoltage215HoldSec:  60,
		Undervoltage210HoldSec:  30,
		UndervoltageInstantVolt: 205,
	}
}

func TestGarageConfigDisabled(t *testing.T) {
	on := true
	cfg, err := garageFromSection(&garageSection{Disabled: &on})
	if err != nil || cfg != nil {
		t.Fatalf("disabled=true: cfg=%v err=%v", cfg, err)
	}
	if cfg, err := garageFromSection(nil); err != nil || cfg != nil {
		t.Fatalf("нет раздела: cfg=%v err=%v", cfg, err)
	}
}

func TestGarageConfigDisabledRequired(t *testing.T) {
	s := validGarageSection()
	s.Disabled = nil
	if _, err := garageFromSection(&s); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("ожидали ошибку про disabled, err=%v", err)
	}
}

func TestGarageConfigValid(t *testing.T) {
	s := validGarageSection()
	cfg, err := garageFromSection(&s)
	if err != nil {
		t.Fatal(err)
	}
	if cfg == nil {
		t.Fatal("ожидали конфигурацию")
	}
	if cfg.Overvoltage235Hold != 5*time.Minute {
		t.Fatalf("Overvoltage235Hold=%v", cfg.Overvoltage235Hold)
	}
	if cfg.UndervoltageInstant != 205 {
		t.Fatalf("UndervoltageInstant=%v", cfg.UndervoltageInstant)
	}
	if cfg.MonitorInterval != 60*time.Second {
		t.Fatalf("MonitorInterval=%v, want 60s", cfg.MonitorInterval)
	}
	if cfg.Relay1OffHold != 5*time.Minute {
		t.Fatalf("Relay1OffHold=%v, want 5m", cfg.Relay1OffHold)
	}
	if cfg.Overvoltage240Hold != 30*time.Second || cfg.Undervoltage210Hold != 30*time.Second {
		t.Fatalf("hold wiring: ov240=%v uv210=%v", cfg.Overvoltage240Hold, cfg.Undervoltage210Hold)
	}
	if cfg.HostReadyTimeout != 60*time.Second {
		t.Fatalf("HostReadyTimeout=%v", cfg.HostReadyTimeout)
	}
	if cfg.MeterStaleGrace != 10*time.Second {
		t.Fatalf("MeterStaleGrace=%v", cfg.MeterStaleGrace)
	}
}

// Валидация: мгновенный порог понижения должен быть ниже тира <210.
func TestGarageConfigRejectsInstantInTier(t *testing.T) {
	s := validGarageSection()
	s.UndervoltageInstantVolt = 212
	if _, err := garageFromSection(&s); err == nil {
		t.Fatal("ожидали ошибку: undervoltage_instant_voltage должен быть < 210")
	}
}

func TestGarageConfigRejectsZeroInstant(t *testing.T) {
	s := validGarageSection()
	s.UndervoltageInstantVolt = 0
	if _, err := garageFromSection(&s); err == nil {
		t.Fatal("ожидали ошибку для undervoltage_instant_voltage=0")
	}
	s = validGarageSection()
	s.OvervoltageInstantVolt = 0
	if _, err := garageFromSection(&s); err == nil {
		t.Fatal("ожидали ошибку для overvoltage_instant_voltage=0")
	}
}

func TestGarageConfigRejectsNonMonotonicHolds(t *testing.T) {
	s := validGarageSection()
	s.Overvoltage235HoldSec = 30
	if _, err := garageFromSection(&s); err == nil {
		t.Fatal("ожидали ошибку для неубывающих выдержек перенапряжения")
	}
	s = validGarageSection()
	s.Undervoltage215HoldSec = 5
	if _, err := garageFromSection(&s); err == nil {
		t.Fatal("ожидали ошибку для неубывающих выдержек понижения")
	}
}

func TestGarageConfigRejectsDirectWindowMismatch(t *testing.T) {
	s := validGarageSection()
	s.DirectVoltageMax = 240
	if _, err := garageFromSection(&s); err == nil {
		t.Fatal("ожидали ошибку: direct_voltage_max должен быть 235")
	}
	s = validGarageSection()
	s.OvervoltageInstantVolt = 240
	if _, err := garageFromSection(&s); err == nil {
		t.Fatal("ожидали ошибку: мгновенный порог должен быть > 240")
	}
}

func TestGarageConfigMonitorInterval(t *testing.T) {
	s := validGarageSection()
	s.MonitorIntervalSec = 0
	if _, err := garageFromSection(&s); err == nil {
		t.Fatal("ожидали ошибку для monitor_interval_sec=0")
	}
	s = validGarageSection()
	s.MonitorIntervalSec = 241
	if _, err := garageFromSection(&s); err == nil {
		t.Fatal("ожидали ошибку для monitor_interval_sec=241 (запас к аренде 300 с)")
	}
	s = validGarageSection()
	s.MonitorIntervalSec = 240
	if _, err := garageFromSection(&s); err != nil {
		t.Fatalf("240 с должен допускаться: %v", err)
	}
}

func TestGarageConfigRejectsMissingField(t *testing.T) {
	s := validGarageSection()
	s.MeterStaleSec = 0
	if _, err := garageFromSection(&s); err == nil {
		t.Fatal("ожидали ошибку для meter_stale_sec=0")
	}
}

// Связка «секция конфига → garageConfig → решатель»: дебаунс отключения
// реле 1 реально равен relay1_off_hold_sec (ловит пропущенную проводку поля).
func TestGarageSectionToDeciderDebounce(t *testing.T) {
	s := validGarageSection()
	cfg, err := garageFromSection(&s)
	if err != nil {
		t.Fatal(err)
	}
	d := newGarageDecider(cfg)
	now := time.Now()
	inv := []garageInverterInput{{Name: "D1", Fresh: true, PV1: 10}}
	target, _ := d.decide(now, relaysOn(4), freshMeter(230, 230, 230), inv)
	if target[0] {
		t.Fatalf("раньше 5 минут реле 1 не должно включаться: %v", target)
	}
	target, _ = d.decide(now.Add(299*time.Second), relaysOn(4), freshMeter(230, 230, 230), inv)
	if target[0] {
		t.Fatalf("на 299-й секунде ещё не отключаем: %v", target)
	}
	target, _ = d.decide(now.Add(301*time.Second), relaysOn(4), freshMeter(230, 230, 230), inv)
	if !target[0] {
		t.Fatalf("после 5 минут без PV — отключаем: %v", target)
	}
}
