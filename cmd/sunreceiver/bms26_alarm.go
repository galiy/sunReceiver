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
	"strings"
)

// Трактовка бинарного `alarm` (19 Б) контракта BMS по типу устройства.
// Раскладка — зеркало упаковки BMS-Receiver (malina-26): биты нумеруются от 0,
// setAlarmBit → a[bit/8] |= 1<<(bit%8) (LSB-first внутри байта).
//
//   - AntBMS-DSP (docs/bms/antbms-dsp.md §«Раскладка alarm»):
//       биты 0..3   — код charge_mos (0..15)
//       биты 4..7   — код discharge_mos (0..15)
//       биты 8..11  — код balancer (0..10)
//       биты 12..27 — system log (u16, бит k → alarm bit 12+k)
//   - EnBMS-RS485 (docs/bms/enbms-rs485.md §«Раскладка alarm»):
//       биты 0..63   — ячейки, 16×4 (HVwarn, OVprot, LVwarn, UVprot)
//       биты 64..95  — температуры, 8×4 (тот же порядок)
//       биты 96..99  — одиночные warn (env, power, chargecurrent, customer)
//       биты 100..146— Ext_Bit Byte0..5 (Byte5 — биты 0..6)
//
// Значения «нет тревоги» — 0 (для кодов MOS AntBMS норма = 0/1). Тексты — как у
// прежних BMS-страниц (переиспользуем те же функции/таблицы).

// alarmBit — установлен ли бит bit в 19-байтном alarm.
func alarmBit(a []int, bit int) bool {
	if bit < 0 || bit/8 >= len(a) {
		return false
	}
	return a[bit/8]&(1<<uint(bit%8)) != 0
}

// alarmNibble — 4-битное значение (nibble) начиная с бита base.
func alarmNibble(a []int, base int) int {
	v := 0
	for b := 0; b < 4; b++ {
		if alarmBit(a, base+b) {
			v |= 1 << uint(b)
		}
	}
	return v
}

// bmsSubtype — подтип в рамках единого контракта BMS26 (поле bms_type). От
// подтипа зависят раскладка тревог/защит/ошибок.
type bmsSubtype string

const (
	bmsSubtypeAntDSP     bmsSubtype = "AntBMS-DSP"
	bmsSubtypeEnbmsRS485 bmsSubtype = "EnBMS-RS485"
	bmsSubtypeUnknown    bmsSubtype = "unknown"
)

func bmsSubtypeOf(bmsType string) bmsSubtype {
	switch strings.TrimSpace(bmsType) {
	case string(bmsSubtypeAntDSP):
		return bmsSubtypeAntDSP
	case string(bmsSubtypeEnbmsRS485):
		return bmsSubtypeEnbmsRS485
	default:
		return bmsSubtypeUnknown
	}
}

// decodeBms26Alarms типозависимо разбивает alarm на предупреждения (alarms) и
// защиты (protections). У AntBMS отдельных «защит» нет — всё в alarms.
func decodeBms26Alarms(sn bms26Snapshot) (alarms, protections []string) {
	switch bmsSubtypeOf(sn.BmsType) {
	case bmsSubtypeAntDSP:
		return decodeAntBms26Alarms(sn.AlarmBytes), nil
	case bmsSubtypeEnbmsRS485:
		return decodeEnBms26Alarms(sn)
	default:
		return nil, nil
	}
}

// decodeAntBms26Alarms — коды MOS (4 бита) + system log (16 бит) AntBMS.
func decodeAntBms26Alarms(a []int) []string {
	charge := map[int]string{
		2: "Overvoltage protection", 3: "Over current protection", 5: "Total overpressure",
		6: "Battery overtemperature", 7: "Power overtemperature", 8: "Abnormal current",
		9: "Balanced line dropped", 10: "Motherboard overtemperature", 13: "Discharge tube abnormality",
	}
	disch := map[int]string{
		2: "Over-discharge protection", 3: "Over current protection", 5: "Total undervoltage",
		6: "Battery overtemperature", 7: "Power overtemperature", 8: "Abnormal current",
		9: "Balanced line dropped", 10: "Motherboard overtemperature", 12: "Short circuit protection",
		13: "Discharge tube abnormality", 14: "Start exception",
	}
	bal := map[int]string{3: "Balance overtemperature", 10: "Motherboard overtemperature"}
	var out []string
	pick := func(code int, m map[int]string) string {
		if s, ok := m[code]; ok {
			return s
		}
		return fmt.Sprintf("code %d", code)
	}
	if c := alarmNibble(a, 0); c >= 2 {
		out = append(out, "Charge: "+pick(c, charge))
	}
	if c := alarmNibble(a, 4); c >= 2 {
		out = append(out, "Discharge: "+pick(c, disch))
	}
	if c := alarmNibble(a, 8); c >= 2 {
		out = append(out, "Balance: "+pick(c, bal))
	}
	sys := 0
	for k := 0; k < 16; k++ {
		if alarmBit(a, 12+k) {
			sys |= 1 << uint(k)
		}
	}
	if sys != 0 {
		out = append(out, fmt.Sprintf("System log: status=%d battery=%d order=%d chg/dis=%d",
			sys&0x1F, (sys>>5)&0x1F, (sys>>10)&0x1F, (sys>>15)&1))
	}
	return out
}

// enbmsCellBitText/enbmsTempBitText — текст и класс (protect) по биту ниббла.
var enbmsCellBitText = [4]struct {
	text string
	prot bool
}{
	{"высокое напряжение (предупреждение)", false},
	{"защита от перенапряжения", true},
	{"низкое напряжение (предупреждение)", false},
	{"защита от пониженного напряжения", true},
}

var enbmsTempBitText = [4]struct {
	text string
	prot bool
}{
	{"перегрев (предупреждение)", false},
	{"защита от перегрева", true},
	{"переохлаждение (предупреждение)", false},
	{"защита от переохлаждения", true},
}

// decodeEnBms26Alarms — ячейки/датчики (ниббл: 0/2 — warn, 1/3 — protect),
// одиночные warn (96..99), Ext_Bit Byte0..5 (Warn→alarms, Protect→protections).
func decodeEnBms26Alarms(sn bms26Snapshot) (alarms, protections []string) {
	a := sn.AlarmBytes
	for i := 0; i < sn.CellCount && i < 16; i++ {
		v := alarmNibble(a, i*4)
		for b := 0; b < 4; b++ {
			if v&(1<<uint(b)) == 0 {
				continue
			}
			msg := fmt.Sprintf("Ячейка %d: %s", i+1, enbmsCellBitText[b].text)
			if enbmsCellBitText[b].prot {
				protections = append(protections, msg)
			} else {
				alarms = append(alarms, msg)
			}
		}
	}
	for i := 0; i < sn.TempCount && i < 8; i++ {
		v := alarmNibble(a, 64+i*4)
		for b := 0; b < 4; b++ {
			if v&(1<<uint(b)) == 0 {
				continue
			}
			msg := fmt.Sprintf("Датчик %d: %s", i+1, enbmsTempBitText[b].text)
			if enbmsTempBitText[b].prot {
				protections = append(protections, msg)
			} else {
				alarms = append(alarms, msg)
			}
		}
	}
	single := map[int]string{
		96: "Температура среды: предупреждение",
		97: "Силовая часть: предупреждение",
		98: "Ток заряда: предупреждение",
		99: "Пользовательский вход: предупреждение",
	}
	for bit := 96; bit <= 99; bit++ {
		if alarmBit(a, bit) {
			alarms = append(alarms, single[bit])
		}
	}
	for by := 0; by < 6; by++ {
		grp, ok := enbmsExtGroups[by]
		if !ok {
			continue
		}
		for _, eb := range grp.bits {
			if !alarmBit(a, 100+by*8+eb.bit) {
				continue
			}
			switch eb.typ {
			case "Warn":
				alarms = append(alarms, eb.name)
			case "Protect":
				protections = append(protections, eb.name)
			}
		}
	}
	return alarms, protections
}

// bms26CellMasks возвращает по-ячеечные маски тревог (бит i = ячейка i+1):
// warn — предупреждение, prot — защита. Значимо только для EnBMS (EnBMS-RS485:
// биты ниббла ячейки 0=HVwarn, 1=OVprot, 2=LVwarn, 3=UVprot); для AntBMS — нули
// (по-ячеечных бит в alarm нет).
func bms26CellMasks(sn bms26Snapshot) (warn, prot uint32) {
	if sn.BmsType != "EnBMS-RS485" {
		return 0, 0
	}
	n := sn.CellCount
	if n > 32 {
		n = 32
	}
	for i := 0; i < n; i++ {
		v := alarmNibble(sn.AlarmBytes, i*4)
		if v&0x1 != 0 || v&0x4 != 0 {
			warn |= 1 << uint(i)
		}
		if v&0x2 != 0 || v&0x8 != 0 {
			prot |= 1 << uint(i)
		}
	}
	return warn, prot
}

// bms26TempMasks — по-датчиковые маски (бит i = датчик i+1): warn/prot.
// Только EnBMS-RS485 (ниббл датчика: 0=перегрев warn, 1=защита от перегрева,
// 2=переохлаждение warn, 3=защита от переохлаждения); для AntBMS — нули.
func bms26TempMasks(sn bms26Snapshot) (warn, prot uint32) {
	if sn.BmsType != "EnBMS-RS485" {
		return 0, 0
	}
	n := sn.TempCount
	if n > 8 {
		n = 8
	}
	for i := 0; i < n; i++ {
		v := alarmNibble(sn.AlarmBytes, 64+i*4)
		if v&0x1 != 0 || v&0x4 != 0 {
			warn |= 1 << uint(i)
		}
		if v&0x2 != 0 || v&0x8 != 0 {
			prot |= 1 << uint(i)
		}
	}
	return warn, prot
}
