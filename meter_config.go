package main

import (
	"encoding/json"
	"fmt"
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
// Значения по умолчанию и валидация — как у loadMeterConfig (файл dds238.json).
func meterFromSection(m *meterSection) *meterConfig {
	if m == nil {
		return nil
	}
	if m.IP == "" || m.Port == 0 || m.Unit == 0 || m.Name == "" {
		return nil
	}
	mc := &meterConfig{
		Name:        m.Name,
		IP:          m.IP,
		Port:        m.Port,
		Unit:        m.Unit,
		FirstReg:    m.FirstReg,
		RegisterCnt: m.RegisterCnt,
	}
	if mc.FirstReg == 0 && mc.RegisterCnt == 0 {
		mc.FirstReg = 0
		mc.RegisterCnt = 27
	}
	if mc.RegisterCnt == 0 {
		return nil
	}
	return mc
}

// loadMeterConfig читает и проверяет конфигурацию счётчика. Источники по приоритету:
//   1) раздел "meter" в sunReceiver.json (передаётся из main как meterSection);
//   2) отдельный файл dds238.json рядом с бинарником (обратная совместимость).
// Если ни там, ни там счётчик не задан полностью — возвращает nil (опрос отключён).
func loadMeterConfig(section *meterSection) *meterConfig {
	if mc := meterFromSection(section); mc != nil {
		return mc
	}
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
	if mc.FirstReg == 0 && mc.RegisterCnt == 0 {
		// Значения по умолчанию: полный блок 27 регистров с адреса 0 (как в dds238read.py).
		mc.FirstReg = 0
		mc.RegisterCnt = 27
	}
	if mc.RegisterCnt == 0 {
		return nil
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