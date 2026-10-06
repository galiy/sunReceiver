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
	"math"
)

// Трёхфазный счётчик DTS017M (Wenzhou Taiye Electric, серия 7P), Modbus RTU
// (unit 1) через прозрачный шлюз. Карта регистров — по исследованию
// ~/src/DTS-DLT645-2007/dts017m-monitoring.md (все значения при Cr=×1).
//
// Телеметрия (0x0000..0x001C, функция 03/04; регистр 0x0006 пропущен):
//
//	0x00..0x02 Ua/Ub/Uc          UINT ×0.1  В
//	0x03..0x05 Ia/Ib/Ic          UINT ×0.01 А (модуль)
//	0x07..0x0A ΣP/Pa/Pb/Pc       INT16     Вт (знак = направление)
//	0x0B..0x0E ΣQ/Qa/Qb/Qc       INT16     вар
//	0x0F..0x12 ΣS/Sa/Sb/Sc       UINT      ВА
//	0x13..0x16 cosφ Σ/A/B/C      UINT ×0.001
//	0x17..0x19 Uab/Ubc/Uca       UINT ×0.1  В
//	0x1A..0x1C Fa/Fb/Fc          UINT ×0.01 Гц
//
// Энергии (Long = 2 регистра, старшее слово первым, ×0.01 кВт·ч/квар·ч):
//
//	0x001D общая активная (Σ)
//	0x0027 прямая (import) суммарная активная
//	0x0031 обратная (export) суммарная активная
//	0x003B общая реактивная (Σ)
//	0x0045 прямая суммарная реактивная
//	0x004F обратная суммарная реактивная
//
// Историю счётчика (0x0300+/0x0400+) НЕ читаем — статистику потребления/отдачи
// считаем сами по границам тарифных зон (см. dts017_tariff.go), как у DDS238.
const (
	dts017RegTelemetry   = 0x0000
	dts017TelemetryCount = 29 // 0x00..0x1C (29 регистров, 0x06 пропущен)
	dts017RegEnergy      = 0x001D
	dts017EnergyCount    = 52 // 0x1D..0x50: покрывает 0x4F/0x50 (обратная реактив.)
)

// Смещения энергий внутри блока dts017RegEnergy.
const (
	dts017OffTotalActive    = 0x001D - dts017RegEnergy // 0
	dts017OffImportActive   = 0x0027 - dts017RegEnergy // 10
	dts017OffExportActive   = 0x0031 - dts017RegEnergy // 20
	dts017OffTotalReactive  = 0x003B - dts017RegEnergy // 30
	dts017OffImportReactive = 0x0045 - dts017RegEnergy // 40
	dts017OffExportReactive = 0x004F - dts017RegEnergy // 50
)

// Теги контракта DTS017M. Префикс dts017_ — обособлен от общего контракта values
// инверторов/МАП/DDS238; снимок счётчика хранится в собственных ключах Redis и
// собственной таблице PG (см. dts017_store.go), сериализуется как обычная map.
const (
	dts017VoltageA  = "dts017_voltage_a"
	dts017VoltageB  = "dts017_voltage_b"
	dts017VoltageC  = "dts017_voltage_c"
	dts017VoltageAB = "dts017_voltage_ab"
	dts017VoltageBC = "dts017_voltage_bc"
	dts017VoltageCA = "dts017_voltage_ca"

	dts017CurrentA = "dts017_current_a"
	dts017CurrentB = "dts017_current_b"
	dts017CurrentC = "dts017_current_c"

	dts017ActivePower    = "dts017_active_power"
	dts017ActivePowerA   = "dts017_active_power_a"
	dts017ActivePowerB   = "dts017_active_power_b"
	dts017ActivePowerC   = "dts017_active_power_c"
	dts017ReactivePower  = "dts017_reactive_power"
	dts017ReactivePowerA = "dts017_reactive_power_a"
	dts017ReactivePowerB = "dts017_reactive_power_b"
	dts017ReactivePowerC = "dts017_reactive_power_c"
	dts017ApparentPower  = "dts017_apparent_power"
	dts017ApparentPowerA = "dts017_apparent_power_a"
	dts017ApparentPowerB = "dts017_apparent_power_b"
	dts017ApparentPowerC = "dts017_apparent_power_c"

	dts017PowerFactor  = "dts017_power_factor"
	dts017PowerFactorA = "dts017_power_factor_a"
	dts017PowerFactorB = "dts017_power_factor_b"
	dts017PowerFactorC = "dts017_power_factor_c"

	dts017FrequencyA = "dts017_frequency_a"
	dts017FrequencyB = "dts017_frequency_b"
	dts017FrequencyC = "dts017_frequency_c"

	dts017Import         = "dts017_import"          // кВт·ч, прямая (потребление из сети)
	dts017Export         = "dts017_export"          // кВт·ч, обратная (отдача в сеть)
	dts017Total          = "dts017_total"           // кВт·ч, общая активная
	dts017ReactiveTotal  = "dts017_reactive_total"  // квар·ч, общая реактивная
	dts017ImportReactive = "dts017_import_reactive" // квар·ч, прямая реактивная
	dts017ExportReactive = "dts017_export_reactive" // квар·ч, обратная реактивная
)

// dts017CumulativeTags — накопительные (монотонно растущие) величины: при
// усреднении в PG берётся ПОСЛЕДНЕЕ значение окна, а не среднее (см.
// dts017_accumulator.go). Список — теги энергий.
var dts017CumulativeTags = map[string]bool{
	dts017Import:         true,
	dts017Export:         true,
	dts017Total:          true,
	dts017ReactiveTotal:  true,
	dts017ImportReactive: true,
	dts017ExportReactive: true,
}

// dts017Snapshot — снимок счётчика DTS017M для собственных ключей Redis
// (HASH current и месячный ряд). Values — плоская map тегов, сериализуется как есть.
type dts017Snapshot struct {
	Name      string             `json:"name"`
	Timestamp string             `json:"timestamp"`
	Values    map[string]float64 `json:"values"`
}

// dts017Readings — накопительные показания для тарифного захвата (kWh).
type dts017Readings struct {
	Import float64
	Export float64
}

// roundDts017 округляет до digits знаков после запятой.
func roundDts017(v float64, digits int) float64 {
	m := math.Pow(10, float64(digits))
	return math.Round(v*m) / m
}

// decodeDts017Telemetry разбирает блок телеметрии (0x0000, dts017TelemetryCount).
func decodeDts017Telemetry(regs []uint16) map[string]float64 {
	out := map[string]float64{}
	if len(regs) < dts017TelemetryCount {
		return out
	}
	u := func(i int) float64 { return float64(regs[i]) }
	i16 := func(i int) float64 { return float64(int16(regs[i])) }
	// Напряжения фазные (×0.1 В).
	out[dts017VoltageA] = roundDts017(u(0x00)/10, 1)
	out[dts017VoltageB] = roundDts017(u(0x01)/10, 1)
	out[dts017VoltageC] = roundDts017(u(0x02)/10, 1)
	// Токи (×0.01 А, модуль).
	out[dts017CurrentA] = roundDts017(u(0x03)/100, 2)
	out[dts017CurrentB] = roundDts017(u(0x04)/100, 2)
	out[dts017CurrentC] = roundDts017(u(0x05)/100, 2)
	// Активная мощность (INT16, Вт).
	out[dts017ActivePower] = roundDts017(i16(0x07), 1)
	out[dts017ActivePowerA] = roundDts017(i16(0x08), 1)
	out[dts017ActivePowerB] = roundDts017(i16(0x09), 1)
	out[dts017ActivePowerC] = roundDts017(i16(0x0A), 1)
	// Реактивная мощность (INT16, вар).
	out[dts017ReactivePower] = roundDts017(i16(0x0B), 1)
	out[dts017ReactivePowerA] = roundDts017(i16(0x0C), 1)
	out[dts017ReactivePowerB] = roundDts017(i16(0x0D), 1)
	out[dts017ReactivePowerC] = roundDts017(i16(0x0E), 1)
	// Полная мощность (UINT, ВА).
	out[dts017ApparentPower] = u(0x0F)
	out[dts017ApparentPowerA] = u(0x10)
	out[dts017ApparentPowerB] = u(0x11)
	out[dts017ApparentPowerC] = u(0x12)
	// Коэффициент мощности (×0.001).
	out[dts017PowerFactor] = roundDts017(u(0x13)/1000, 3)
	out[dts017PowerFactorA] = roundDts017(u(0x14)/1000, 3)
	out[dts017PowerFactorB] = roundDts017(u(0x15)/1000, 3)
	out[dts017PowerFactorC] = roundDts017(u(0x16)/1000, 3)
	// Линейные напряжения (×0.1 В).
	out[dts017VoltageAB] = roundDts017(u(0x17)/10, 1)
	out[dts017VoltageBC] = roundDts017(u(0x18)/10, 1)
	out[dts017VoltageCA] = roundDts017(u(0x19)/10, 1)
	// Частоты каналов (×0.01 Гц).
	out[dts017FrequencyA] = roundDts017(u(0x1A)/100, 2)
	out[dts017FrequencyB] = roundDts017(u(0x1B)/100, 2)
	out[dts017FrequencyC] = roundDts017(u(0x1C)/100, 2)
	return out
}

// decodeDts017Energy разбирает блок энергий (0x001D, dts017EnergyCount). Возвращает
// карту энергетических тегов и накопительные показания Import/Export (для тарифов).
func decodeDts017Energy(regs []uint16) (map[string]float64, dts017Readings) {
	out := map[string]float64{}
	var r dts017Readings
	if len(regs) < dts017EnergyCount {
		return out, r
	}
	// Long: старшее слово первым, ×0.01.
	lng := func(off int) float64 {
		hi := float64(regs[off])
		lo := float64(regs[off+1])
		return (hi*65536 + lo) / 100
	}
	out[dts017Total] = roundDts017(lng(dts017OffTotalActive), 2)
	out[dts017Import] = roundDts017(lng(dts017OffImportActive), 2)
	out[dts017Export] = roundDts017(lng(dts017OffExportActive), 2)
	out[dts017ReactiveTotal] = roundDts017(lng(dts017OffTotalReactive), 2)
	out[dts017ImportReactive] = roundDts017(lng(dts017OffImportReactive), 2)
	out[dts017ExportReactive] = roundDts017(lng(dts017OffExportReactive), 2)
	r.Import = out[dts017Import]
	r.Export = out[dts017Export]
	return out, r
}

// decodeDts017 объединяет телеметрию и энергии в один снимок values.
func decodeDts017(tele, energy []uint16) (map[string]float64, dts017Readings) {
	vals := decodeDts017Telemetry(tele)
	en, r := decodeDts017Energy(energy)
	for k, v := range en {
		vals[k] = v
	}
	return vals, r
}
