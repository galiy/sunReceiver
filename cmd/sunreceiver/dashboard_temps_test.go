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

// TestTemperatureSeries проверяет сборку рядов температур для графика: инверторы
// (Deye — Корпус/Транзисторы, Sofar — Корпус/Транзисторы) и МАП (Батарея/Тор/
// Транзисторы). Отсутствующий датчик Deye (raw 0 → −100) не добавляется, счётчик
// (meter_voltage) исключается, каждая линия получает имя «Устройство — датчик».
func TestTemperatureSeries(t *testing.T) {
	now := time.Now()
	devices := []deviceSnapshot{
		animSnapKind("Deye", "10.0.0.1", "Дом", now, map[string]float64{
			"ac_active_power":      100,
			"temperature_radiator": 41.2,
			"temperature_igbt":     -100, // датчик отсутствует (offset −100)
		}, "deye"),
		animSnapKind("Sofar", "10.0.0.2", "Дом", now, map[string]float64{
			"ac_active_power":    50,
			"temperature_inner":  38,
			"temperature_module": 47,
		}, "sofar"),
		animSnap("МАП", "10.0.0.8", "", now, map[string]float64{
			"battery_voltage":     52,
			"map_temp_battery":    23,
			"map_temp_tor":        44,
			"map_temp_transistor": 35,
		}),
		animSnapKind("Счётчик", "10.0.0.9", "", now, map[string]float64{
			"meter_voltage":        230,
			"temperature_radiator": 99, // не должно попасть: это счётчик
		}, ""),
	}

	got := temperatureSeries(devices)
	byName := map[string]deviceSeries{}
	for _, s := range got {
		byName[s.Name] = s
	}

	if len(byName) != 6 {
		t.Fatalf("линий температур: want 6, got %d: %v", len(byName), namesOf(got))
	}
	want := map[string]float64{
		"Deye — Корпус":       41.2,
		"Sofar — Корпус":      38,
		"Sofar — Транзисторы": 47,
		"МАП — Батарея":       23,
		"МАП — Тор":           44,
		"МАП — Транзисторы":   35,
	}
	for name, v := range want {
		s, ok := byName[name]
		if !ok {
			t.Fatalf("нет линии %q (есть %v)", name, namesOf(got))
		}
		if len(s.Points) != 1 || s.Points[0].V != v {
			t.Fatalf("%s: want одну точку %v, got %v", name, v, s.Points)
		}
		if s.Color == "" {
			t.Fatalf("%s: пустой цвет", name)
		}
	}
	if _, ok := byName["Deye — Транзисторы"]; ok {
		t.Error("датчик Deye с температурой −100 не должен добавляться")
	}
	if _, ok := byName["Счётчик — Корпус"]; ok {
		t.Error("температура счётчика не должна попадать на график температур")
	}
	// Порядок: инверторы по имени, МАП — в конце.
	last := got[len(got)-1].Name
	if last != "МАП — Транзисторы" {
		t.Errorf("последняя линия: want МАП, got %q", last)
	}
}

func namesOf(ss []deviceSeries) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, s.Name)
	}
	return out
}
