package main

import "testing"

func TestInverterNameByIP(t *testing.T) {
	targets := []invTarget{
		{IP: "192.168.0.10", Name: "Дом", Kind: kindDeyeString, Placement: "Дом"},
		{IP: "192.168.0.11", Name: "Гараж", Kind: kindSofar, Placement: "Гараж"},
		{IP: "192.168.0.12", Name: "MAP", Kind: kindMAP},
	}
	m := inverterNameByIP(targets)
	if m["192.168.0.10"] != "Дом" || m["192.168.0.11"] != "Гараж" {
		t.Fatalf("inverterNameByIP = %v", m)
	}
	if _, ok := m["192.168.0.12"]; ok {
		t.Errorf("МАП не должен попадать в карту имён инверторов: %v", m)
	}
	if ip, ok := inverterIPByName(m, "Гараж"); !ok || ip != "192.168.0.11" {
		t.Errorf("inverterIPByName(Гараж) = %q,%v", ip, ok)
	}
	if _, ok := inverterIPByName(m, "Нет такого"); ok {
		t.Error("inverterIPByName для неизвестного имени должен вернуть false")
	}
}
