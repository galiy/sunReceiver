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

// bmsDevice — представление BMS-устройства для дашборда/API (единый BMS26:
// ANT + EnBMS). Формируется из снимка bms26 (bmsDeviceFromBms26) и используется
// хранилищем/дашбордом; отдельного legacy-пулера BMS нет.
type bmsDevice struct {
	// Kind — тип BMS: "bms26" (единый BMS26), либо "agm" (синтетическая AGM).
	Kind           string    `json:"kind,omitempty"`
	DeviceName     string    `json:"deviceName"`                 // отображаемое имя (serial/ёмкость)
	Key            string    `json:"key"`                        // ключ устройства (bmsKey): Key или deviceName; для запросов/URL
	Timestamp      int64     `json:"timestamp"`                  // Unix-время последнего валидного кадра
	Time           string    `json:"time"`                       // локальное время HH:MM:SS
	SourceLastSeen int64     `json:"source_last_seen,omitempty"` // last_seen источника (unix seconds)
	Stale          bool      `json:"stale,omitempty"`            // источник устарел (last_seen старше порога)
	DupSerial      int       `json:"dup_serial,omitempty"`       // число устройств с этим serial (>1 — конфликт)
	CellCount      int       `json:"cell_count"`                 // число ячеек (S)
	CellsV         []float64 `json:"cells_v"`                    // напряжения ячеек, V
	TotalV         float64   `json:"total_voltage_v,omitempty"`  // напряжение пакета (контракт total_cv), V
	PortV          float64   `json:"port_voltage_v,omitempty"`   // напряжение порта/клемм (контракт port_cv), V
	CurrentA       float64   `json:"current_a"`                  // ток, A (знаковый)
	Soc            int       `json:"soc"`                        // State of Charge, %
	CapacityAh     float64   `json:"capacity_ah"`                // ёмкость, А·ч
	RemainingAh    float64   `json:"remaining_ah"`               // остаточная ёмкость, А·ч
	TemperaturesC  []float64 `json:"temperatures_c"`             // температуры, °C (до 6 датчиков)
	ChargeMos      int       `json:"charge_mos"`                 // MOS зарядки: 1 = включён
	DischargeMos   int       `json:"discharge_mos"`              // MOS разряда: 1 = включён
	Balancer       int       `json:"balancer"`                   // балансировка: 1 = активна
	PowerW         float64   `json:"power_w"`                    // мощность, W (знаковая)
	MaxCellIdx     int       `json:"max_cell_idx"`               // индекс ячейки с максимальным напряжением
	MaxCellV       float64   `json:"max_cell_v"`                 // напряжение максимальной ячейки, V
	MinCellIdx     int       `json:"min_cell_idx"`               // индекс ячейки с минимальным напряжением
	MinCellV       float64   `json:"min_cell_v"`                 // напряжение минимальной ячейки, V
	AvgCellV       float64   `json:"avg_cell_v"`                 // среднее напряжение ячейки, V
	Frames         uint32    `json:"frames"`                     // счётчик валидных кадров с запуска слушателя
	// Дополнительно (по типам BMS):
	Soh          float64  `json:"soh,omitempty"`            // SOH, % (EnBMS)
	Cycles       int      `json:"cycles,omitempty"`         // число циклов (EnBMS)
	RatedAh      float64  `json:"rated_ah,omitempty"`       // номинальная ёмкость, А·ч (EnBMS/контракт rated_mah)
	CellWarnMask uint32   `json:"cell_warn_mask,omitempty"` // бит i: предупреждение ячейки i+1 (BMS26/EnBMS)
	CellProtMask uint32   `json:"cell_prot_mask,omitempty"` // бит i: защита ячейки i+1 (BMS26/EnBMS)
	TempWarnMask uint32   `json:"temp_warn_mask,omitempty"` // бит i: предупреждение датчика i+1 (BMS26/EnBMS)
	TempProtMask uint32   `json:"temp_prot_mask,omitempty"` // бит i: защита датчика i+1 (BMS26/EnBMS)
	BalanceMask  uint32   `json:"balance_mask,omitempty"`   // маска балансируемых ячеек (ANT, addr 132)
	CycleAh      float64  `json:"cycle_ah,omitempty"`       // суммарная цикловая ёмкость, А·ч (ANT, addr 83)
	Model        string   `json:"model,omitempty"`          // модель/протокол устройства (EnBMS BasicInfo)
	Alarms       []string `json:"alarms,omitempty"`         // активные алармы (человекочитаемо)
	Protections  []string `json:"protections,omitempty"`    // активные защиты (BMS26, по подтипу)
	Mode         string   `json:"mode,omitempty"`           // режим работы BMS (EnBMS, TeleState 0x44)
}

// bmsKey — ключ BMS-устройства для HASH bms26, Redis-ряда, PG (name)
// и дашборда (/api/bms/<name>). Приоритет — уже вычисленный d.Key; иначе —
// deviceName.
func bmsKey(d bmsDevice) string {
	if d.Key != "" {
		return d.Key
	}
	return d.DeviceName
}
