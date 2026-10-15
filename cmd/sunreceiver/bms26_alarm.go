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

import "fmt"

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

// decodeBms26Alarms возвращает человекочитаемые тревоги по типу BMS.
func decodeBms26Alarms(sn bms26Snapshot) []string {
	switch sn.BmsType {
	case "AntBMS-DSP":
		return decodeAntBms26Alarms(sn.AlarmBytes)
	case "EnBMS-RS485":
		return decodeEnBms26Alarms(sn)
	default:
		return nil
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
	// Коды MOS: 0=выкл, 1=вкл — норма; всё прочее — тревога/неисправность.
	// Незнакомые коды не теряем (иначе часть ошибок ANT была бы невидима).
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
	// system log: биты 12..27 (u16, LSB-first) — показываем всегда, когда ненулевой.
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

// decodeEnBms26Alarms — ячейки/датчики (4 бита) + Ext_Bit Byte0..5 EnBMS.
func decodeEnBms26Alarms(sn bms26Snapshot) []string {
	a := sn.AlarmBytes
	var out []string
	for i := 0; i < sn.CellCount && i < 16; i++ {
		for _, s := range enbmsCellStateBits(alarmNibble(a, i*4)) {
			out = append(out, fmt.Sprintf("Ячейка %d: %s", i+1, s))
		}
	}
	for i := 0; i < sn.TempCount && i < 8; i++ {
		for _, s := range enbmsTempStateBits(alarmNibble(a, 64+i*4)) {
			out = append(out, fmt.Sprintf("Датчик %d: %s", i+1, s))
		}
	}
	// Одиночные warn (биты 96..99): env, power, chargecurrent, customer.
	single := map[int]string{
		96: "Температура среды: предупреждение",
		97: "Силовая часть: предупреждение",
		98: "Ток заряда: предупреждение",
		99: "Пользовательский вход: предупреждение",
	}
	for bit := 96; bit <= 99; bit++ {
		if alarmBit(a, bit) {
			out = append(out, single[bit])
		}
	}
	// Ext_Bit Byte0..5 → группы 0..5; берём только Warn/Protect (как parseEnBmsState).
	for by := 0; by < 6; by++ {
		grp, ok := enbmsExtGroups[by]
		if !ok {
			continue
		}
		for _, eb := range grp.bits {
			if eb.typ != "Warn" && eb.typ != "Protect" {
				continue
			}
			if alarmBit(a, 100+by*8+eb.bit) {
				out = append(out, eb.name)
			}
		}
	}
	return out
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
