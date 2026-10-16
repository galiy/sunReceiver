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
	"regexp"
	"strconv"
	"sync"
	"time"
)

// antNameCap — ёмкость из старого имени ANT-устройства ("AntBms 160 A/h ...").
var antNameCap = regexp.MustCompile(`AntBms\s+(\d+)\s*A/h`)

// antBaseCap — ёмкость из нового BASE bms26 ("AntBMS-DSP-160").
var antBaseCap = regexp.MustCompile(`AntBMS-DSP-(\d+)`)

func antCapFromName(name string) (int, bool) { return capFromRe(antNameCap, name) }
func antCapFromBase(base string) (int, bool) { return capFromRe(antBaseCap, base) }

func capFromRe(re *regexp.Regexp, s string) (int, bool) {
	m := re.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

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
	sn := bms26Snapshot{
		Serial: serial, Base: d.DeviceName, BmsType: "AntBMS-DSP", DeviceName: serial,
		Timestamp: time.Now().Format(time.RFC3339),
		CellCount: d.CellCount, CellsV: d.CellsV, TemperaturesC: d.TemperaturesC,
		CurrentA: d.CurrentA, PowerW: d.PowerW, Soc: float64(d.Soc), Soh: d.Soh,
		CapacityAh: d.CapacityAh, RemainingAh: d.RemainingAh, Cycles: d.Cycles,
		Flags: flags, BalanceMask: d.BalanceMask, Mode: legacyModeFromCurrent(d.CurrentA),
		AlarmBytes: antAlarmBytesFromCodes(d.ChargeMos, d.DischargeMos, d.Balancer),
		MaxCellV:   d.MaxCellV, MinCellV: d.MinCellV, AvgCellV: d.AvgCellV,
	}
	finalizeBms26(&sn)
	return sn
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
	}
	// Двоичный alarm: прямая копия (собрана EnBMS-парсером из сырых нибблов, RS485
	// 0x44). Если её нет (BLE-путь — 0x44 не читается), биты восстанавливаем из строк
	// хвоста best-effort (для масок), а человекочитаемые Alarms СОХРАНЯЕМ как есть
	// (декод из частично собранных бит их бы потерял).
	if len(s.AlarmBytes) > 0 {
		sn.AlarmBytes = s.AlarmBytes
		finalizeBms26(&sn) // alarms+маски из сырых бит
	} else {
		sn.AlarmBytes = enbmsAlarmBytesFromStrings(s.Alarms, s.CellCount, len(s.TemperaturesC))
		sn.Alarms = s.Alarms
		sn.CellWarnMask, sn.CellProtMask = bms26CellMasks(sn)
		sn.TempWarnMask, sn.TempProtMask = bms26TempMasks(sn)
	}
	return sn
}

// antAveragedToBms26 — 5-мин среднее ANT → bms26Snapshot (name → serial по ёмкости).
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

// ---- Сборка двоичного alarm (19 Б) из старых данных (по упаковке bms-reader) ----

// setLegacyAlarmBit — a[bit/8] |= 1<<(bit%8) (как setAlarmBit в bms-reader).
func setLegacyAlarmBit(a []int, bit int) {
	if bit < 0 || bit/8 >= len(a) {
		return
	}
	a[bit/8] |= 1 << uint(bit%8)
}

// setLegacyAlarmNibble — упаковка 4-битного кода начиная с бита base (setAlarmCode).
func setLegacyAlarmNibble(a []int, base, code int) {
	for b := 0; b < 4; b++ {
		if code&(1<<uint(b)) != 0 {
			setLegacyAlarmBit(a, base+b)
		}
	}
}

// antAlarmBytesFromCodes — alarm_bytes ANT: биты 0..3/4..7/8..11 (коды MOS).
// system log (12..27) в старом снимке отсутствует → 0.
func antAlarmBytesFromCodes(charge, disch, bal int) []int {
	a := make([]int, 19)
	setLegacyAlarmNibble(a, 0, charge)
	setLegacyAlarmNibble(a, 4, disch)
	setLegacyAlarmNibble(a, 8, bal)
	return a
}

// enbmsAlarmBytesFromStrings — реконструкция alarm_bytes EnBMS из строк тревог
// (обратный маппинг к enbmsCellStateBits/enbmsTempStateBits/enbmsExtGroups, те же
// таблицы, что и парсер). Точны для строк RS485-пути (TeleState 0x44).
func enbmsAlarmBytesFromStrings(alarms []string, cellCount, tempCount int) []int {
	a := make([]int, 19)
	cellBit := map[string]int{
		"высокое напряжение (предупреждение)": 0,
		"защита от перенапряжения":            1,
		"низкое напряжение (предупреждение)":  2,
		"защита от пониженного напряжения":    3,
	}
	tempBit := map[string]int{
		"перегрев (предупреждение)":       0,
		"защита от перегрева":             1,
		"переохлаждение (предупреждение)": 2,
		"защита от переохлаждения":        3,
	}
	single := map[string]int{
		"Температура среды: предупреждение":     96,
		"Силовая часть: предупреждение":         97,
		"Ток заряда: предупреждение":            98,
		"Пользовательский вход: предупреждение": 99,
	}
	// Ext_Bit: имя → глобальный бит (100 + by*8 + bit), только Warn/Protect.
	extBit := map[string]int{}
	for by := 0; by < 6; by++ {
		grp, ok := enbmsExtGroups[by]
		if !ok {
			continue
		}
		for _, eb := range grp.bits {
			if eb.typ == "Warn" || eb.typ == "Protect" {
				extBit[eb.name] = 100 + by*8 + eb.bit
			}
		}
	}
	var idx int
	var phrase string
	parseIdx := func(s, prefix string) bool {
		// "Ячейка 12: фраза"
		if len(s) <= len(prefix) || s[:len(prefix)] != prefix {
			return false
		}
		rest := s[len(prefix):]
		colon := -1
		for i := 0; i < len(rest); i++ {
			if rest[i] == ':' {
				colon = i
				break
			}
		}
		if colon < 0 {
			return false
		}
		n := 0
		for i := 0; i < colon; i++ {
			if rest[i] < '0' || rest[i] > '9' {
				return false
			}
			n = n*10 + int(rest[i]-'0')
		}
		idx = n
		phrase = rest[colon+1:]
		if len(phrase) > 0 && phrase[0] == ' ' {
			phrase = phrase[1:]
		}
		return true
	}
	for _, s := range alarms {
		if b, ok := single[s]; ok {
			setLegacyAlarmBit(a, b)
			continue
		}
		if b, ok := extBit[s]; ok {
			setLegacyAlarmBit(a, b)
			continue
		}
		if parseIdx(s, "Ячейка") && idx >= 1 && idx <= cellCount {
			if b, ok := cellBit[phrase]; ok {
				setLegacyAlarmNibble(a, (idx-1)*4, 1<<uint(b))
			}
			continue
		}
		if parseIdx(s, "Датчик") && idx >= 1 && idx <= tempCount {
			if b, ok := tempBit[phrase]; ok {
				setLegacyAlarmNibble(a, 64+(idx-1)*4, 1<<uint(b))
			}
			continue
		}
	}
	return a
}

// finalizeBms26 заполняет производные (alarms+маски) из alarm_bytes/типа.
func finalizeBms26(sn *bms26Snapshot) {
	if len(sn.AlarmBytes) == 0 {
		return
	}
	sn.Alarms = decodeBms26Alarms(*sn)
	sn.CellWarnMask, sn.CellProtMask = bms26CellMasks(*sn)
	sn.TempWarnMask, sn.TempProtMask = bms26TempMasks(*sn)
}
