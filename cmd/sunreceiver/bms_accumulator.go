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

// bmsAveraged — одна усреднённая за 5 минут точка ANT BMS. Используется как
// промежуточная форма при переносе точек старых пулеров в bms26 (см.
// bms26_legacy.go); собственный аккумулятор ANT удалён (усреднение — общий
// bms26Accumulator).
type bmsAveraged struct {
	CurrentA     float64   `json:"current_a"`      // A, среднее (знаковый)
	PowerW       float64   `json:"power_w"`        // W, среднее (знаковая)
	Soc          float64   `json:"soc"`            // %, среднее
	CapacityAh   float64   `json:"capacity_ah"`    // А·ч, среднее
	RemainingAh  float64   `json:"remaining_ah"`   // А·ч, среднее
	MaxCellV     float64   `json:"max_cell_v"`     // V, среднее
	MinCellV     float64   `json:"min_cell_v"`     // V, среднее
	AvgCellV     float64   `json:"avg_cell_v"`     // V, среднее
	CellsV       []float64 `json:"cells_v"`        // V, среднее по каждой ячейке
	Temperatures []float64 `json:"temperatures_c"` // °C, среднее по каналам T1..T6
	CellCount    int       `json:"cell_count"`     // число ячеек, последнее в промежутке
	ChargeMos    int       `json:"charge_mos"`     // MOS зарядки, последнее в промежутке
	DischargeMos int       `json:"discharge_mos"`  // MOS разряда, последнее в промежутке
	Balancer     int       `json:"balancer"`       // балансировка, последнее в промежутке
	Frames       uint32    `json:"frames"`         // счётчик кадров, последнее в промежутке
	Samples      int       `json:"samples"`        // сколько 1-секундных снимков вошло в точку
}

// bmsSeriesPoint — точка старого ANT-ряда: ключ устройства, время (RFC3339) и
// значения. Используется при переносе точек старых пулеров в bms26.
type bmsSeriesPoint struct {
	Name    string `json:"name"`    // старый ключ (bmsKey)
	Display string `json:"display"` // отображаемое имя
	Ts      string `json:"ts"`      // время точки (RFC3339)
	bmsAveraged
}
