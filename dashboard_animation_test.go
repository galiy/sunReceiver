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
	"testing"
	"time"
)

// animSnap строит снимок устройства с заданными тегами и timestamp.
func animSnap(name, ip, placement string, ts time.Time, tags map[string]float64) deviceSnapshot {
	return animSnapKind(name, ip, placement, ts, tags, "")
}

func animSnapKind(name, ip, placement string, ts time.Time, tags map[string]float64, kind string) deviceSnapshot {
	v := valuesContract{}
	for k, val := range tags {
		v[k] = val
	}
	return deviceSnapshot{
		Timestamp: ts.Format(time.RFC3339),
		Name:      name, IP: ip, Placement: placement, Kind: kind, Values: v,
	}
}

// TestBuildAnimationKindFromConfig — марка инвертора берётся из конфига (снимок.Kind),
// а не из тегов: даже «молчащий» Deye (без dc_total_power в старом снимке) остаётся
// deye, а не превращается в sofar (регрессия выбора спрайта).
func TestBuildAnimationKindFromConfig(t *testing.T) {
	now := time.Now()
	old := now.Add(-time.Hour)
	devices := []deviceSnapshot{
		// старый снимок Deye без телега dc_total_power → Kind из конфига "deye".
		animSnapKind("Deye Off", "10.0.0.1", "Дом", old, map[string]float64{"ac_active_power": 0}, "deye"),
	}
	res := buildAnimationResponse(devices, nil, nil, now)
	if len(res.House.Inverters) != 1 {
		t.Fatalf("want 1 inverter, got %d", len(res.House.Inverters))
	}
	if res.House.Inverters[0].Kind != "deye" {
		t.Fatalf("kind: want deye (из конфига), got %q", res.House.Inverters[0].Kind)
	}
	if !res.House.Inverters[0].Stale {
		t.Fatalf("inverter должен быть stale")
	}
}

// TestBuildAnimationResponse проверяет сборку данных страницы анимации:
// группировку инверторов по размещению (Дом/Гараж), MPPT-контроллеры (КЭС) — в
// Дом, мощности МАП/счётчика, формулу мощности Дома (разница) и пометку Stale.
func TestBuildAnimationResponse(t *testing.T) {
	now := time.Now()
	old := now.Add(-time.Hour) // «молчащее» устройство
	devices := []deviceSnapshot{
		animSnapKind("Deye Дом1", "10.0.0.1", "Дом", now, map[string]float64{"ac_active_power": 100, "dc_total_power": 120}, "deye"),
		animSnapKind("Sofar Дом2", "10.0.0.2", "Дом", now, map[string]float64{"ac_active_power": 50, "pv1_power": 40, "pv2_power": 20}, "sofar"),
		animSnapKind("Deye Гараж1", "10.0.0.3", "Гараж", now, map[string]float64{"ac_active_power": 30, "dc_total_power": 35}, "deye"),
		animSnapKind("Sofar Гараж2", "10.0.0.4", "Гараж", now, map[string]float64{"ac_active_power": 20, "pv1_power": 25}, "sofar"),
		// MPPT (КЭС) — всегда в Дом, под батареей.
		animSnapKind("КЭС1", "host#mppt0", "", now, map[string]float64{"ac_active_power": 200, "pv1_power": 210}, "kes"),
		// МАП (батарея/сеть): маркер batter_voltage; сеть и батарея.
		animSnap("МАП", "10.0.0.8", "", now, map[string]float64{"battery_voltage": 52, "grid_power": 300, "battery_power": -150}),
		// Счётчик: маркер meter_voltage.
		animSnap("Счётчик", "10.0.0.9", "", now, map[string]float64{"meter_voltage": 230, "meter_active_power": 250}),
		// Молчащий инвертор (Дом) — Stale=true, мощность не входит в дом.
		animSnapKind("Deye Off", "10.0.0.5", "Дом", old, map[string]float64{"ac_active_power": 500, "dc_total_power": 500}, "deye"),
	}

	res := buildAnimationResponse(devices, nil, nil, now)

	if len(res.House.Inverters) != 3 {
		t.Fatalf("house inverters: want 3 (2 fresh + 1 stale), got %d (%v)", len(res.House.Inverters), res.House.Inverters)
	}
	// PV Deye — dc_total_power; PV Sofar — pv1+pv2.
	byName := map[string]animInverter{}
	for _, inv := range res.House.Inverters {
		byName[inv.Name] = inv
	}
	if byName["Deye Дом1"].PV != 120 {
		t.Fatalf("deye pv: want 120 (dc_total_power), got %v", byName["Deye Дом1"].PV)
	}
	if byName["Sofar Дом2"].PV != 60 {
		t.Fatalf("sofar pv: want 60 (pv1+pv2), got %v", byName["Sofar Дом2"].PV)
	}
	if byName["Deye Дом1"].Kind != "deye" || byName["Sofar Дом2"].Kind != "sofar" {
		t.Fatalf("kinds wrong: %v / %v", byName["Deye Дом1"].Kind, byName["Sofar Дом2"].Kind)
	}

	if len(res.Garage.Inverters) != 2 {
		t.Fatalf("garage inverters: want 2, got %d (%v)", len(res.Garage.Inverters), res.Garage.Inverters)
	}

	if len(res.House.KES) != 1 || res.House.KES[0].PV != 210 {
		t.Fatalf("kes: want 1 with pv 210, got %v", res.House.KES)
	}

	// P_дом = Σac(инверторы дома) + P(сеть) + P(батарея) = (100+50) + 300 + (−150) = 300.
	if want := 300.0; res.House.HousePower != want {
		t.Fatalf("house power: want %v, got %v", want, res.House.HousePower)
	}
	if res.House.MapGridPower != 300 || res.House.MapBatteryPower != -150 {
		t.Fatalf("map powers wrong: grid=%v batt=%v", res.House.MapGridPower, res.House.MapBatteryPower)
	}
	if res.House.MeterActivePower != 250 {
		t.Fatalf("meter power: want 250, got %v", res.House.MeterActivePower)
	}
	if res.Garage.GaragePower != 0 {
		t.Fatalf("garage power: want 0 (нет нагрузки), got %v", res.Garage.GaragePower)
	}
}

// TestBuildAnimationGarageMeter проверяет мощность Гаража на развилке со счётчиком
// CE308: P_гараж = P(CE308) − Σac(инверторы гаража). Без свежего снимка CE308 —
// мощность гаража нулевая (узел статичен).
func TestBuildAnimationGarageMeter(t *testing.T) {
	now := time.Now()
	devices := []deviceSnapshot{
		// Гараж: два инвертора — 30 Вт (Deye) и 20 Вт (Sofar). Σac = 50 Вт.
		animSnapKind("Deye Гараж", "10.0.0.3", "Гараж", now, map[string]float64{"ac_active_power": 30, "dc_total_power": 35}, "deye"),
		animSnapKind("Sofar Гараж", "10.0.0.4", "Гараж", now, map[string]float64{"ac_active_power": 20, "pv1_power": 25}, "sofar"),
	}
	// Свежий снимок CE308: потребление из сети 120 Вт.
	ce308 := map[string]ce308Snapshot{
		"CE308": {Name: "CE308", IP: "10.0.0.20", Timestamp: now.Format(time.RFC3339),
			Values: map[string]float64{ce308ActiveP: 120}},
	}
	res := buildAnimationResponse(devices, ce308, nil, now)
	if want := 120.0; res.Garage.Ce308Power != want {
		t.Fatalf("ce308 power: want %v, got %v", want, res.Garage.Ce308Power)
	}
	// 120 − (30+20) = 70 Вт в гараж.
	if want := 70.0; res.Garage.GaragePower != want {
		t.Fatalf("garage power: want %v, got %v", want, res.Garage.GaragePower)
	}
	// Без свежего снимка CE308 — мощности гаража не считаем (0).
	res2 := buildAnimationResponse(devices, nil, nil, now)
	if res2.Garage.Ce308Power != 0 || res2.Garage.GaragePower != 0 {
		t.Fatalf("без CE308: ce308=%v garage=%v, want 0/0", res2.Garage.Ce308Power, res2.Garage.GaragePower)
	}
	// Устаревший снимок CE308 — тоже не считаем.
	staleCE := map[string]ce308Snapshot{
		"CE308": {Name: "CE308", IP: "10.0.0.20", Timestamp: now.Add(-time.Hour).Format(time.RFC3339),
			Values: map[string]float64{ce308ActiveP: 120}},
	}
	res3 := buildAnimationResponse(devices, staleCE, nil, now)
	if res3.Garage.GaragePower != 0 {
		t.Fatalf("stale ce308: garage=%v, want 0", res3.Garage.GaragePower)
	}
}

// TestBuildAnimationStaleMark проверяет, что «молчащие» (снимок старше окна)
// устройства помечаются Stale=true и их мощность не влияет на формулу Дома.
func TestBuildAnimationStaleMark(t *testing.T) {
	now := time.Now()
	old := now.Add(-time.Hour)
	devices := []deviceSnapshot{
		animSnap("Off", "10.0.0.1", "Дом", old, map[string]float64{"ac_active_power": 700, "dc_total_power": 700}),
		animSnap("МАП", "10.0.0.8", "", old, map[string]float64{"battery_voltage": 52, "grid_power": 100, "battery_power": 0}),
	}
	// Молчащий инвертор (снимок > 20 мин) в формуле Дома не участвует: его
	// устаревшая мощность (700 Вт) не «оживляет» дом ночью.
	res := buildAnimationResponse(devices, nil, nil, now)
	if len(res.House.Inverters) != 1 || !res.House.Inverters[0].Stale {
		t.Fatalf("inverter должен быть Stale=true: %v", res.House.Inverters)
	}
	// Молчащее устройство: мощности PV/AC считаются нулевыми (не показываются).
	inv := res.House.Inverters[0]
	if inv.PV != 0 || inv.AC != 0 {
		t.Fatalf("stale inverter PV/AC должны быть 0, got pv=%v ac=%v", inv.PV, inv.AC)
	}
	// МАП со старым снимком игнорируется — мощность сети 0, P_дом = 0 + 0 − 0 = 0.
	if res.House.HousePower != 0 {
		t.Fatalf("house power with stale devices: want 0, got %v", res.House.HousePower)
	}
	if res.House.MapGridPower != 0 {
		t.Fatalf("stale map grid power: want 0, got %v", res.House.MapGridPower)
	}
}

// TestBuildAnimationPlacementFromConfig проверяет, что размещение берётся из
// конфига (placeByIP) даже для устаревшего снимка без поля placement.
func TestBuildAnimationPlacementFromConfig(t *testing.T) {
	now := time.Now()
	// У снимка пустой placement — он был записан старой версией.
	devices := []deviceSnapshot{
		animSnap("Stale Left", "10.0.0.70", "", now, map[string]float64{"ac_active_power": 700}),
	}
	placeByIP := map[string]string{"10.0.0.70": "Гараж"}
	res := buildAnimationResponse(devices, nil, placeByIP, now)
	if len(res.Garage.Inverters) != 1 || res.Garage.Inverters[0].Name != "Stale Left" {
		t.Fatalf("инвертор из по конфигу должен быть в гараже: house=%v garage=%v",
			res.House.Inverters, res.Garage.Inverters)
	}
	if len(res.House.Inverters) != 0 {
		t.Fatalf("не должно быть инверторов в доме: %v", res.House.Inverters)
	}
}

// tempByLabel возвращает значение температуры из списка по подписи.
func tempByLabel(t *testing.T, temps []animTemp, label string) (float64, bool) {
	t.Helper()
	for _, tp := range temps {
		if tp.Label == label {
			return tp.Value, true
		}
	}
	return 0, false
}

// TestBuildAnimationTemperatures проверяет извлечение температур на страницу
// анимации: панель над МАП (Тор/Транзисторы), температура батареи (по данным МАП)
// и температуры инверторов (Deye: Корпус/Транзисторы; Sofar: Корпус/Транзисторы).
func TestBuildAnimationTemperatures(t *testing.T) {
	now := time.Now()
	devices := []deviceSnapshot{
		// МАП: температуры тора/транзисторов/батареи.
		animSnap("МАП", "10.0.0.8", "", now, map[string]float64{
			"battery_voltage":     52,
			"map_temp_tor":        44,
			"map_temp_transistor": 35,
			"map_temp_battery":    23,
		}),
		// Deye: radiator (Корпус) и igbt (Транзисторы).
		animSnapKind("Deye", "10.0.0.1", "Дом", now, map[string]float64{
			"ac_active_power":      100,
			"temperature_radiator": 41.2,
			"temperature_igbt":     55.6,
		}, "deye"),
		// Sofar: inner (Корпус) и module (Транзисторы).
		animSnapKind("Sofar", "10.0.0.2", "Дом", now, map[string]float64{
			"ac_active_power":    50,
			"temperature_inner":  38,
			"temperature_module": 47,
		}, "sofar"),
	}

	res := buildAnimationResponse(devices, nil, nil, now)

	// Панель над МАП: Тор и Транзисторы.
	if len(res.House.MapTemps) != 2 {
		t.Fatalf("map temps: want 2, got %v", res.House.MapTemps)
	}
	if v, ok := tempByLabel(t, res.House.MapTemps, "Тор"); !ok || v != 44 {
		t.Fatalf("map Тор: want 44, got %v (ok=%v)", v, ok)
	}
	if v, ok := tempByLabel(t, res.House.MapTemps, "Транзисторы"); !ok || v != 35 {
		t.Fatalf("map Транзисторы: want 35, got %v (ok=%v)", v, ok)
	}
	// Температура батареи (справа от спрайта).
	if res.House.BatteryTemp == nil || *res.House.BatteryTemp != 23 {
		t.Fatalf("battery temp: want 23, got %v", res.House.BatteryTemp)
	}

	byName := map[string]animInverter{}
	for _, inv := range res.House.Inverters {
		byName[inv.Name] = inv
	}
	// Deye: radiator → Корпус, igbt → Транзисторы.
	deye, ok := byName["Deye"]
	if !ok {
		t.Fatal("нет Deye в схеме")
	}
	if v, ok := tempByLabel(t, deye.Temps, "Корпус"); !ok || v != 41.2 {
		t.Fatalf("deye Корпус (radiator): want 41.2, got %v (ok=%v)", v, ok)
	}
	if v, ok := tempByLabel(t, deye.Temps, "Транзисторы"); !ok || v != 55.6 {
		t.Fatalf("deye Транзисторы (igbt): want 55.6, got %v (ok=%v)", v, ok)
	}
	// Sofar: inner → Корпус, module → Транзисторы.
	sofar, ok := byName["Sofar"]
	if !ok {
		t.Fatal("нет Sofar в схеме")
	}
	if v, ok := tempByLabel(t, sofar.Temps, "Корпус"); !ok || v != 38 {
		t.Fatalf("sofar Корпус (inner): want 38, got %v (ok=%v)", v, ok)
	}
	if v, ok := tempByLabel(t, sofar.Temps, "Транзисторы"); !ok || v != 47 {
		t.Fatalf("sofar Транзисторы (module): want 47, got %v (ok=%v)", v, ok)
	}
}
