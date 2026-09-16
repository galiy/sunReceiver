package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// meterConfig — конфигурация электросчётчика DDS238. Читается из отдельного
// файла dds238.json рядом с исполняемым файлом (как sunReceiver.json);
// при `go run .` — fallback в CWD. Файл содержит учёт/параметры подключения
// конкретного счётчика, поэтому в git не коммитится; шаблон — dds238.json.sample.
type meterConfig struct {
	Name         string `json:"name"`
	IP           string `json:"ip"`
	Port         int    `json:"port"`
	Unit         byte   `json:"unit"`
	FirstReg     uint16 `json:"first_reg"`
	RegisterCnt  uint16 `json:"register_count"`
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
	return &meterConfig{Name: m.Name, IP: m.IP, Port: m.Port, Unit: m.Unit, FirstReg: 0, RegisterCnt: regCnt}, nil
}

// loadMeterConfig читает и проверяет конфигурацию счётчика. Источники по приоритету:
//   1) раздел "meter" в sunReceiver.json (передаётся из main как meterSection);
//   2) отдельный файл dds238.json рядом с бинарником (обратная совместимость).
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
	return &mc
}

// describeMeterConfig возвращает строку-описание конфигурации счётчика для лога.
func describeMeterConfig(c *meterConfig) string {
	if c == nil {
		return ""
	}
	return fmt.Sprintf("%s (%s:%d, unit %d, regs %d..%d)",
		c.Name, c.IP, c.Port, c.Unit, c.FirstReg, c.FirstReg+c.RegisterCnt-1)
}