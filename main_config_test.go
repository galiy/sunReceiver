package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadConfigLoggerSNRequired (п. 2.2): Deye/Sofar без logger_sn — ошибка
// при старте (fatal), а не вечное heartbeat_only с логами на каждый poll.
func TestLoadConfigLoggerSNRequired(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")

	// Deye без logger_sn — ошибка с упоминанием logger_sn.
	if err := os.WriteFile(path, []byte(`{"invertors":[{"ip":"192.168.13.91","name":"D1","type":"deye","disabled":false}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := loadConfig(path); err == nil {
		t.Fatal("ожидали ошибку для Deye без logger_sn")
	} else if !strings.Contains(err.Error(), "logger_sn") {
		t.Fatalf("err=%v, want упоминание logger_sn", err)
	}

	// Sofar без logger_sn — тоже ошибка.
	if err := os.WriteFile(path, []byte(`{"invertors":[{"ip":"192.168.13.76","name":"S1","type":"sofar","disabled":false}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := loadConfig(path); err == nil {
		t.Fatal("ожидали ошибку для Sofar без logger_sn")
	}

	// С logger_sn — валиден.
	if err := os.WriteFile(path, []byte(`{"invertors":[{"ip":"192.168.13.91","name":"D1","type":"deye","disabled":false,"logger_sn":1774265353}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	targets, _, _, _, err := loadConfig(path)
	if err != nil {
		t.Fatalf("валидный конфиг: %v", err)
	}
	if len(targets) != 1 || targets[0].LoggerSN != 1774265353 {
		t.Fatalf("targets=%+v, want 1 с LoggerSN=1774265353", targets)
	}
}

// TestLoadMeterConfigValidation (п. 2.3): неполный раздел meter и first_reg != 0 —
// опрос отключён (nil) БЕЗ fallback на legacy; register_count=0 → дефолт 27.
func TestLoadMeterConfigValidation(t *testing.T) {
	// Неполный раздел (port=0) — nil (опрос отключён, legacy НЕ используется).
	if mc := loadMeterConfig(&meterSection{Name: "M", IP: "192.168.13.77", Port: 0, Unit: 1}); mc != nil {
		t.Fatalf("неполный раздел → mc=%v, want nil", mc)
	}
	// first_reg != 0 — nil (декодер заточен под регистры 0..26).
	if mc := loadMeterConfig(&meterSection{Name: "M", IP: "192.168.13.77", Port: 502, Unit: 1, FirstReg: 5}); mc != nil {
		t.Fatalf("first_reg!=0 → mc=%v, want nil", mc)
	}
	// Валидный с register_count=0 — дефолт 27.
	mc := loadMeterConfig(&meterSection{Name: "M", IP: "192.168.13.77", Port: 502, Unit: 1})
	if mc == nil {
		t.Fatal("валидный раздел → nil")
	}
	if mc.FirstReg != 0 || mc.RegisterCnt != 27 {
		t.Fatalf("FirstReg=%d RegisterCnt=%d, want 0/27", mc.FirstReg, mc.RegisterCnt)
	}
}
