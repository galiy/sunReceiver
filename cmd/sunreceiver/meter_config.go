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
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// Тип протокола опроса счётчика:
//   - meterProtoTCP — Modbus TCP: кадр с MBAP-заголовком (transID/proto/len/unit)
//   - PDU, без CRC. Прямой опрос счётчика;
//   - meterProtoRTU — Modbus RTU поверх TCP: на шину уходит сырой серийный кадр
//     (unit + PDU + CRC16), без MBAP. Так работает прозрачный шлюз (напр. USR-DR164,
//     режим Modbus OFF): TCP-сокет есть, но преобразования TCP↔RTU нет.
const (
	meterProtoTCP = "tcp"
	meterProtoRTU = "rtu"
)

// meterDeviceKey — единый стабильный идентификатор счётчика DDS238 во всех
// хранилищах: поле HASH `sunreceiver:current`, identity снимков временного ряда
// Redis, колонка `ip` в PG `averages`, добор тарифных границ. НЕ зависит от
// адреса/транспорта (прямой Modbus TCP или прозрачный шлюз) — при смене IP
// (напр. .77 → .75) история и агрегаты не распадаются на разные устройства.
const meterDeviceKey = "dds238"

// meterCurrentKey возвращает ключ счётчика в Redis-current ("" если счётчик не
// настроен). Используется потребителями, которым нужен актуальный снимок
// счётчика по ключу (уведомления, лампы-индикаторы).
func meterCurrentKey(c *meterConfig) string {
	if c == nil {
		return ""
	}
	return meterDeviceKey
}

// normalizeMeterProtocol приводит значение поля protocol к каноническому виду.
// Пустая строка = "tcp" (обратная совместимость). Недопустимое значение — ошибка
// (опрос отключается), чтобы опечатка не привела к молчаливому выбору протокола.
func normalizeMeterProtocol(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", meterProtoTCP:
		return meterProtoTCP, nil
	case meterProtoRTU:
		return meterProtoRTU, nil
	default:
		return "", fmt.Errorf("protocol=%q: поддерживаются только %q (Modbus TCP) и %q (Modbus RTU через прозрачный шлюз)", s, meterProtoTCP, meterProtoRTU)
	}
}

// meterConfig — конфигурация электросчётчика DDS238. Читается из отдельного
// файла dds238.json рядом с исполняемым файлом (как sunReceiver.json);
// при `go run .` — fallback в CWD. Файл содержит учёт/параметры подключения
// конкретного счётчика, поэтому в git не коммитится; шаблон — dds238.json.sample.
type meterConfig struct {
	Name        string `json:"name"`
	IP          string `json:"ip"`
	Port        int    `json:"port"`
	Unit        byte   `json:"unit"`
	FirstReg    uint16 `json:"first_reg"`
	RegisterCnt uint16 `json:"register_count"`
	// Protocol — "tcp" (Modbus TCP, по умолчанию) или "rtu" (Modbus RTU поверх
	// TCP, прозрачный шлюз). Пусто трактуется как "tcp".
	Protocol string `json:"protocol"`
}

// meterConfigPath возвращает путь к dds238.json в каталоге исполняемого файла.
func meterConfigPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "dds238.json"
	}
	return filepath.Join(filepath.Dir(exe), "dds238.json")
}

// meterFromSection строит *meterConfig из раздела meter основного sunReceiver.json.
// Валидация:
//   - m == nil (раздела нет) — nil, nil (вызывающий сам решает про fallback);
//   - неполный раздел (ip/port/unit/name) — nil, nil (опрос отключён);
//   - first_reg != 0 — nil, error: декодер заточен под абсолютные регистры 0..26
//     (decodeMeterRegs), при first_reg != 0 маппинг молча неверен;
//   - register_count == 0 — дефолт 27 (полный блок, как в dds238read.py).
func meterFromSection(m *meterSection) (*meterConfig, error) {
	if m == nil {
		return nil, nil
	}
	if m.IP == "" || m.Port == 0 || m.Unit == 0 || m.Name == "" {
		return nil, nil
	}
	if m.FirstReg != 0 {
		return nil, fmt.Errorf("first_reg=%d: декодер заточен под регистры 0..26 (абсолютные адреса) — опрос счётчика отключён", m.FirstReg)
	}
	regCnt := m.RegisterCnt
	if regCnt == 0 {
		regCnt = 27
	}
	if regCnt < 18 {
		return nil, fmt.Errorf("register_count=%d: нужно минимум 18 (декодер читает регистры 0..17); при меньшем уйдут нулевые показания", regCnt)
	}
	proto, err := normalizeMeterProtocol(m.Protocol)
	if err != nil {
		return nil, err
	}
	return &meterConfig{Name: m.Name, IP: m.IP, Port: m.Port, Unit: m.Unit, FirstReg: 0, RegisterCnt: regCnt, Protocol: proto}, nil
}

// loadMeterConfig читает и проверяет конфигурацию счётчика. Источники по приоритету:
//  1. раздел "meter" в sunReceiver.json (передаётся из main как meterSection);
//  2. отдельный файл dds238.json рядом с бинарником (обратная совместимость).
//
// ВАЖНО: если раздел "meter" в конфиге ЕСТЬ (section != nil), но некорректен
// (неполный/first_reg!=0) — опрос отключается с логом, и legacy-файл НЕ
// используется (иначе молча опрашивался бы ДРУГОЙ счётчик с чужим IP). Fallback
// на dds238.json — только когда раздела "meter" в конфиге НЕТ вовсе.
func loadMeterConfig(section *meterSection) *meterConfig {
	if section != nil {
		if section.Disabled != nil && *section.Disabled {
			log.Printf("meter: раздел meter disabled=true — опрос счётчика отключён, legacy-файл НЕ используется")
			return nil
		}
		mc, err := meterFromSection(section)
		if err != nil {
			log.Printf("meter: раздел meter некорректен (%v) — опрос счётчика отключён, legacy-файл НЕ используется", err)
			return nil
		}
		if mc == nil {
			log.Printf("meter: раздел meter в sunReceiver.json неполный — опрос счётчика отключён, legacy-файл НЕ используется")
			return nil
		}
		return mc
	}
	// Раздела "meter" в конфиге нет — fallback на legacy dds238.json.
	path := meterConfigPath()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		// `go run .`: бинарник во временном каталоге go-сборки — ищем в CWD.
		path = "dds238.json"
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var mc meterConfig
	if err := json.Unmarshal(b, &mc); err != nil {
		return nil
	}
	if mc.IP == "" || mc.Port == 0 || mc.Unit == 0 || mc.Name == "" {
		return nil
	}
	if mc.FirstReg != 0 {
		log.Printf("meter: legacy dds238.json с first_reg=%d: декодер заточен под регистры 0..26 — опрос счётчика отключён", mc.FirstReg)
		return nil
	}
	if mc.RegisterCnt == 0 {
		// Значения по умолчанию: полный блок 27 регистров с адреса 0 (как в dds238read.py).
		mc.RegisterCnt = 27
	}
	if mc.RegisterCnt < 18 {
		log.Printf("meter: legacy dds238.json register_count=%d < 18 — опрос счётчика отключён", mc.RegisterCnt)
		return nil
	}
	proto, err := normalizeMeterProtocol(mc.Protocol)
	if err != nil {
		log.Printf("meter: legacy dds238.json некорректен (%v) — опрос счётчика отключён", err)
		return nil
	}
	mc.Protocol = proto
	return &mc
}

// describeMeterConfig возвращает строку-описание конфигурации счётчика для лога.
func describeMeterConfig(c *meterConfig) string {
	if c == nil {
		return ""
	}
	return fmt.Sprintf("%s (%s:%d, unit %d, %s, regs %d..%d)",
		c.Name, c.IP, c.Port, c.Unit, c.Protocol, c.FirstReg, c.FirstReg+c.RegisterCnt-1)
}
