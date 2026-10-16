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
	"log"
	"sync"
	"time"
)

// КОСТЫЛЬ: если временно включаются СТАРЫЕ пулеры BMS (ANT read_bms.php / EnBMS
// BLE/RS485 напрямую), они пишут не в старые ключи, а в НОВЫЕ ряды bms26
// (sunreceiver:bms26:current / :series / PG bms26_averages), чтобы данные легли в
// те же ряды, что и у пулера malina26.
//
// Соответствие «старое устройство → новый serial» — по документированному правилу
// (cleaning.malina docs/bms): ANT — BASE=AntBMS-DSP-<ёмкость>, EnBMS — BASE=<SN>.
// Конкретные ID берём из текущего состава bms26 (кэш serial по BASE), чтобы суффикс
// -NNNNN совпал с тем, что пишет malina26.

var bms26Serials = struct {
	mu     sync.Mutex
	byBase map[string]string // BASE ("AntBMS-DSP-160") -> serial ("AntBMS-DSP-160-00001")
	enbms  string            // единственный EnBMS-serial
}{byBase: map[string]string{}}

// cacheBms26Snapshot обновляет кэш serial по снимку bms26.
func cacheBms26Snapshot(sn bms26Snapshot) {
	if sn.Serial == "" {
		return
	}
	bms26Serials.mu.Lock()
	defer bms26Serials.mu.Unlock()
	if sn.Base != "" {
		bms26Serials.byBase[sn.Base] = sn.Serial
	}
	if sn.BmsType == "EnBMS-RS485" {
		bms26Serials.enbms = sn.Serial
	}
}

// loadBms26SerialCache наполняет кэш из текущего состава bms26 (при старте).
func loadBms26SerialCache(store *redisStore) {
	if store == nil {
		return
	}
	snaps, err := store.Bms26Current()
	if err != nil {
		return
	}
	for _, sn := range snaps {
		cacheBms26Snapshot(sn)
	}
}

// legacyAntSerial — новый serial для ANT-устройства по ёмкости (из старого имени).
func legacyAntSerial(cap int) string {
	bms26Serials.mu.Lock()
	defer bms26Serials.mu.Unlock()
	return bms26Serials.byBase["AntBMS-DSP-"+itoa(cap)]
}

// legacyEnbmsSerial — новый serial для EnBMS (единственный).
func legacyEnbmsSerial() string {
	bms26Serials.mu.Lock()
	defer bms26Serials.mu.Unlock()
	return bms26Serials.enbms
}

// legacyModeFromCurrent — канонический mode по знаку тока (разряд «+», заряд «−»).
func legacyModeFromCurrent(currentA float64) int {
	switch {
	case currentA > 0:
		return 1 // разряд
	case currentA < 0:
		return 2 // заряд
	default:
		return 0 // ожидание
	}
}

// legacyModeFromText — RU-текст режима EnBMS → канонический код.
func legacyModeFromText(s string) int {
	for b, name := range enbmsModeNames {
		if name == s {
			switch b {
			case 0x01:
				return 1
			case 0x02:
				return 2
			case 0x04:
				return 3
			case 0x08:
				return 4
			case 0x20:
				return 5
			default:
				return 0
			}
		}
	}
	return 0
}

// antDeviceToBms26 — старый ANT-снимок (bmsDevice) → bms26Snapshot с новым serial.
func antDeviceToBms26(d bmsDevice, serial string) bms26Snapshot {
	flags := 0
	if d.ChargeMos != 0 {
		flags |= 1
	}
	if d.DischargeMos != 0 {
		flags |= 2
	}
	if d.Balancer != 0 {
		flags |= 4
	}
	return bms26Snapshot{
		Serial: serial, Base: d.DeviceName, BmsType: "AntBMS-DSP", DeviceName: serial,
		Timestamp: time.Now().Format(time.RFC3339),
		CellCount: d.CellCount, CellsV: d.CellsV, TemperaturesC: d.TemperaturesC,
		CurrentA: d.CurrentA, PowerW: d.PowerW, Soc: float64(d.Soc), Soh: d.Soh,
		CapacityAh: d.CapacityAh, RemainingAh: d.RemainingAh, Cycles: d.Cycles,
		Flags: flags, BalanceMask: d.BalanceMask, Mode: legacyModeFromCurrent(d.CurrentA),
		MaxCellV: d.MaxCellV, MinCellV: d.MinCellV, AvgCellV: d.AvgCellV,
		Alarms: d.Alarms,
	}
}

// enbmsSnapshotToBms26 — старый EnBMS-снимок → bms26Snapshot с новым serial.
func enbmsSnapshotToBms26(s enbmsSnapshot, serial string) bms26Snapshot {
	sn := bms26Snapshot{
		Serial: serial, BmsType: "EnBMS-RS485", DeviceName: serial,
		Timestamp: s.Timestamp, CellCount: s.CellCount, CellsV: s.CellsV,
		TemperaturesC: s.TemperaturesC, CurrentA: s.CurrentA, PowerW: s.PowerW,
		Soc: s.Soc, Soh: s.Soh, CapacityAh: s.CapacityAh, RemainingAh: s.RemainingAh,
		TotalV: s.TotalVoltageV, PortV: s.PortVoltageV, Cycles: s.Cycles,
		BalanceMask: enbmsBalanceMask(s.BalanceCells), Mode: legacyModeFromText(s.Mode),
		MaxCellV: s.MaxCellV, MinCellV: s.MinCellV, AvgCellV: s.AvgCellV,
		Alarms: s.Alarms,
	}
	return sn
}

// antAveragedToBms26 — 5-мин среднее ANT → bms26Snapshot (name → serial по ёмкости).
func antAveragedToBms26(name string, a bmsAveraged, ts string) (bms26Snapshot, bool) {
	cap, ok := antCapFromName(name)
	if !ok {
		return bms26Snapshot{}, false
	}
	serial := legacyAntSerial(cap)
	if serial == "" {
		return bms26Snapshot{}, false
	}
	flags := 0
	if a.ChargeMos != 0 {
		flags |= 1
	}
	if a.DischargeMos != 0 {
		flags |= 2
	}
	if a.Balancer != 0 {
		flags |= 4
	}
	return bms26Snapshot{
		Serial: serial, BmsType: "AntBMS-DSP", DeviceName: serial, Timestamp: ts,
		CellCount: a.CellCount, CellsV: a.CellsV, TemperaturesC: a.Temperatures,
		CurrentA: a.CurrentA, PowerW: a.PowerW, Soc: a.Soc,
		CapacityAh: a.CapacityAh, RemainingAh: a.RemainingAh, Flags: flags,
		MaxCellV: a.MaxCellV, MinCellV: a.MinCellV, AvgCellV: a.AvgCellV,
	}, true
}

// enbmsAveragedToBms26 — 5-мин среднее EnBMS → bms26Snapshot (MAC → единственный serial).
func enbmsAveragedToBms26(a enbmsAveraged, ts string) (bms26Snapshot, bool) {
	serial := legacyEnbmsSerial()
	if serial == "" {
		return bms26Snapshot{}, false
	}
	return bms26Snapshot{
		Serial: serial, BmsType: "EnBMS-RS485", DeviceName: serial, Timestamp: ts,
		CellCount: a.CellCount, CellsV: a.CellsV, TemperaturesC: a.Temperatures,
		CurrentA: a.CurrentA, PowerW: a.PowerW, Soc: a.Soc, Soh: a.Soh,
		CapacityAh: a.CapacityAh, RemainingAh: a.RemainingAh,
		TotalV: a.TotalVoltageV, PortV: a.PortVoltageV, Cycles: a.Cycles,
		MaxCellV: a.MaxCellV, MinCellV: a.MinCellV, AvgCellV: a.AvgCellV,
	}, true
}

// itoa — маленький локальный int→string (без импорта strconv в этом файле).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

var _ = log.Printf
