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

	"tinygo.org/x/bluetooth"
)

// agmSection — раздел "agm" sunReceiver.json: мониторинг свинцово-кислотной
// сборки (AGM) — PZEM-017 по RS485 через удалённый CH340 (usbip) и четыре BK300
// по BLE. Disabled — ОБЯЗАТЕЛЬНОЕ поле (отсутствие = ошибка конфига).
//
// AGM — часть общего сервиса sunReceiver (отдельного демона/юнита нет).
// Документация — docs/modules/agm-monitor.md.
type agmSection struct {
	Disabled     *bool            `json:"disabled"`
	Name         string           `json:"name"`
	PollInterval int              `json:"poll_interval"` // секунды, минимум 1
	PZEM         *agmPZEMSection  `json:"pzem"`
	BK300        *agmBK300Section `json:"bk300"`
}

// agmPZEMSection — параметры PZEM-017: удалённый CH340 (usbip) и поиск порта.
type agmPZEMSection struct {
	// Usbip — адрес удалённого CH340 (host:port usbip-сервера). Демон его только
	// логирует: подключение/переподключение usbip выполняет внешний watchdog
	// (deploy/agm-usbip-watchdog.sh), busid в конфиге не хранится.
	Usbip string `json:"usbip"`
	// PathGlobs — шаблоны локальных кандидатов USB-serial.
	PathGlobs []string `json:"path_globs"`
	// Slave — Modbus-адрес PZEM (1..247), по умолчанию 1.
	Slave int `json:"slave"`
}

// agmBK300Section — четыре BK300 на батареях сборки.
type agmBK300Section struct {
	Devices []agmDeviceSection `json:"devices"`
}

// agmDeviceSection — одно устройство BK300.
type agmDeviceSection struct {
	Name     string `json:"name"`
	Position int    `json:"position"` // 1..4, порядок плюс → минус
	MAC      string `json:"mac"`
	Serial   string `json:"serial"` // 18 печатных ASCII (команда 02 01)
}

// agmDeviceConfig — проверенное устройство BK300.
type agmDeviceConfig struct {
	Name     string
	Position int
	MAC      string
	Serial   string
}

// agmPZEMConfig — проверенные параметры PZEM-017.
type agmPZEMConfig struct {
	Usbip     string
	PathGlobs []string
	Slave     byte
}

// agmConfig — проверенная конфигурация модуля (nil — опрос отключён).
type agmConfig struct {
	Name         string
	PollInterval time.Duration
	PZEM         agmPZEMConfig
	Devices      []agmDeviceConfig // отсортированы по Position
}

// agmCfg — активная конфигурация AGM (заполняется loadConfig; nil — выключено).
// Пакетная переменная по образцу dts017Cfg, чтобы не менять сигнатуру loadConfig.
var agmCfg *agmConfig

// agmDefaultPathGlobs — кандидаты локального порта CH340 (приходит по usbip как
// обычный USB-serial).
var agmDefaultPathGlobs = []string{"/dev/serial/by-id/*", "/dev/ttyUSB*"}

// agmFromSection строит *agmConfig из раздела "agm". (nil, nil) — раздела нет
// или disabled=true. Ошибка — активный, но некорректный раздел.
func agmFromSection(s *agmSection) (*agmConfig, error) {
	if s == nil {
		return nil, nil
	}
	if s.Disabled != nil && *s.Disabled {
		return nil, nil
	}
	name := strings.TrimSpace(s.Name)
	if name == "" {
		name = "AGM"
	}
	interval := s.PollInterval
	if interval == 0 {
		interval = 1
	}
	if interval < 0 {
		return nil, fmt.Errorf("agm.poll_interval=%d: не может быть отрицательным", s.PollInterval)
	}
	if interval > 3600 {
		return nil, fmt.Errorf("agm.poll_interval=%d: больше 3600 с", s.PollInterval)
	}

	if s.PZEM == nil {
		return nil, fmt.Errorf("agm: не задан раздел pzem")
	}
	usbip := strings.TrimSpace(s.PZEM.Usbip)
	if usbip == "" {
		return nil, fmt.Errorf("agm.pzem: не задан обязательный адрес usbip (host:port удалённого CH340)")
	}
	slave := s.PZEM.Slave
	if slave == 0 {
		slave = 1
	}
	if slave < 1 || slave > 247 {
		return nil, fmt.Errorf("agm.pzem.slave=%d вне 1..247", s.PZEM.Slave)
	}
	var globs []string
	for _, g := range s.PZEM.PathGlobs {
		if g = strings.TrimSpace(g); g != "" {
			globs = append(globs, g)
		}
	}
	if len(globs) == 0 {
		globs = agmDefaultPathGlobs
	}

	if s.BK300 == nil || len(s.BK300.Devices) != 4 {
		n := 0
		if s.BK300 != nil {
			n = len(s.BK300.Devices)
		}
		return nil, fmt.Errorf("agm.bk300.devices: нужно ровно 4 устройства, задано %d", n)
	}
	devices := make([]agmDeviceConfig, 0, 4)
	byPos := make([]*agmDeviceConfig, 5)
	seenMAC := map[string]bool{}
	seenSerial := map[string]bool{}
	seenName := map[string]bool{}
	for i, d := range s.BK300.Devices {
		devName := strings.TrimSpace(d.Name)
		if devName == "" {
			return nil, fmt.Errorf("agm.bk300.devices[%d].name пуст", i)
		}
		if seenName[devName] {
			return nil, fmt.Errorf("agm.bk300.devices[%d].name=%q повторяется", i, devName)
		}
		seenName[devName] = true
		if d.Position < 1 || d.Position > 4 {
			return nil, fmt.Errorf("agm.bk300.devices[%d].position=%d: нужно 1..4", i, d.Position)
		}
		if byPos[d.Position] != nil {
			return nil, fmt.Errorf("agm.bk300.devices[%d].position=%d повторяется", i, d.Position)
		}
		addr, err := bluetooth.ParseMAC(d.MAC)
		if err != nil {
			return nil, fmt.Errorf("agm.bk300.devices[%d].mac=%q: %w", i, d.MAC, err)
		}
		macKey := strings.ToUpper(addr.String())
		if seenMAC[macKey] {
			return nil, fmt.Errorf("agm.bk300.devices[%d].mac=%q повторяется", i, d.MAC)
		}
		seenMAC[macKey] = true
		if len(d.Serial) != 18 {
			return nil, fmt.Errorf("agm.bk300.devices[%d].serial: нужно 18 ASCII, задано %d", i, len(d.Serial))
		}
		for _, r := range d.Serial {
			if r < 0x20 || r > 0x7E {
				return nil, fmt.Errorf("agm.bk300.devices[%d].serial: непечатный символ", i)
			}
		}
		if seenSerial[d.Serial] {
			return nil, fmt.Errorf("agm.bk300.devices[%d].serial повторяется", i)
		}
		seenSerial[d.Serial] = true
		byPos[d.Position] = &agmDeviceConfig{
			Name: devName, Position: d.Position, MAC: strings.ToUpper(addr.String()), Serial: d.Serial,
		}
	}
	for pos := 1; pos <= 4; pos++ {
		if byPos[pos] == nil {
			return nil, fmt.Errorf("agm.bk300.devices: нет устройства с position=%d", pos)
		}
		devices = append(devices, *byPos[pos])
	}

	return &agmConfig{
		Name:         name,
		PollInterval: time.Duration(interval) * time.Second,
		PZEM:         agmPZEMConfig{Usbip: usbip, PathGlobs: globs, Slave: byte(slave)},
		Devices:      devices,
	}, nil
}

// loadAGMConfig проверяет раздел "agm" и заполняет agmCfg. Вызывается из
// loadConfig; ошибка конфигурации — fail-fast (как у остальных разделов).
func loadAGMConfig(s *agmSection) error {
	cfg, err := agmFromSection(s)
	if err != nil {
		return err
	}
	agmCfg = cfg
	return nil
}

// describeAGMConfig — строка-описание конфигурации для лога.
func describeAGMConfig(c *agmConfig) string {
	if c == nil {
		return ""
	}
	return fmt.Sprintf("%s: PZEM-017 (usbip %s, slave %d) + 4×BK300, период %s",
		c.Name, c.PZEM.Usbip, c.PZEM.Slave, c.PollInterval)
}
