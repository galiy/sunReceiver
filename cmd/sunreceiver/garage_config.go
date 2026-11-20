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
	"fmt"
	"strings"
	"time"
)

// garageSection — конфигурация модуля управления гаражом (раздел "garage"
// sunReceiver.json): 4-канальное TCP-реле relay4 (ESP-01S + STM8S103). Все поля
// обязательны (как у раздела poll): отсутствие/ноль — ошибка загрузки конфига.
// Пороговые значения и гистерезисы задаются только здесь (решение оператора).
type garageSection struct {
	Disabled *bool  `json:"disabled"`
	IP       string `json:"ip"`
	Port     int    `json:"port"`

	PollIntervalSec      int `json:"poll_interval_sec"`
	ReconcileIntervalSec int `json:"reconcile_interval_sec"`
	TCPKeepaliveSec      int `json:"tcp_keepalive_sec"`
	MonitorIntervalSec   int `json:"monitor_interval_sec"`

	InverterStaleSec int     `json:"inverter_stale_sec"`
	PVOnVoltage      float64 `json:"pv_on_voltage"`
	Relay1HoldSec    int     `json:"relay1_hold_sec"`
	Relay1OffHoldSec int     `json:"relay1_off_hold_sec"`

	MeterStaleSec       int     `json:"meter_stale_sec"`
	MeterStaleGraceSec  int     `json:"meter_stale_grace_sec"`
	PhaseFaultVoltage   float64 `json:"phase_fault_voltage"`
	PhaseRecoverSec     int     `json:"phase_recover_sec"`
	StateGraceSec       int     `json:"state_grace_sec"`
	HostReadyTimeoutSec int     `json:"host_ready_timeout_sec"`

	DirectVoltageMin    float64 `json:"direct_voltage_min"`
	DirectVoltageMax    float64 `json:"direct_voltage_max"`
	DirectReturnMin     float64 `json:"direct_return_min"`
	DirectReturnMax     float64 `json:"direct_return_max"`
	DirectReturnHoldSec int     `json:"direct_return_hold_sec"`

	// Гистерезис перехода на стабилизатор при превышении (максимальная фаза):
	// выдержки для порогов 235/240 В и порог мгновенного перехода (245 В).
	Overvoltage235HoldSec  int     `json:"overvoltage_235_hold_sec"`
	Overvoltage240HoldSec  int     `json:"overvoltage_240_hold_sec"`
	OvervoltageInstantVolt float64 `json:"overvoltage_instant_voltage"`

	// Гистерезис перехода на стабилизатор при понижении (минимальная фаза):
	// выдержки для порогов 215/210 В и порог мгновенного перехода (205 В).
	Undervoltage215HoldSec  int     `json:"undervoltage_215_hold_sec"`
	Undervoltage210HoldSec  int     `json:"undervoltage_210_hold_sec"`
	UndervoltageInstantVolt float64 `json:"undervoltage_instant_voltage"`
}

// garageConfig — проверенная конфигурация модуля гаража.
type garageConfig struct {
	IP   string
	Port int

	PollInterval      time.Duration
	ReconcileInterval time.Duration
	TCPKeepalive      time.Duration
	MonitorInterval   time.Duration

	InverterStale time.Duration
	PVOnVoltage   float64
	Relay1Hold    time.Duration
	Relay1OffHold time.Duration

	MeterStale        time.Duration
	MeterStaleGrace   time.Duration
	PhaseFaultVoltage float64
	PhaseRecover      time.Duration
	StateGrace        time.Duration
	HostReadyTimeout  time.Duration

	DirectVoltageMin float64
	DirectVoltageMax float64
	DirectReturnMin  float64
	DirectReturnMax  float64
	DirectReturnHold time.Duration

	// Переход на стабилизатор при превышении: выдержки и порог мгновенного.
	Overvoltage235Hold time.Duration
	Overvoltage240Hold time.Duration
	OvervoltageInstant float64

	Undervoltage215Hold time.Duration
	Undervoltage210Hold time.Duration
	UndervoltageInstant float64
}

// Фиксированные пороги выдержек перехода на стабилизатор (решение оператора):
// превышение 235/240 В, мгновенно >245 В; понижение 215/210 В, мгновенно
// <205 В. Имена полей конфига отражают эти пороги.
const (
	garageDirectMinVoltage       = 215
	garageUndervoltage210Voltage = 210
	garageDirectMaxVoltage       = 235
	garageOvervoltage240Voltage  = 240
)

// garageCfg — активная конфигурация модуля гаража (nil — модуль выключен).
// Пакетная переменная по образцу dts017Cfg, чтобы не менять сигнатуру loadConfig.
var garageCfg *garageConfig

// garageFromSection строит *garageConfig из раздела "garage". Возвращает
// (nil, nil), если раздела нет или disabled=true. Любое отсутствующее/нулевое
// поле активного раздела — ошибка (fail-fast).
func garageFromSection(s *garageSection) (*garageConfig, error) {
	if s == nil {
		return nil, nil
	}
	if s.Disabled == nil {
		return nil, fmt.Errorf("в разделе garage не задано обязательное поле disabled (false/true)")
	}
	if *s.Disabled {
		return nil, nil
	}
	ip := strings.TrimSpace(s.IP)
	if ip == "" {
		return nil, fmt.Errorf("раздел garage: не задан ip устройства relay4")
	}
	if s.Port < 1 || s.Port > 65535 {
		return nil, fmt.Errorf("garage.port=%d: должно быть 1..65535", s.Port)
	}
	posInt := func(name string, v int) error {
		if v <= 0 {
			return fmt.Errorf("обязательное поле garage.%s должно быть > 0", name)
		}
		return nil
	}
	for _, f := range []struct {
		name string
		v    int
	}{
		{"poll_interval_sec", s.PollIntervalSec},
		{"reconcile_interval_sec", s.ReconcileIntervalSec},
		{"tcp_keepalive_sec", s.TCPKeepaliveSec},
		{"monitor_interval_sec", s.MonitorIntervalSec},
		{"inverter_stale_sec", s.InverterStaleSec},
		{"relay1_hold_sec", s.Relay1HoldSec},
		{"relay1_off_hold_sec", s.Relay1OffHoldSec},
		{"meter_stale_sec", s.MeterStaleSec},
		{"meter_stale_grace_sec", s.MeterStaleGraceSec},
		{"phase_recover_sec", s.PhaseRecoverSec},
		{"state_grace_sec", s.StateGraceSec},
		{"host_ready_timeout_sec", s.HostReadyTimeoutSec},
		{"direct_return_hold_sec", s.DirectReturnHoldSec},
		{"overvoltage_235_hold_sec", s.Overvoltage235HoldSec},
		{"overvoltage_240_hold_sec", s.Overvoltage240HoldSec},
		{"undervoltage_215_hold_sec", s.Undervoltage215HoldSec},
		{"undervoltage_210_hold_sec", s.Undervoltage210HoldSec},
	} {
		if err := posInt(f.name, f.v); err != nil {
			return nil, err
		}
	}
	// Запас к таймауту аренды (300 с): такт может растянуться на ошибках связи/PG.
	if s.MonitorIntervalSec > 240 {
		return nil, fmt.Errorf("garage.monitor_interval_sec=%d: должен быть ≤ 240 (запас к таймауту аренды 300 с)", s.MonitorIntervalSec)
	}
	if s.TCPKeepaliveSec >= 60 {
		return nil, fmt.Errorf("garage.tcp_keepalive_sec=%d: должен быть < 60 (idle-таймаут устройства)", s.TCPKeepaliveSec)
	}
	if s.UndervoltageInstantVolt <= 0 {
		return nil, fmt.Errorf("garage.undervoltage_instant_voltage: должно быть > 0")
	}
	if s.PVOnVoltage <= 0 {
		return nil, fmt.Errorf("garage.pv_on_voltage: должно быть > 0")
	}
	if s.PhaseFaultVoltage <= 0 {
		return nil, fmt.Errorf("garage.phase_fault_voltage: должно быть > 0")
	}
	if s.DirectVoltageMin <= 0 || s.DirectVoltageMax <= 0 || s.DirectReturnMin <= 0 || s.DirectReturnMax <= 0 {
		return nil, fmt.Errorf("garage.direct_voltage_* / garage.direct_return_*: должны быть > 0")
	}
	if s.DirectVoltageMin >= s.DirectVoltageMax {
		return nil, fmt.Errorf("garage.direct_voltage_min должно быть меньше direct_voltage_max")
	}
	if s.DirectReturnMin >= s.DirectReturnMax {
		return nil, fmt.Errorf("garage.direct_return_min должно быть меньше direct_return_max")
	}
	if s.DirectReturnMin < s.DirectVoltageMin || s.DirectReturnMax > s.DirectVoltageMax {
		return nil, fmt.Errorf("рамки garage.direct_return_* должны лежать внутри direct_voltage_*")
	}
	// Пороги выдержек фиксированы решением оператора и зашиты в имена полей
	// (235/240 и 215/210 В); рабочая зона прямой подачи должна совпадать
	// с крайними порогами, иначе конфиг противоречит логике.
	if s.DirectVoltageMin != garageDirectMinVoltage || s.DirectVoltageMax != garageDirectMaxVoltage {
		return nil, fmt.Errorf("garage.direct_voltage_min/max должны быть %v/%v В (фиксированные пороги выдержек)",
			garageDirectMinVoltage, garageDirectMaxVoltage)
	}
	if s.OvervoltageInstantVolt <= garageOvervoltage240Voltage {
		return nil, fmt.Errorf("garage.overvoltage_instant_voltage должно быть больше %v В", garageOvervoltage240Voltage)
	}
	if s.Overvoltage235HoldSec <= s.Overvoltage240HoldSec {
		return nil, fmt.Errorf("выдержки garage.overvoltage_*_hold_sec должны убывать: 235 В > 240 В")
	}
	if s.UndervoltageInstantVolt >= garageUndervoltage210Voltage {
		return nil, fmt.Errorf("garage.undervoltage_instant_voltage должно быть меньше %v В (ниже выдержки <210)", garageUndervoltage210Voltage)
	}
	if s.Undervoltage215HoldSec <= s.Undervoltage210HoldSec {
		return nil, fmt.Errorf("выдержки garage.undervoltage_*_hold_sec должны убывать: 215 В > 210 В")
	}
	return &garageConfig{
		IP:                  ip,
		Port:                s.Port,
		PollInterval:        time.Duration(s.PollIntervalSec) * time.Second,
		ReconcileInterval:   time.Duration(s.ReconcileIntervalSec) * time.Second,
		TCPKeepalive:        time.Duration(s.TCPKeepaliveSec) * time.Second,
		MonitorInterval:     time.Duration(s.MonitorIntervalSec) * time.Second,
		InverterStale:       time.Duration(s.InverterStaleSec) * time.Second,
		PVOnVoltage:         s.PVOnVoltage,
		Relay1Hold:          time.Duration(s.Relay1HoldSec) * time.Second,
		Relay1OffHold:       time.Duration(s.Relay1OffHoldSec) * time.Second,
		MeterStale:          time.Duration(s.MeterStaleSec) * time.Second,
		MeterStaleGrace:     time.Duration(s.MeterStaleGraceSec) * time.Second,
		PhaseFaultVoltage:   s.PhaseFaultVoltage,
		PhaseRecover:        time.Duration(s.PhaseRecoverSec) * time.Second,
		StateGrace:          time.Duration(s.StateGraceSec) * time.Second,
		HostReadyTimeout:    time.Duration(s.HostReadyTimeoutSec) * time.Second,
		DirectVoltageMin:    s.DirectVoltageMin,
		DirectVoltageMax:    s.DirectVoltageMax,
		DirectReturnMin:     s.DirectReturnMin,
		DirectReturnMax:     s.DirectReturnMax,
		DirectReturnHold:    time.Duration(s.DirectReturnHoldSec) * time.Second,
		Overvoltage235Hold:  time.Duration(s.Overvoltage235HoldSec) * time.Second,
		Overvoltage240Hold:  time.Duration(s.Overvoltage240HoldSec) * time.Second,
		OvervoltageInstant:  s.OvervoltageInstantVolt,
		Undervoltage215Hold: time.Duration(s.Undervoltage215HoldSec) * time.Second,
		Undervoltage210Hold: time.Duration(s.Undervoltage210HoldSec) * time.Second,
		UndervoltageInstant: s.UndervoltageInstantVolt,
	}, nil
}

// loadGarageConfig проверяет раздел "garage" и заполняет пакетную garageCfg.
func loadGarageConfig(s *garageSection) error {
	cfg, err := garageFromSection(s)
	if err != nil {
		return err
	}
	garageCfg = cfg
	return nil
}

// describeGarageConfig — строка для журнала запуска (пустая — модуль выключен).
func describeGarageConfig(c *garageConfig) string {
	if c == nil {
		return ""
	}
	return fmt.Sprintf("управление гаражом: relay4 %s:%d, цикл %s, сверка %s, аренда %s, гистерезис реле 1 %s",
		c.IP, c.Port, c.PollInterval, c.ReconcileInterval, c.MonitorInterval, c.Relay1Hold)
}
