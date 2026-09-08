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

// loadMeterConfig читает и проверяет dds238.json. Если файла нет или поля не
// полностью заданы — возвращает nil (опрос счётчика отключён).
func loadMeterConfig() *meterConfig {
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