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
	body := `{"timestamp":"1788806657","_Uacc":"52.0","_Iacc":"4","_UNET":"220","_PNET":"910","_PNET_calc":"1141.2","_PLoad":"-200","_TFNET":"50.0"}`
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
		"ac_active_power":  208.0, // 52.0 × 4
		"grid_frequency":  50.0,
		"grid_voltage":    220.0,
		"grid_power":      1141.2, // из _PNET_calc (достоверная), а не _PNET=910
		"battery_power":   200.0, // −(−200)
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

func TestMapMAPAPIGridPowerFallback(t *testing.T) {
	// Нет _PNET_calc → grid_power берём из _PNET (фолбэк).
	var r mapRaw
	if err := json.Unmarshal([]byte(`{"timestamp":"1","_Uacc":"52.0","_Iacc":"1","_UNET":"220","_PNET":"910","_PLoad":"0","_TFNET":"50.0"}`), &r); err != nil {
		t.Fatalf("unmarshal mapRaw: %v", err)
	}
	vals, _, ok := mapMAPAPI(r)
	if !ok {
		t.Fatal("mapMAPAPI: ok=false, want true")
	}
	if v := vals["grid_power"].(float64); v != 910 {
		t.Errorf("grid_power = %v, want 910 (фолбэк на _PNET)", v)
	}
}

func TestMapMAPAPIChargeSign(t *testing.T) {
	// Заряд АКБ: _Iacc < 0, _PLoad > 0 (поступление в АКБ) → battery_power отрицательная.
	body := `{"timestamp":"1","_Uacc":"52.0","_Iacc":"-3.9","_UNET":"0","_PNET":"0","_PLoad":"200","_TFNET":"50.0"}`
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
}