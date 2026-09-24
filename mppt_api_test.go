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
	"math"
	"testing"
	"time"
)

// closeEnough возвращает true, если |a-b| > ε (то есть значения НЕ совпадают).
func closeEnough(a, b float64) bool {
	const eps = 1e-6
	return math.Abs(a-b) > eps
}

func TestMapMAPAPIParse(t *testing.T) {
	// Фрагмент ответа read_json.php?device=map (docs/read_json.md) + _PNET_calc.
	body := `{"timestamp":"1788806657","_Uacc":"52.0","_Iacc":"4","_UNET":"220","_PNET":"910","_PNET_calc":"1141.2","_PLoad":"-200","_PLoad_calc":"208","_TFNET":"50.0"}`
	var r mapRaw
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("unmarshal mapRaw: %v", err)
	}
	if r.Timestamp != 1788806657 {
		t.Errorf("timestamp = %d, want 1788806657", r.Timestamp)
	}
	if r.PNetCalc != "1141.2" {
		t.Errorf("PNetCalc = %q, want 1141.2", r.PNetCalc)
	}
	vals, ts, ok := mapMAPAPI(r)
	if !ok {
		t.Fatal("mapMAPAPI: ok=false, want true")
	}
	wantTS := time.Unix(1788806657, 0)
	if !ts.Equal(wantTS) {
		t.Errorf("ts = %v, want %v", ts, wantTS)
	}
	checks := map[string]float64{
		"battery_voltage": 52.0,
		"l1_voltage":      52.0,
		"l1_current":      4.0,
		"ac_active_power": 208.0, // 52.0 × 4
		"grid_frequency":  50.0,
		"grid_voltage":    220.0,
		"grid_power":      -1141.2, // −_PNET_calc (инверсия знака ветки API; не _PNET=910)
		"battery_power":   -208.0,  // −_PLoad_calc (= −(I×U)), а не −_PLoad=−(−200)
	}
	for k, want := range checks {
		got, ok := vals[k].(float64)
		if !ok {
			t.Errorf("%s: отсутствует", k)
			continue
		}
		if got != want {
			t.Errorf("%s = %v, want %v", k, got, want)
		}
	}
}

func TestMapMAPAPIGridPowerSign(t *testing.T) {
	// Новая Малина (mapd fw 4.3) отдаёт _PNET_calc с обратным знаком: при
	// потреблении из сети значение отрицательное. Контракт дашборда — потребление
	// положительное, отдача отрицательная, поэтому знак инвертируется.
	cases := []struct {
		net    string
		wantGP float64
	}{
		{"-13578.8", 13578.8}, // потребление из сети → + (заряд АКБ из сети)
		{"1141.2", -1141.2},   // отдача в сеть → −
	}
	for _, c := range cases {
		body := `{"timestamp":"1","_Uacc":"52.0","_Iacc":"4","_UNET":"220","_PNET":"0","_PLoad":"0","_PLoad_calc":"208","_TFNET":"50.0","_PNET_calc":"` + c.net + `"}`
		var r mapRaw
		if err := json.Unmarshal([]byte(body), &r); err != nil {
			t.Fatalf("unmarshal mapRaw: %v", err)
		}
		vals, _, ok := mapMAPAPI(r)
		if !ok {
			t.Fatal("mapMAPAPI: ok=false, want true")
		}
		if got := vals["grid_power"].(float64); got != c.wantGP {
			t.Errorf("_PNET_calc=%s → grid_power = %v, want %v", c.net, got, c.wantGP)
		}
	}
}

func TestMapMAPAPINoGridPowerWithoutCalc(t *testing.T) {
	// Нет _PNET_calc → grid_power НЕ выставляется (фолбэк на недостоверный _PNET
	// убран: подставлять заведомо ошибочное значение хуже, чем отсутствие).
	var r mapRaw
	if err := json.Unmarshal([]byte(`{"timestamp":"1","_Uacc":"52.0","_Iacc":"1","_UNET":"220","_PNET":"910","_PLoad":"0","_TFNET":"50.0"}`), &r); err != nil {
		t.Fatalf("unmarshal mapRaw: %v", err)
	}
	vals, _, ok := mapMAPAPI(r)
	if !ok {
		t.Fatal("mapMAPAPI: ok=false, want true")
	}
	if _, present := vals["grid_power"]; present {
		t.Errorf("grid_power присутствует без _PNET_calc=%q, want отсутствует", r.PNetCalc)
	}
}

func TestMapMAPAPIChargeSign(t *testing.T) {
	// Заряд АКБ: battery_power = −_PLoad_calc (положительное _PLoad_calc → отрицательная).
	// _PLoad_calc — достоверная батарейная мощность (I×U), используется вместо _PLoad.
	body := `{"timestamp":"1","_Uacc":"52.0","_Iacc":"-3.9","_UNET":"0","_PNET":"0","_PLoad":"200","_PLoad_calc":"200","_TFNET":"50.0"}`
	var r mapRaw
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("unmarshal mapRaw: %v", err)
	}
	vals, _, ok := mapMAPAPI(r)
	if !ok {
		t.Fatal("mapMAPAPI: ok=false, want true")
	}
	if v := vals["ac_active_power"].(float64); closeEnough(v, -202.8) {
		t.Errorf("ac_active_power = %v, want -202.8", v)
	}
	if v := vals["battery_power"].(float64); closeEnough(v, -200.0) {
		t.Errorf("battery_power = %v, want -200.0 (заряд — отрицательная)", v)
	}
}

func TestMapMAPAPINoUacc(t *testing.T) {
	// Нет напряжения АКБ → устройство не считается имеющим данные (нет battery_voltage).
	var r mapRaw
	if err := json.Unmarshal([]byte(`{"timestamp":"1","_UNET":"220"}`), &r); err != nil {
		t.Fatalf("unmarshal mapRaw: %v", err)
	}
	if _, _, ok := mapMAPAPI(r); ok {
		t.Fatal("mapMAPAPI: ok=true без _Uacc, want false")
	}
}

func TestMapMAPAPITemperatures(t *testing.T) {
	// Температуры МАП из device=map: _Temp_Grad0 (АКБ), _Temp_Grad1 (тор),
	// _Temp_Grad2 (транзисторы) — API отдаёт уже в градусах. Temp_off — признаки
	// отсутствия датчиков.
	body := `{"timestamp":"1","_Uacc":"52.0","_Iacc":"1","_UNET":"220","_PNET_calc":"1141.2","_PLoad_calc":"208","_TFNET":"50.0","_Temp_Grad0":"23","_Temp_Grad1":"44","_Temp_Grad2":"35","Temp_off":"0"}`
	var r mapRaw
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("unmarshal mapRaw: %v", err)
	}
	vals, _, ok := mapMAPAPI(r)
	if !ok {
		t.Fatal("mapMAPAPI: ok=false, want true")
	}
	exp := map[string]float64{
		"map_temp_battery":    23,
		"map_temp_tor":        44,
		"map_temp_transistor": 35,
	}
	for k, want := range exp {
		if v, ok := vals[k].(float64); !ok || v != want {
			t.Errorf("%s = %v, want %v", k, vals[k], want)
		}
	}
}

func TestMapMAPAPITempOffMask(t *testing.T) {
	// Temp_off=0x04 (датчик транзисторов отсутствует) → map_temp_transistor
	// НЕ выставляется, остальные температуры остаются.
	body := `{"timestamp":"1","_Uacc":"52.0","_Iacc":"1","_UNET":"220","_PNET_calc":"1141.2","_PLoad_calc":"208","_TFNET":"50.0","_Temp_Grad0":"23","_Temp_Grad1":"44","_Temp_Grad2":"35","Temp_off":"4"}`
	var r mapRaw
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("unmarshal mapRaw: %v", err)
	}
	vals, _, ok := mapMAPAPI(r)
	if !ok {
		t.Fatal("mapMAPAPI: ok=false, want true")
	}
	if _, present := vals["map_temp_transistor"]; present {
		t.Errorf("map_temp_transistor присутствует при Temp_off&0x04, want отсутствует")
	}
	if v, ok := vals["map_temp_battery"].(float64); !ok || v != 23 {
		t.Errorf("map_temp_battery = %v, want 23", vals["map_temp_battery"])
	}
	if v, ok := vals["map_temp_tor"].(float64); !ok || v != 44 {
		t.Errorf("map_temp_tor = %v, want 44", vals["map_temp_tor"])
	}
}

func TestMapMAPRegistersTemperatures(t *testing.T) {
	// Modbus-ветка: ячейки температуры _Temp_Grad0/1/2 = 0x42E/0x42F/0x430 (raw,
	// T = raw − 50), _Temp_off = 0x43C (биты отсутствия датчиков). Блок 0x400
	// уже включает эти ячейки, отдельного чтения не требуется.
	cells := map[uint16]byte{
		0x405: 0x8, 0x406: 0x6, // UAcc = 0x0608 = 1544 → 154.4 В (не важно для теста)
		0x432: 0, 0x433: 0,      // IAcc = 0
		0x400: 0,                // MODE (не заряд)
		0x42E: 73,               // АКБ: 73−50 = 23
		0x42F: 94,               // тор: 94−50 = 44
		0x430: 85,               // транз.: 85−50 = 35
	}
	v := mapMAPRegisters(cells)
	exp := map[string]float64{
		"map_temp_battery":    23,
		"map_temp_tor":        44,
		"map_temp_transistor": 35,
	}
	for k, want := range exp {
		if got, ok := v[k].(float64); !ok || got != want {
			t.Errorf("%s = %v, want %v", k, v[k], want)
		}
	}
}

func TestMapMAPRegistersTempOffMask(t *testing.T) {
	// _Temp_off=0x04 (датчик транзисторов отсутствует) → map_temp_transistor нет.
	cells := map[uint16]byte{
		0x405: 0x8, 0x406: 0x6,
		0x432: 0, 0x433: 0,
		0x400: 0,
		0x42E: 73,
		0x42F: 94,
		0x430: 85,
		0x43C: 0x04,
	}
	v := mapMAPRegisters(cells)
	if _, present := v["map_temp_transistor"]; present {
		t.Errorf("map_temp_transistor присутствует при _Temp_off&0x04, want отсутствует")
	}
	if got, ok := v["map_temp_battery"].(float64); !ok || got != 23 {
		t.Errorf("map_temp_battery = %v, want 23", v["map_temp_battery"])
	}
}

func TestMpptSiteAPIURL(t *testing.T) {
	s := &mpptSite{BaseURL: "http://192.168.0.60", MPPTPath: "/read_json.php?device=mppt"}
	if got := s.apiURL("map"); got != "http://192.168.0.60/read_json.php?device=map" {
		t.Errorf("apiURL(map) = %q", got)
	}
	if got := s.apiURL("mppt"); got != "http://192.168.0.60/read_json.php?device=mppt" {
		t.Errorf("apiURL(mppt) = %q", got)
	}
	// Путь без query — параметр device добавляется.
	s2 := &mpptSite{BaseURL: "http://x", MPPTPath: "/read_json.php"}
	if got := s2.apiURL("map"); got != "http://x/read_json.php?device=map" {
		t.Errorf("apiURL(map) без query = %q", got)
	}
	// Явный map_path имеет приоритет для device=map.
	s3 := &mpptSite{BaseURL: "http://192.168.0.60", MPPTPath: "/read_json.php?device=mppt", MapPath: "/read_json.php?device=map"}
	if got := s3.apiURL("map"); got != "http://192.168.0.60/read_json.php?device=map" {
		t.Errorf("apiURL(map) с map_path = %q", got)
	}
	if got := s3.apiURL("mppt"); got != "http://192.168.0.60/read_json.php?device=mppt" {
		t.Errorf("apiURL(mppt) с map_path = %q", got)
	}
}

func TestMapMPPTAPIZeroTimestamp(t *testing.T) {
	// timestamp отсутствует/не распарсился (=0) → mapMPPTAPI возвращает НУЛЕВОЕ
	// время, а не time.Unix(0,0) (оно не IsZero() и дало бы снимок с 1970).
	var r mpptRaw
	if err := json.Unmarshal([]byte(`{"UID":"1097","Vc_PV":"120.0","Ic_PV":"2","P_PV":"240"}`), &r); err != nil {
		t.Fatalf("unmarshal mpptRaw (без timestamp): %v", err)
	}
	vals, ts, ok := mapMPPTAPI(r)
	if !ok {
		t.Fatal("mapMPPTAPI: ok=false, want true")
	}
	if !ts.IsZero() {
		t.Errorf("ts = %v, want нулевое (IsZero) — saveWindowSnapshot возьмёт now", ts)
	}
	if v := vals["pv1_voltage"].(float64); v != 120.0 {
		t.Errorf("pv1_voltage = %v, want 120", v)
	}
}
