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
	"fmt"
	"time"
)

// enbmsAveraged — одна усреднённая за 5 минут (avgStep) точка BMS EnBMS.
// Хранится в PostgreSQL (sunreceiver.enbms_averages.values, jsonb). В Redis-ряду
// лежат СЫРЫЕ показания (saveEnBmsReading), не эти средние. Мгновенные параметры
// (ток/мощность/SOC/ёмкости/напряжения ячеек/температуры) усредняются за
// промежуток; дискретные (число ячеек, циклы) — по последнему значению.
type enbmsAveraged struct {
	CurrentA      float64   `json:"current_a"`       // A, среднее (знаковый)
	PowerW        float64   `json:"power_w"`         // W, среднее (знаковая)
	Soc           float64   `json:"soc"`             // %, среднее
	CapacityAh    float64   `json:"capacity_ah"`     // А·ч, среднее
	RemainingAh   float64   `json:"remaining_ah"`    // А·ч, среднее
	TotalVoltageV float64   `json:"total_voltage_v"` // В, среднее
	PortVoltageV  float64   `json:"port_voltage_v"`  // В, среднее
	Soh           float64   `json:"soh"`             // %, среднее
	MaxCellV      float64   `json:"max_cell_v"`      // V, среднее
	MinCellV      float64   `json:"min_cell_v"`      // V, среднее
	AvgCellV      float64   `json:"avg_cell_v"`      // V, среднее
	CellsV        []float64 `json:"cells_v"`         // V, среднее по каждой ячейке
	Temperatures  []float64 `json:"temperatures_c"`  // °C, среднее по каналам
	CellCount     int       `json:"cell_count"`      // число ячеек, последнее
	Cycles        int       `json:"cycles"`          // циклы, последнее
	Samples       int       `json:"samples"`         // сколько снимков вошло в точку
}

// enbmsSeriesPoint — точка ряда EnBMS: ключ (MAC), отображаемое имя, время
// (RFC3339). В Redis-ряду это сырое показание (ts = секунда снятия, Samples=1);
// та же форма используется API для точек PG (ts = начало 5-минутного промежутка).
type enbmsSeriesPoint struct {
	Name    string `json:"name"`    // ключ (MAC)
	Display string `json:"display"` // отображаемое имя
	Ts      string `json:"ts"`      // время точки (RFC3339)
	enbmsAveraged
}

// enbmsAvgPoint — завершённый (или выгружаемый при остановке) 5-минутный
// промежуток одного устройства.
type enbmsAvgPoint struct {
	name    string
	display string
	start   time.Time
	avg     enbmsAveraged
}

// enbmsBucket — in-memory накопитель снимков EnBMS за один 5-минутный промежуток.
type enbmsBucket struct {
	start      time.Time
	display    string
	sums       map[string]float64
	cnt        map[string]int
	maxCellIdx int
	maxTempIdx int
	last       enbmsSnapshot
}

func newEnBmsBucket(start time.Time) *enbmsBucket {
	return &enbmsBucket{start: start, sums: map[string]float64{}, cnt: map[string]int{}}
}

// add вносит один снимок в накопитель.
func (b *enbmsBucket) add(s enbmsSnapshot) {
	b.last = s
	add := func(key string, v float64) {
		b.sums[key] += v
		b.cnt[key]++
	}
	add("current_a", s.CurrentA)
	add("power_w", s.PowerW)
	add("soc", s.Soc)
	add("capacity_ah", s.CapacityAh)
	add("remaining_ah", s.RemainingAh)
	add("total_voltage_v", s.TotalVoltageV)
	add("port_voltage_v", s.PortVoltageV)
	add("soh", s.Soh)
	add("max_cell_v", s.MaxCellV)
	add("min_cell_v", s.MinCellV)
	add("avg_cell_v", s.AvgCellV)
	for i, c := range s.CellsV {
		add(fmt.Sprintf("cell_%d", i), c)
		if i > b.maxCellIdx {
			b.maxCellIdx = i
		}
	}
	for i, t := range s.TemperaturesC {
		add(fmt.Sprintf("temp_%d", i), t)
		if i > b.maxTempIdx {
			b.maxTempIdx = i
		}
	}
}

// avg строит усреднённую точку: мгновенные параметры — среднее (напряжения
// ячеек до 0.001 В, температуры до 0.1 °C, прочее до 0.1); дискретные — из
// последнего снимка промежутка.
func (b *enbmsBucket) avg() enbmsAveraged {
	avg1 := func(key string) float64 {
		n := b.cnt[key]
		if n == 0 {
			return 0
		}
		return enbmsRound(b.sums[key]/float64(n), 1)
	}
	nCells := b.last.CellCount
	if l := len(b.last.CellsV); l > nCells {
		nCells = l
	}
	if n := b.maxCellIdx + 1; n > nCells {
		nCells = n
	}
	cells := make([]float64, 0, nCells)
	for i := 0; i < nCells && i <= b.maxCellIdx; i++ {
		key := fmt.Sprintf("cell_%d", i)
		if n := b.cnt[key]; n > 0 {
			cells = append(cells, enbmsRound(b.sums[key]/float64(n), 3))
		} else {
			cells = append(cells, 0)
		}
	}
	temps := make([]float64, 0, b.maxTempIdx+1)
	for i := 0; i <= b.maxTempIdx; i++ {
		key := fmt.Sprintf("temp_%d", i)
		if n := b.cnt[key]; n > 0 {
			temps = append(temps, enbmsRound(b.sums[key]/float64(n), 1))
		} else {
			temps = append(temps, 0)
		}
	}
	cellCount := b.last.CellCount
	if len(cells) > cellCount {
		cellCount = len(cells)
	}
	return enbmsAveraged{
		CurrentA:      avg1("current_a"),
		PowerW:        avg1("power_w"),
		Soc:           avg1("soc"),
		CapacityAh:    avg1("capacity_ah"),
		RemainingAh:   avg1("remaining_ah"),
		TotalVoltageV: avg1("total_voltage_v"),
		PortVoltageV:  avg1("port_voltage_v"),
		Soh:           avg1("soh"),
		MaxCellV:      avg1("max_cell_v"),
		MinCellV:      avg1("min_cell_v"),
		AvgCellV:      avg1("avg_cell_v"),
		CellsV:        cells,
		Temperatures:  temps,
		CellCount:     cellCount,
		Cycles:        b.last.Cycles,
		Samples:       b.cnt["current_a"],
	}
}

// enbmsAccumulator — in-memory аккумуляция 5-минутных усреднённых точек EnBMS.
// Однопоточный: трогает только горутина runEnBmsPoll.
type enbmsAccumulator struct {
	buckets map[string]*enbmsBucket // key = MAC
	pending []enbmsAvgPoint
}

func newEnBmsAccumulator() *enbmsAccumulator {
	return &enbmsAccumulator{buckets: map[string]*enbmsBucket{}}
}

// add кладёт снимок в накопитель 5-минутного промежутка, содержащего now.
func (a *enbmsAccumulator) add(s enbmsSnapshot, now time.Time) {
	if s.MAC == "" {
		return
	}
	start := floorToStep(now)
	b := a.buckets[s.MAC]
	if b == nil || !b.start.Equal(start) {
		if b != nil && !b.start.Add(avgStep).After(now) {
			a.pending = append(a.pending, enbmsAvgPoint{name: s.MAC, display: b.display, start: b.start, avg: b.avg()})
		}
		b = newEnBmsBucket(start)
		b.display = s.Name
		a.buckets[s.MAC] = b
	}
	b.add(s)
}

// closed возвращает завершившиеся промежутки (конец = start+avgStep <= now).
func (a *enbmsAccumulator) closed(now time.Time) []enbmsAvgPoint {
	var out []enbmsAvgPoint
	for key, b := range a.buckets {
		if !b.start.Add(avgStep).After(now) {
			out = append(out, enbmsAvgPoint{name: key, display: b.display, start: b.start, avg: b.avg()})
			delete(a.buckets, key)
		}
	}
	out = append(out, a.pending...)
	a.pending = nil
	return out
}

// drain возвращает все оставшиеся (возможно неполные) промежутки при остановке.
func (a *enbmsAccumulator) drain() []enbmsAvgPoint {
	var out []enbmsAvgPoint
	for key, b := range a.buckets {
		out = append(out, enbmsAvgPoint{name: key, display: b.display, start: b.start, avg: b.avg()})
	}
	a.buckets = map[string]*enbmsBucket{}
	return out
}
