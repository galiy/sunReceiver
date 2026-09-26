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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLoadConfigLoggerSNRequired (п. 2.2): Deye/Sofar без logger_sn — ошибка
// при старте (fatal), а не вечное heartbeat_only с логами на каждый poll.
func TestLoadConfigLoggerSNRequired(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")

	// Deye без logger_sn — ошибка с упоминанием logger_sn.
	if err := os.WriteFile(path, []byte(`{"dashboard_port":8080,"invertors":[{"ip":"192.0.2.91","name":"D1","type":"deye","disabled":false}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, _, _, _, err := loadConfig(path); err == nil {
		t.Fatal("ожидали ошибку для Deye без logger_sn")
	} else if !strings.Contains(err.Error(), "logger_sn") {
		t.Fatalf("err=%v, want упоминание logger_sn", err)
	}

	// Sofar без logger_sn — тоже ошибка.
	if err := os.WriteFile(path, []byte(`{"dashboard_port":8080,"invertors":[{"ip":"192.0.2.76","name":"S1","type":"sofar","disabled":false}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, _, _, _, err := loadConfig(path); err == nil {
		t.Fatal("ожидали ошибку для Sofar без logger_sn")
	}

	// С logger_sn — валиден.
	if err := os.WriteFile(path, []byte(`{"dashboard_port":8080,"invertors":[{"ip":"192.0.2.91","name":"D1","type":"deye","disabled":false,"logger_sn":1234567890}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	targets, _, _, _, _, _, _, err := loadConfig(path)
	if err != nil {
		t.Fatalf("валидный конфиг: %v", err)
	}
	if len(targets) != 1 || targets[0].LoggerSN != 1234567890 {
		t.Fatalf("targets=%+v, want 1 с LoggerSN=1234567890", targets)
	}
}

// TestLoadMeterConfigValidation (п. 2.3): неполный раздел meter и first_reg != 0 —
// опрос отключён (nil) БЕЗ fallback на legacy; register_count=0 → дефолт 27.
func TestLoadMeterConfigValidation(t *testing.T) {
	// Неполный раздел (port=0) — nil (опрос отключён, legacy НЕ используется).
	if mc := loadMeterConfig(&meterSection{Name: "M", IP: "192.0.2.77", Port: 0, Unit: 1}); mc != nil {
		t.Fatalf("неполный раздел → mc=%v, want nil", mc)
	}
	// first_reg != 0 — nil (декодер заточен под регистры 0..26).
	if mc := loadMeterConfig(&meterSection{Name: "M", IP: "192.0.2.77", Port: 502, Unit: 1, FirstReg: 5}); mc != nil {
		t.Fatalf("first_reg!=0 → mc=%v, want nil", mc)
	}
	// Валидный с register_count=0 — дефолт 27.
	mc := loadMeterConfig(&meterSection{Name: "M", IP: "192.0.2.77", Port: 502, Unit: 1})
	if mc == nil {
		t.Fatal("валидный раздел → nil")
	}
	if mc.FirstReg != 0 || mc.RegisterCnt != 27 {
		t.Fatalf("FirstReg=%d RegisterCnt=%d, want 0/27", mc.FirstReg, mc.RegisterCnt)
	}
}

// TestLoadConfigDashboardPortRequired (п. 4): dashboard_port — обязательное поле;
// без него — ошибка конфига при старте.
func TestLoadConfigDashboardPortRequired(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	if err := os.WriteFile(path, []byte(`{"invertors":[{"ip":"192.0.2.91","name":"D1","type":"deye","disabled":false,"logger_sn":1234567890}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, _, _, _, err := loadConfig(path); err == nil {
		t.Fatal("ожидали ошибку при отсутствии dashboard_port")
	} else if !strings.Contains(err.Error(), "dashboard_port") {
		t.Fatalf("err=%v, want упоминание dashboard_port", err)
	}
}

// TestDefaultPGRestoreWindow — дефолт и разбор из конфига.
func TestDefaultPGRestoreWindow(t *testing.T) {
	if got := defaultPGRestoreWindow(nil); got != 30*24*time.Hour {
		t.Fatalf("nil db → %v, want 30 суток", got)
	}
	if got := defaultPGRestoreWindow(&dbConfig{PGRestoreWindow: "48h"}); got != 48*time.Hour {
		t.Fatalf("48h → %v, want 48ч", got)
	}
	// Некорректная строка — дефолт.
	if got := defaultPGRestoreWindow(&dbConfig{PGRestoreWindow: "abc"}); got != 30*24*time.Hour {
		t.Fatalf("abc → %v, want дефолт", got)
	}
}

// Дублирующийся IP у инверторов недопустим: общий solarman-клиент и общий ключ
// Redis, гонка на одном устройстве.
func TestLoadConfigDuplicateIPRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	body := `{"dashboard_port":8080,"invertors":[` +
		`{"ip":"192.0.2.10","name":"A","type":"deye","disabled":false,"logger_sn":1},` +
		`{"ip":"192.0.2.10","name":"B","type":"deye","disabled":false,"logger_sn":2}]}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, _, _, _, err := loadConfig(path); err == nil {
		t.Fatal("ожидали ошибку для дублирующегося ip")
	} else if !strings.Contains(err.Error(), "дублирующийся ip") {
		t.Fatalf("err=%v, want упоминание дубля ip", err)
	}
}

// Конфиг только со счётчиком (без инверторов/МАП/CE308) допустим.
func TestLoadConfigMeterOnlyAccepted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	body := `{"dashboard_port":8080,"meter":{"disabled":false,"name":"M","ip":"192.0.2.40","port":502,"unit":1}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, _, _, _, err := loadConfig(path); err != nil {
		t.Fatalf("meter-only конфиг должен грузиться, got %v", err)
	}
}

// register_count < 18 отключает опрос счётчика (декодер читает регистры 0..17).
func TestMeterRegisterCountValidation(t *testing.T) {
	if mc := loadMeterConfig(&meterSection{Name: "M", IP: "192.0.2.77", Port: 502, Unit: 1, RegisterCnt: 10}); mc != nil {
		t.Fatalf("register_count=10 → mc=%v, want nil", mc)
	}
	if mc := loadMeterConfig(&meterSection{Name: "M", IP: "192.0.2.77", Port: 502, Unit: 1, RegisterCnt: 18}); mc == nil {
		t.Fatal("register_count=18 должен быть валиден")
	}
}

// Энергия счётчика (kWh) округляется до 1 знака, коэффициент мощности — нет.
func TestNeedsRoundingMeterTags(t *testing.T) {
	for _, tag := range []string{"meter_import", "meter_export", "meter_total", "meter_voltage", "meter_power"} {
		if !needsRounding(tag) {
			t.Errorf("needsRounding(%q)=false, want true", tag)
		}
	}
	if needsRounding("meter_power_factor") {
		t.Error("needsRounding(meter_power_factor)=true, want false")
	}
}

// Публичный sample обязан парситься как configFile (в частности logger_sn ≤ uint32).
func TestSampleConfigParses(t *testing.T) {
	b, err := os.ReadFile("sunReceiver.sample.json")
	if err != nil {
		t.Skipf("sample недоступен: %v", err)
	}
	var cf configFile
	if err := json.Unmarshal(b, &cf); err != nil {
		t.Fatalf("sunReceiver.sample.json не парсится: %v", err)
	}
}
