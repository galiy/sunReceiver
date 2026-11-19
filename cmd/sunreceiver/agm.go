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
	"math"
	"strings"
	"time"
)

// Мониторинг AGM-сборки: PZEM-017 (RS485/CH340) + 4×BK300 (BLE). Один полный
// цикл опроса пяти источников = один снимок с ОДНОЙ меткой времени (момент
// барьера). Неполный цикл не пишется никуда (решение оператора).
// Документация — docs/modules/agm-monitor.md.

// Теги контракта AGM. Префиксы pzem_/bk300_ — обособлены от общего контракта
// values; снимок хранится в собственных ключах Redis и таблице PG.
const (
	agmTagPZEMVoltage   = "pzem_voltage_v"
	agmTagPZEMCurrent   = "pzem_current_a"
	agmTagPZEMPower     = "pzem_power_w"
	agmTagPZEMEnergy    = "pzem_energy_wh"
	agmTagPZEMAlarmHigh = "pzem_alarm_high"
	agmTagPZEMAlarmLow  = "pzem_alarm_low"
)

// agmBKTag — имя тега BK300 по позиции (1..4) и суффиксу.
func agmBKTag(pos int, suffix string) string {
	return fmt.Sprintf("bk300_%d_%s", pos, suffix)
}

const (
	agmSuffixVoltage = "voltage_v"
	agmSuffixFlagsA  = "flags_a"
	agmSuffixFlagsB  = "flags_b"
)

// agmPZEMAvgMetrics — метрики таблицы sunreceiver.agm_pzem_averages_r.
func agmPZEMAvgMetrics() []string {
	return []string{
		agmTagPZEMVoltage, agmTagPZEMCurrent, agmTagPZEMPower, agmTagPZEMEnergy,
		agmTagPZEMAlarmHigh, agmTagPZEMAlarmLow,
	}
}

// agmBKAvgMetrics — метрики таблицы sunreceiver.agm_bk300_averages_r.
func agmBKAvgMetrics() []string {
	out := make([]string, 0, 12)
	for pos := 1; pos <= 4; pos++ {
		out = append(out,
			agmBKTag(pos, agmSuffixVoltage),
			agmBKTag(pos, agmSuffixFlagsA),
			agmBKTag(pos, agmSuffixFlagsB),
		)
	}
	return out
}

// agmCumulativeTags — накопительные величины: в PG берётся ПОСЛЕДНЕЕ значение окна.
var agmCumulativeTags = map[string]bool{
	agmTagPZEMEnergy: true,
}

// agmLastValueTags — сырые статусные значения: в PG берётся ПОСЛЕДНЕЕ значение
// окна (усреднять флаги/битовые поля смысла нет).
var agmLastValueTags = map[string]bool{
	agmBKTag(1, agmSuffixFlagsA): true, agmBKTag(1, agmSuffixFlagsB): true,
	agmBKTag(2, agmSuffixFlagsA): true, agmBKTag(2, agmSuffixFlagsB): true,
	agmBKTag(3, agmSuffixFlagsA): true, agmBKTag(3, agmSuffixFlagsB): true,
	agmBKTag(4, agmSuffixFlagsA): true, agmBKTag(4, agmSuffixFlagsB): true,
}

// agmAlarmTags — флаги аварий PZEM: в PG 1, если в окне была хотя бы одна 1
// (логическое ИЛИ), иначе 0.
var agmAlarmTags = map[string]bool{
	agmTagPZEMAlarmHigh: true,
	agmTagPZEMAlarmLow:  true,
}

// agmPZEMDeviceKey / agmBKDeviceKey — стабильные идентификаторы источников AGM
// во всех новых хранилищах (current, series, PG). Разделены, чтобы PZEM и BLE
// жили независимо.
const (
	agmPZEMDeviceKey = "agm-pzem"
	agmBKDeviceKey   = "agm-bk300"

	agmPZEMAvgTable = "sunreceiver.agm_pzem_averages_r"
	agmBKAvgTable   = "sunreceiver.agm_bk300_averages_r"
)

// agmSnapshot — один полный снимок сборки. Values — плоская карта тегов.
type agmSnapshot struct {
	Name      string             `json:"name"`
	Timestamp string             `json:"timestamp"`
	Values    map[string]float64 `json:"values"`
}

// agmPZEMReading — измерения PZEM-017.
type agmPZEMReading struct {
	VoltageV  float64
	CurrentA  float64
	PowerW    float64
	EnergyWh  uint32
	AlarmHigh bool
	AlarmLow  bool
}

// agmBKReading — измерения одного BK300.
type agmBKReading struct {
	VoltageV float64
	FlagsA   byte
	FlagsB   byte
}

// agmBKSample — результат опроса одного BK300 в цикле (для сборки снимка).
type agmBKSample struct {
	Position int
	Reading  agmBKReading
}

// roundAGM округляет до digits знаков после запятой.
func roundAGM(v float64, digits int) float64 {
	m := math.Pow(10, float64(digits))
	return math.Round(v*m) / m
}

// agmRoundTag — округление значения тега по правилам ТЗ (§5.1): напряжение и
// ток — 2 знака, мощность — 1, энергия/флаги/аварии — целые.
func agmRoundTag(tag string, v float64) float64 {
	switch {
	case agmLastValueTags[tag], agmAlarmTags[tag], tag == agmTagPZEMEnergy:
		return math.Round(v)
	case tag == agmTagPZEMPower:
		return roundAGM(v, 1)
	case strings.HasSuffix(tag, "_voltage_v"), strings.HasSuffix(tag, "_current_a"):
		return roundAGM(v, 2)
	default:
		return roundAGM(v, 2)
	}
}

// agmTimestamp — время снимка в RFC3339 (локальная зона сервера, как у остальных
// модулей; на проде — MSK).
func agmTimestamp(t time.Time) string { return t.Format(time.RFC3339) }

// agmApplySign приводит измерения PZEM (по модулю) к соглашению «заряд −,
// разряд +» по напряжению АКБ МАП. Если UАКБ МАП > U сборки — заряд.
func agmApplySign(p agmPZEMReading, mapV float64) agmPZEMReading {
	p.CurrentA = math.Abs(p.CurrentA)
	p.PowerW = math.Abs(p.PowerW)
	if mapV > 0 && p.VoltageV > 0 && mapV > p.VoltageV {
		p.CurrentA = -p.CurrentA
		p.PowerW = -p.PowerW
	}
	// Не отдавать отрицательный ноль («-0» в плашках и JSON).
	if p.CurrentA == 0 {
		p.CurrentA = 0
	}
	if p.PowerW == 0 {
		p.PowerW = 0
	}
	return p
}

// agmBuildPZEMValues собирает карту тегов PZEM.
func agmBuildPZEMValues(p agmPZEMReading) map[string]float64 {
	out := map[string]float64{
		agmTagPZEMVoltage:   roundAGM(p.VoltageV, 2),
		agmTagPZEMCurrent:   roundAGM(p.CurrentA, 2),
		agmTagPZEMPower:     roundAGM(p.PowerW, 1),
		agmTagPZEMEnergy:    float64(p.EnergyWh),
		agmTagPZEMAlarmHigh: 0,
		agmTagPZEMAlarmLow:  0,
	}
	if p.AlarmHigh {
		out[agmTagPZEMAlarmHigh] = 1
	}
	if p.AlarmLow {
		out[agmTagPZEMAlarmLow] = 1
	}
	return out
}

// agmBuildBKValues собирает карту тегов доступных BK300.
func agmBuildBKValues(bks []agmBKSample) map[string]float64 {
	out := make(map[string]float64, len(bks)*3)
	for _, s := range bks {
		out[agmBKTag(s.Position, agmSuffixVoltage)] = roundAGM(s.Reading.VoltageV, 2)
		out[agmBKTag(s.Position, agmSuffixFlagsA)] = float64(s.Reading.FlagsA)
		out[agmBKTag(s.Position, agmSuffixFlagsB)] = float64(s.Reading.FlagsB)
	}
	return out
}

// agmBuildValues собирает полный снимок (для тестов/совместимости).
func agmBuildValues(p agmPZEMReading, bks []agmBKSample) map[string]float64 {
	out := agmBuildPZEMValues(p)
	for k, v := range agmBuildBKValues(bks) {
		out[k] = v
	}
	return out
}
