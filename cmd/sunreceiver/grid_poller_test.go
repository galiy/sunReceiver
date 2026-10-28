package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGridBrand(t *testing.T) {
	cases := []struct {
		typ  string
		kind uint8
		want string
	}{
		{"deye", 1, "deye"},
		{"Sofar", 0, "sofar"},
		{"", 0, "deye"},
		{"", 1, "sofar"},
		{"deye_string", 1, ""},
		{"unknown", 0, ""},
	}
	for _, c := range cases {
		if got := gridBrand(c.typ, c.kind); got != c.want {
			t.Errorf("gridBrand(%q,%d)=%q, want %q", c.typ, c.kind, got, c.want)
		}
	}
}

func TestGridValuesUnitsAndTags(t *testing.T) {
	doc := gridDeviceDoc{
		EnergyToday: "7.9", EnergyTotal: "5693.3",
		Fields: []gridDeviceField{
			{ID: 1, NameKey: "stats.f.grid.pv1_voltage", Value: "63.2"},
			{ID: 2, NameKey: "stats.f.grid.pv1_current", Value: "1.25"},
			{ID: 7, NameKey: "stats.f.grid.ac_active_power", Value: "1234"},
			{ID: 8, NameKey: "stats.f.grid.ac_reactive_power", Value: "80"},
			{ID: 9, NameKey: "stats.f.grid.grid_frequency", Value: "49.98"},
			{ID: 16, NameKey: "stats.f.grid.temperature_1", Value: "41.7"},
			{ID: 17, NameKey: "stats.f.grid.temperature_2", Value: "", ND: true},
			{ID: 18, NameKey: "stats.f.grid.inverter_status", Value: "2"},
			{ID: 19, NameKey: "stats.f.grid.alarm_w0", Value: "0x0000"},
		},
	}
	vc := gridValues(doc, "deye")
	if vc["pv1_voltage"] != 63.2 || vc["ac_active_power"] != 1234.0 || vc["grid_frequency"] != 49.98 {
		t.Fatalf("values: %+v", vc)
	}
	if vc["energy_today"] != 7.9 || vc["energy_total"] != 5693.3 {
		t.Fatalf("energy: %+v", vc)
	}
	if vc["temperature_igbt"] != 41.7 {
		t.Fatalf("temperature_1: %+v", vc)
	}
	if _, ok := vc["temperature_radiator"]; ok {
		t.Fatalf("nd-температура не должна попадать в values: %+v", vc)
	}
	if _, ok := vc["inverter_status"]; ok {
		t.Fatalf("inverter_status не должен попадать в values: %+v", vc)
	}
	if _, ok := vc["alarm_w0"]; ok {
		t.Fatalf("alarm_w0 не должен попадать в values: %+v", vc)
	}

	sv := gridValues(doc, "sofar")
	if sv["temperature_module"] != 41.7 {
		t.Fatalf("sofar temperature_1: %+v", sv)
	}
}

func TestGridSnapshotIdentity(t *testing.T) {
	item := gridStatusItem{Serial: "SN1V480", Name: "Deye Left", Placement: "", LastSeen: 100, Kind: 0, LoggerSN: 1234567890}
	doc := gridDeviceDoc{Type: "deye", Placement: "Гараж", LastSeen: 200}
	sn := gridSnapshot(item, doc, "deye")
	if sn.InverterSN != "SN1V480" {
		t.Fatalf("deye serial changed: %q", sn.InverterSN)
	}
	if sn.DeviceSN != "1234567890" {
		t.Fatalf("logger SN: %q", sn.DeviceSN)
	}
	if sn.Name != "Deye Left" || sn.Placement != "Гараж" || sn.Kind != "deye" {
		t.Fatalf("snapshot: %+v", sn)
	}
	if sn.Timestamp != time.Unix(200, 0).Format(time.RFC3339) {
		t.Fatalf("timestamp=%s", sn.Timestamp)
	}

	// Sofar: хвостовой V<цифры> срезается — ключ истории не распадается.
	item = gridStatusItem{Serial: "SOF123V480", Kind: 1, LastSeen: 1}
	doc = gridDeviceDoc{Type: "sofar", Placement: ""}
	sn = gridSnapshot(item, doc, "sofar")
	if sn.DeviceSN != "" {
		t.Fatalf("пустой logger_sn должен давать пустой device_sn: %q", sn.DeviceSN)
	}
	if sn.InverterSN != "SOF123" {
		t.Fatalf("sofar serial normalization: %q", sn.InverterSN)
	}
	if sn.Placement != "Дом" {
		t.Fatalf("empty placement: %q", sn.Placement)
	}
}

func TestGridAlarmCodes(t *testing.T) {
	doc := gridDeviceDoc{Alarms: []gridDeviceAlarm{{Code: "W1"}, {Code: "W1"}, {Code: "F3"}}}
	got := gridAlarmCodes(doc)
	if len(got) != 2 || got[0] != "W1" || got[1] != "F3" {
		t.Fatalf("alarms: %v", got)
	}
	doc = gridDeviceDoc{AlarmWords: []string{"0x0000", "0x0002"}}
	got = gridAlarmCodes(doc)
	if len(got) != 1 || got[0] != "alarm_w1:0x0002" {
		t.Fatalf("alarm words fallback: %v", got)
	}
	// Варианты F35 канонизируются в один код.
	doc = gridDeviceDoc{Alarms: []gridDeviceAlarm{{Code: "F35: нет сети (AC_NoUtility)"}, {Code: "F35"}, {Code: "f35"}}}
	got = gridAlarmCodes(doc)
	if len(got) != 1 || got[0] != "F35" {
		t.Fatalf("F35 canonicalization: %v", got)
	}
}

func TestGridAPIParsing(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/grid/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// Литеральный JSON: опечатка в теге logger_sn не должна проходить.
		w.Write([]byte(`{"updated":1,"inverters":[{"id":0,"serial":"S1","name":"Deye Left","placement":"Дом","kind":0,"last_seen":100,"logger_sn":1234567890}]}`))
	})
	mux.HandleFunc("/api/grid/device", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("serial") != "S1" {
			http.Error(w, "not found", 404)
			return
		}
		json.NewEncoder(w).Encode(gridDeviceDoc{
			Serial: "S1", Name: "Deye Left", Placement: "Дом", Type: "deye",
			LastSeen: 100, EnergyToday: "1.2", EnergyTotal: "10.3",
			Fields: []gridDeviceField{{ID: 7, NameKey: "stats.f.grid.ac_active_power", Value: "42"}},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	src := &malina26Source{baseURL: srv.URL, client: srv.Client(), token: "test"}
	items, err := src.gridStatus(context.Background())
	if err != nil || len(items) != 1 || items[0].Serial != "S1" || items[0].LoggerSN != 1234567890 {
		t.Fatalf("gridStatus: %v %v", items, err)
	}
	doc, err := src.gridDevice(context.Background(), "S1")
	if err != nil || doc.Type != "deye" || len(doc.Fields) != 1 {
		t.Fatalf("gridDevice: %+v %v", doc, err)
	}
}

func TestGridNoUtilityCode(t *testing.T) {
	for _, c := range []string{"F35", "f35", "F35: нет сети (AC_NoUtility)", "F35 нет сети"} {
		if !gridNoUtilityCode(c) {
			t.Errorf("gridNoUtilityCode(%q)=false", c)
		}
	}
	for _, c := range []string{"F36", "F350", "", "W35"} {
		if gridNoUtilityCode(c) {
			t.Errorf("gridNoUtilityCode(%q)=true", c)
		}
	}
}
