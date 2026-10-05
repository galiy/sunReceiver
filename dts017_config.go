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

// dts017Section — конфигурация трёхфазного счётчика DTS017M (раздел "dts017m"
// sunReceiver.json). Disabled — ОБЯЗАТЕЛЬНОЕ поле (отсутствие = ошибка конфига).
// Транспорт — Modbus RTU через прозрачный шлюз (protocol="rtu", по умолчанию) либо
// Modbus TCP напрямую (protocol="tcp"). Хранилище счётчика полностью обособлено:
// собственные ключи/ряд Redis и собственные таблицы PostgreSQL (см. dts017_store.go,
// dts017_tariff.go, dts017_accumulator.go) — общего с DDS238 нет.
type dts017Section struct {
	Name         string `json:"name"`
	IP           string `json:"ip"`
	Port         int    `json:"port"`
	Unit         byte   `json:"unit"`
	Protocol     string `json:"protocol"`
	PollInterval int    `json:"poll_interval"` // секунды; по умолчанию 1
	Disabled     *bool  `json:"disabled"`
}

// dts017Config — проверенная конфигурация счётчика DTS017M.
type dts017Config struct {
	Name         string
	IP           string
	Port         int
	Unit         byte
	Protocol     string
	RTU          bool
	PollInterval time.Duration
}

// dts017DeviceKey — единый стабильный идентификатор счётчика DTS017M внутри его
// обособленных хранилищ (поле HASH current, name снимков ряда, name в PG-таблицах).
// Не зависит от адреса/транспорта — смена IP не разрывает историю. Общие ключи и
// таблицы DDS238 счётчик НЕ использует.
const dts017DeviceKey = "dts017m"

// dts017Cfg — активная конфигурация DTS017M (заполняется loadConfig; nil — опрос
// выключен). Пакетная переменная (как mapAPI/notifyCfg), чтобы не менять сигнатуру
// loadConfig и не ломать существующие вызовы/тесты.
var dts017Cfg *dts017Config

// dts017FromSection строит *dts017Config из раздела "dts017m". Возвращает
// (nil, nil), если раздела нет или он disabled=true. Ошибка — активный, но
// некорректный раздел (неполные поля, недопустимый protocol/poll_interval).
func dts017FromSection(s *dts017Section) (*dts017Config, error) {
	if s == nil {
		return nil, nil
	}
	if s.Disabled != nil && *s.Disabled {
		return nil, nil
	}
	name := strings.TrimSpace(s.Name)
	ip := strings.TrimSpace(s.IP)
	if name == "" || ip == "" {
		return nil, fmt.Errorf("раздел dts017m неполный: нужны name и ip")
	}
	port := s.Port
	if port == 0 {
		port = 502
	}
	unit := s.Unit
	if unit == 0 {
		unit = 1
	}
	// Транспорт: по умолчанию "rtu" (прозрачный шлюз, raw Modbus RTU на TCP).
	proto := strings.ToLower(strings.TrimSpace(s.Protocol))
	if proto == "" {
		proto = meterProtoRTU
	}
	if proto != meterProtoTCP && proto != meterProtoRTU {
		return nil, fmt.Errorf("dts017m.protocol=%q: поддерживаются %q и %q", s.Protocol, meterProtoRTU, meterProtoTCP)
	}
	interval := s.PollInterval
	if interval <= 0 {
		interval = 1
	}
	return &dts017Config{
		Name:         name,
		IP:           ip,
		Port:         port,
		Unit:         unit,
		Protocol:     proto,
		RTU:          proto == meterProtoRTU,
		PollInterval: time.Duration(interval) * time.Second,
	}, nil
}

// loadDts017Config проверяет раздел "dts017m" и заполняет пакетную dts017Cfg.
// Вызывается из loadConfig. Ошибка -> опрос DTS017M отключается (cfg=nil), но
// приложение продолжает работу (счётчик — вспомогательное устройство).
func loadDts017Config(s *dts017Section) error {
	cfg, err := dts017FromSection(s)
	if err != nil {
		return err
	}
	dts017Cfg = cfg
	return nil
}

// describeDts017Config возвращает строку-описание конфигурации для лога.
func describeDts017Config(c *dts017Config) string {
	if c == nil {
		return ""
	}
	return fmt.Sprintf("%s (%s:%d, unit %d, %s, период %s)",
		c.Name, c.IP, c.Port, c.Unit, c.Protocol, c.PollInterval)
}
