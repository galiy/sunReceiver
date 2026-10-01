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
	"bytes"
	"fmt"
	"log"
	"math"
	"strings"
	"time"
)

// Модуль мониторинга BMS EnBMS (Enjie EMU1101/1102/1103, бренд N ENERGY).
// Протокол и живой срез: /home/sasha/src/energybms (PROTOCOL.md,
// DEVICE_SNAPSHOT.md). Опрос — по BLE, читается ТОЛЬКО блок Battery (CID2 0x61).
// Принципы работы с BLE (соединение, поддержание, восстановление, ошибки)
// зеркалят модуль CE308; схема опроса/хранения/усреднения — модуль ANT BMS.

// logEnBms — единая точка логирования модуля EnBMS.
func logEnBms(f string, a ...any) {
	log.Printf("enbms: "+f, a...)
}

// enBmsSection — раздел "enBms" sunReceiver.json: коллекция настроек
// мониторинга BMS типа EnBMS. Общий disabled — ОБЯЗАТЕЛЬНОЕ поле: false —
// устройства опрашиваются; true — опрос EnBMS отключён целиком.
type enBmsSection struct {
	Disabled *bool                `json:"disabled"`
	Devices  []enBmsDeviceSection `json:"devices"`
}

// enBmsDeviceSection — один элемент коллекции: mac и/или имя опрашиваемого
// устройства + обязательный disabled. MAC нужен для BLE-подключения (заводское
// BLE-имя "BP00" не уникально и не позволяет надёжно отличить устройства).
type enBmsDeviceSection struct {
	Name     string `json:"name"` // отображаемое имя (необязательно)
	MAC      string `json:"mac"`  // BD_ADDR устройства (обязателен для активного)
	Disabled *bool  `json:"disabled"`
}

// enBmsConfig — проверенный конфиг EnBMS (только активные устройства).
type enBmsConfig struct {
	Devices []enBmsDeviceConfig
}

// enBmsDeviceConfig — одно активное устройство EnBMS.
type enBmsDeviceConfig struct {
	Name string // отображаемое имя
	MAC  string // ключ устройства в Redis/PG (уникален)
}

// Теги протокола EnBMS (CID, транспорт).
const (
	// enBmsSvcUUID — GATT-сервис EnBMS.
	enBmsSvcUUID = "0000ff00-0000-1000-8000-00805f9b34fb"
	// enBmsWriteUUID — характеристика записи команд (host→BMS).
	enBmsWriteUUID = "0000ff02-0000-1000-8000-00805f9b34fb"
	// enBmsNotifyUUID — характеристика уведомлений/ответов (BMS→host).
	enBmsNotifyUUID = "0000ff01-0000-1000-8000-00805f9b34fb"

	// enBmsCID1 — фиксированный CID1 в запросе.
	enBmsCID1 = 0x46
	// enBmsCID2Battery — команда чтения телеметрии (Battery).
	enBmsCID2Battery = 0x61
	// enBmsFrameOverhead — служебные байты кадра: SOI+VER+ADR+CID2+RTN+LEN(2)+
	// CHKSUM(2)+EOI. Полная длина кадра = overhead + LENID (проверено по живым
	// кадрам Battery/BasicInfo: LEN в байтах 5–6, INFO с байта 7).
	enBmsFrameOverhead = 10
)

// enbmsMaxBTDevices — предельное суммарное число BLE-устройств (EnBMS + CE308),
// которые пулеры держат постоянными соединениями. Реальный BLE-контроллер
// допускает ограниченное число одновременных соединений (~5); при превышении
// опрос EnBMS не запускается вовсе (см. main). Значение подлежит уточнению на
// конкретном адаптере.
const enbmsMaxBTDevices = 5

// enBmsConfigFromSection строит *enBmsConfig из раздела enBms. nil — если
// раздела нет, он отключён или не содержит активных устройств. Ошибка — при
// некорректных обязательных полях.
func enBmsConfigFromSection(s *enBmsSection) (*enBmsConfig, error) {
	if s == nil {
		return nil, nil
	}
	if s.Disabled != nil && *s.Disabled {
		logEnBms("disabled=true — опрос EnBMS отключён")
		return nil, nil
	}
	var devs []enBmsDeviceConfig
	seen := map[string]string{}
	for i := range s.Devices {
		d := &s.Devices[i]
		if d.Disabled == nil {
			return nil, fmt.Errorf("в разделе enBms у устройства %d не задано обязательное поле disabled (false/true)", i+1)
		}
		if *d.Disabled {
			logEnBms("устройство %q (mac %s) отключено (disabled=true)", d.Name, d.MAC)
			continue
		}
		if d.MAC == "" {
			return nil, fmt.Errorf("в разделе enBms у устройства %q не задан обязательный mac (нужен для BLE-подключения)", d.Name)
		}
		if prev, dup := seen[d.MAC]; dup {
			return nil, fmt.Errorf("в разделе enBms дублирующийся mac %s (%q и %q)", d.MAC, prev, d.Name)
		}
		seen[d.MAC] = d.Name
		name := d.Name
		if name == "" {
			// Метка по умолчанию — только по MAC: заводское BLE-имя (BP00) у всех
			// одинаково и как имя бесполезно.
			name = "BMS " + d.MAC
		}
		devs = append(devs, enBmsDeviceConfig{Name: name, MAC: d.MAC})
	}
	if len(devs) == 0 {
		return nil, nil
	}
	return &enBmsConfig{Devices: devs}, nil
}

// describeEnBmsConfig — строка-описание конфига для лога.
func describeEnBmsConfig(c *enBmsConfig) string {
	if c == nil {
		return ""
	}
	parts := make([]string, 0, len(c.Devices))
	for _, d := range c.Devices {
		parts = append(parts, fmt.Sprintf("%s (MAC %s)", d.Name, d.MAC))
	}
	return fmt.Sprintf("устройств=%d: %v", len(c.Devices), parts)
}

// crc16CCITT — CRC-16/CCITT (полином 0x1021, init 0, без отражения, MSB-first),
// как в приложении EN BMS (PROTOCOL.md §3.3). Считается по байтам кадра со 2-го
// по предпоследний (без SOI, CRC-поля и EOI).
func crc16CCITT(buf []byte) uint16 {
	var crc uint16
	for _, b := range buf {
		for bit := 0; bit < 8; bit++ {
			if crc&0x8000 != 0 {
				crc = (crc << 1) ^ 0x1021
			} else {
				crc <<= 1
			}
			if b&0x80 != 0 {
				crc ^= 0x1021
			}
			b <<= 1
		}
		crc &= 0xffff
	}
	return crc
}

// buildEnBmsFrame собирает кадр запроса:
// 7E 10 ADR 46 CID2 LEN(2) INFO CHKSUM(2) 0D. ADR=0 (одиночный пакет),
// CHKSUM — CRC-16/CCITT по телу кадра (без SOI/CHKSUM/EOI).
func buildEnBmsFrame(cid2 byte, info []byte) []byte {
	body := make([]byte, 0, 6+len(info))
	body = append(body, 0x10, 0x00, enBmsCID1, cid2,
		byte(len(info)>>8), byte(len(info)))
	body = append(body, info...)
	frame := make([]byte, 0, enBmsFrameOverhead+len(info))
	frame = append(frame, 0x7e)
	frame = append(frame, body...)
	crc := crc16CCITT(body)
	frame = append(frame, byte(crc>>8), byte(crc), 0x0d)
	return frame
}

// crc16Valid проверяет CRC-16/CCITT кадра: CHKSUM — два байта перед EOI.
func crc16Valid(frame []byte) bool {
	if len(frame) < 5 {
		return false
	}
	want := uint16(frame[len(frame)-3])<<8 | uint16(frame[len(frame)-2])
	return crc16CCITT(frame[1:len(frame)-3]) == want
}

// extractEnBmsFrame ищет в буфере первый полный валидный кадр ОТВЕТА.
// Формат ответа: 7E 14 ADR CID2 RTN LEN(2) INFO CHKSUM(2) 0D — LEN по смещению
// 5, INFO с 7, полная длина = 10 + LENID (проверено по живым кадрам Battery и
// BasicInfo). Сборка — строго ПО ДЛИНЕ, а не по 0x0D: байт 0x0D встречается
// внутри payload.
// Возвращает:
//   - frame, rest, true  — найден полный кадр; rest — остаток буфера;
//   - nil, rest, false   — кадра нет, но rest нужно сохранить (неполный кадр);
//   - nil, nil, false    — в буфере нет начала кадра (мусор), буфер можно сбросить.
func extractEnBmsFrame(buf []byte) (frame []byte, rest []byte, ok bool) {
	for len(buf) > 0 {
		s := bytes.IndexByte(buf, 0x7e)
		if s < 0 {
			return nil, nil, false
		}
		buf = buf[s:]
		if len(buf) < 7 {
			return nil, buf, false
		}
		lenid := int(buf[5])<<8 | int(buf[6])
		total := enBmsFrameOverhead + lenid
		if total < enBmsFrameOverhead || total > 2048 {
			buf = buf[1:]
			continue
		}
		if len(buf) < total {
			return nil, buf, false
		}
		fr := buf[:total]
		if fr[total-1] == 0x0d && crc16Valid(fr) {
			return fr, buf[total:], true
		}
		buf = buf[1:]
	}
	return nil, nil, false
}

// enbmsParsed — декодированный блок Battery (CID2 0x61).
type enbmsParsed struct {
	BatteryNum    int
	CellsV        []float64
	TemperaturesC []float64
	CurrentA      float64
	TotalVoltageV float64
	RemainingAh   float64
	TotalCapacity float64
	RatedCapacity float64
	Soc           float64
	Soh           float64
	Cycles        int
	PortVoltageV  float64
	CustomerP     int
	Tail          enbmsTail // сигнальный хвост Battery (защиты/Ext_Bit/баланс/режим)
}

// parseEnBmsBattery декодирует payload блока Battery (PROTOCOL.md §5.1).
// Многобайтовые поля — big-endian; customerp занимает ровно 1 байт. Требует
// минимум 67 байт (данные до portvoltage включительно); хвост предупреждений
// не парсится.
func parseEnBmsBattery(p []byte) (enbmsParsed, error) {
	var r enbmsParsed
	if len(p) < 4 {
		return r, fmt.Errorf("payload Battery слишком короткий: %d Б", len(p))
	}
	n := int(p[2])
	r.BatteryNum = n
	o := 3
	if o+n*2 > len(p) {
		return r, fmt.Errorf("payload Battery: не хватает %d напряжений ячеек (len=%d)", n, len(p))
	}
	r.CellsV = make([]float64, 0, n)
	for i := 0; i < n; i++ {
		r.CellsV = append(r.CellsV, enbmsRound(float64(u16be(p, o))/1000, 3))
		o += 2
	}
	if o >= len(p) {
		return r, fmt.Errorf("payload Battery: нет поля tempnum")
	}
	tn := int(p[o])
	o++
	if o+tn*2 > len(p) {
		return r, fmt.Errorf("payload Battery: не хватает %d температур (len=%d)", tn, len(p))
	}
	r.TemperaturesC = make([]float64, 0, tn)
	for i := 0; i < tn; i++ {
		r.TemperaturesC = append(r.TemperaturesC,
			enbmsRound(float64(u16be(p, o))*0.1-273.1, 1))
		o += 2
	}
	// Дальше — фиксированные поля: current(s16), totalV, leftcap, customerp(u8),
	// totalcap, soc, ratedcap, cycles, soh, portV. Нужно ещё 19 байт.
	if o+19 > len(p) {
		return r, fmt.Errorf("payload Battery: не хватает телеметрии (o=%d, len=%d)", o, len(p))
	}
	// chargecurrent (s16): в протоколе EnBMS «минус = разряд». Принятое в
	// проекте соглашение — разряд положительный, заряд отрицательный (как у ANT
	// BMS), поэтому знак инвертируем. Мощность ниже считается как U·I, поэтому
	// знак мощности согласуется автоматически.
	r.CurrentA = enbmsRound(-float64(int16(u16be(p, o)))*0.01, 2)
	o += 2
	r.TotalVoltageV = enbmsRound(float64(u16be(p, o))*0.01, 2)
	o += 2
	r.RemainingAh = enbmsRound(float64(u16be(p, o))*0.01, 2)
	o += 2
	r.CustomerP = int(p[o])
	o++
	r.TotalCapacity = enbmsRound(float64(u16be(p, o))*0.01, 2)
	o += 2
	r.Soc = enbmsRound(float64(u16be(p, o))*0.1, 1)
	o += 2
	r.RatedCapacity = enbmsRound(float64(u16be(p, o))*0.01, 2)
	o += 2
	r.Cycles = int(u16be(p, o))
	o += 2
	r.Soh = enbmsRound(float64(u16be(p, o))*0.1, 1)
	o += 2
	r.PortVoltageV = enbmsRound(float64(u16be(p, o))*0.01, 2)
	// Сигнальный хвост (защиты ячеек/датчиков, Ext_Bit, балансировка, режим).
	// Раскладка из приложения (16S_V20_ADDR_EN.xml, teleSignal_Group): хвост =
	// 16+6+2+14+1 = 39 байт. Смещение Ext_Bit подтверждается живым кадром лишь
	// косвенно (см. BACKLOG) — возможны ложные срабатывания, калибруем по сырому логу.
	r.Tail = parseEnBmsTail(p)
	return r, nil
}

// u16be читает u16 big-endian.
func u16be(p []byte, o int) uint16 {
	return uint16(p[o])<<8 | uint16(p[o+1])
}

// enbmsRound округляет v до digits знаков после запятой.
func enbmsRound(v float64, digits int) float64 {
	m := math.Pow10(digits)
	return math.Round(v*m) / m
}

// enbmsSnapshot — снимок EnBMS, хранимый в Redis (current/series) и PG.
// Поля повторяют структуру снимка ANT BMS (cells_v/temperatures_c/current_a/
// power_w/soc/…), чтобы данные были единообразны и оптимизированы для чтения
// за период времени. MAC — уникальный ключ устройства (заводское BLE-имя не
// уникально); Name — только отображаемое имя.
type enbmsSnapshot struct {
	Name          string    `json:"name"`
	MAC           string    `json:"mac"`
	Timestamp     string    `json:"timestamp"`
	CellCount     int       `json:"cell_count"`
	CellsV        []float64 `json:"cells_v"`
	TemperaturesC []float64 `json:"temperatures_c"`
	CurrentA      float64   `json:"current_a"`
	PowerW        float64   `json:"power_w"`
	Soc           float64   `json:"soc"`
	CapacityAh    float64   `json:"capacity_ah"`
	RemainingAh   float64   `json:"remaining_ah"`
	TotalVoltageV float64   `json:"total_voltage_v"`
	PortVoltageV  float64   `json:"port_voltage_v"`
	Soh           float64   `json:"soh"`
	Cycles        int       `json:"cycles"`
	MaxCellIdx    int       `json:"max_cell_idx"`
	MaxCellV      float64   `json:"max_cell_v"`
	MinCellIdx    int       `json:"min_cell_idx"`
	MinCellV      float64   `json:"min_cell_v"`
	AvgCellV      float64   `json:"avg_cell_v"`
	Model         string    `json:"model,omitempty"`  // модель/протокол (BasicInfo 0x51)
	Alarms        []string  `json:"alarms,omitempty"` // активные защиты/предупреждения
}

// enbmsSnapshotFromParsed строит снимок из декодированного Battery: мощности
// P = U·I (протокол мощность отдельно не отдаёт), индексы/напряжения max/min
// ячеек — из фактического массива ячеек (как recomputeMinMaxCells в ANT BMS).
func enbmsSnapshotFromParsed(cfg enBmsDeviceConfig, r enbmsParsed, now time.Time) enbmsSnapshot {
	s := enbmsSnapshot{
		Name:          cfg.Name,
		MAC:           cfg.MAC,
		Timestamp:     now.Format(time.RFC3339),
		CellCount:     len(r.CellsV),
		CellsV:        r.CellsV,
		TemperaturesC: r.TemperaturesC,
		CurrentA:      r.CurrentA,
		PowerW:        enbmsRound(r.TotalVoltageV*r.CurrentA, 1),
		Soc:           r.Soc,
		CapacityAh:    r.TotalCapacity,
		RemainingAh:   r.RemainingAh,
		TotalVoltageV: r.TotalVoltageV,
		PortVoltageV:  r.PortVoltageV,
		Soh:           r.Soh,
		Cycles:        r.Cycles,
	}
	if len(r.CellsV) > 0 {
		maxIdx, minIdx := 0, 0
		sum := 0.0
		for i, v := range r.CellsV {
			sum += v
			if v > r.CellsV[maxIdx] {
				maxIdx = i
			}
			if v < r.CellsV[minIdx] {
				minIdx = i
			}
		}
		s.MaxCellIdx = maxIdx + 1
		s.MinCellIdx = minIdx + 1
		s.MaxCellV = r.CellsV[maxIdx]
		s.MinCellV = r.CellsV[minIdx]
		s.AvgCellV = enbmsRound(sum/float64(len(r.CellsV)), 3)
	}
	// Warn-область Battery (раскладка из APK, см. parseEnBmsTail и PROTOCOL §5.1.1);
	// на здоровом кадре пуста. Балансировка/режим пока не декодируются.
	s.Alarms = append(s.Alarms, r.Tail.Alarms...)
	return s
}

// enbmsTail — декодированный хвост Battery. Раскладка восстановлена из APK
// (`parseBody_Battery`, BmsMsgUtil.dart 0x3f3a18; порядок имён — `toJson`
// 0x3def6c): после телеметрии идут warn-списки, затем статусы:
//   [0..15]  список по ячейкам (batterynum=16 байт)
//   [16..19] список по температурам (tempnum-2=4 байта)
//   [20] envtempwarn, [21] powertempwarn, [22] chargecurrentwarn
//   [23] customerwarnp, далее статусы (ключи/баланс/режим) — в норме ненулевые.
// На ЗДОРОВОМ кадре warn-область (0..23) нулевая; ненулевые байты 25..27 —
// статусы, не аварии (см. PROTOCOL.md energybms §5.1.1).
type enbmsTail struct {
	BatWarn    []int // предупреждения по ячейкам (nonzero = активное)
	TempWarn   []int // предупреждения по температурам
	EnvWarn    int
	PowerWarn  int
	ChargeWarn int
	Alarms     []string
}

// parseEnBmsTail декодирует warn-область хвоста Battery (последние 39 байт).
// Безопасно при коротком payload. Статусы (ключи/баланс/режим) не декодируются —
// их смещения не подтверждены.
func parseEnBmsTail(p []byte) enbmsTail {
	var t enbmsTail
	if len(p) < 39 {
		return t
	}
	o := len(p) - 39
	t.BatWarn = make([]int, 16)
	for i := 0; i < 16; i++ {
		t.BatWarn[i] = int(p[o+i])
	}
	t.TempWarn = make([]int, 4)
	for i := 0; i < 4; i++ {
		t.TempWarn[i] = int(p[o+16+i])
	}
	t.EnvWarn = int(p[o+20])
	t.PowerWarn = int(p[o+21])
	t.ChargeWarn = int(p[o+22])
	for i, v := range t.BatWarn {
		if v != 0 {
			t.Alarms = append(t.Alarms, fmt.Sprintf("Ячейка: предупреждение (%d, 0x%02x)", i+1, v))
		}
	}
	for i, v := range t.TempWarn {
		if v != 0 {
			t.Alarms = append(t.Alarms, fmt.Sprintf("Датчик: предупреждение (%d, 0x%02x)", i+1, v))
		}
	}
	if t.EnvWarn != 0 {
		t.Alarms = append(t.Alarms, fmt.Sprintf("Температура среды: предупреждение (0x%02x)", t.EnvWarn))
	}
	if t.PowerWarn != 0 {
		t.Alarms = append(t.Alarms, fmt.Sprintf("Силовая часть: предупреждение (0x%02x)", t.PowerWarn))
	}
	if t.ChargeWarn != 0 {
		t.Alarms = append(t.Alarms, fmt.Sprintf("Ток заряда: предупреждение (0x%02x)", t.ChargeWarn))
	}
	return t
}

// parseEnBmsModel извлекает читаемую модель/протокол из payload BasicInfo (0x51):
// первые ~30 байт — ASCII-строка, дополненная пробелами (далее — служебные байты).
func parseEnBmsModel(p []byte) string {
	if len(p) == 0 {
		return ""
	}
	n := len(p)
	if n > 30 {
		n = 30
	}
	b := make([]byte, 0, n)
	for _, c := range p[:n] {
		if c >= 0x20 && c < 0x7f {
			b = append(b, c)
		} else if c == 0 {
			break
		}
	}
	return strings.TrimSpace(string(b))
}

// Грубые границы валидности показаний EnBMS: отбрасываем очевидный мусор
// (нестабильный BLE-канал), не реальные значения.
const (
	enbmsCellVMin   = 0.5
	enbmsCellVMax   = 5.0
	enbmsTempMin    = -50.0
	enbmsTempMax    = 150.0
	enbmsCurrentMin = -2000.0
	enbmsCurrentMax = 2000.0
	enbmsTotalVMax  = 1000.0
	enbmsSocMax     = 200.0
	enbmsCapMaxAh   = 10000.0
)

// enbmsParsedValid проверяет целостность декодированных показаний.
func enbmsParsedValid(r enbmsParsed) bool {
	if r.BatteryNum <= 0 || r.BatteryNum > 32 || len(r.CellsV) != r.BatteryNum {
		return false
	}
	for _, v := range r.CellsV {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < enbmsCellVMin || v > enbmsCellVMax {
			return false
		}
	}
	for _, v := range r.TemperaturesC {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < enbmsTempMin || v > enbmsTempMax {
			return false
		}
	}
	if math.IsNaN(r.CurrentA) || math.IsInf(r.CurrentA, 0) ||
		r.CurrentA < enbmsCurrentMin || r.CurrentA > enbmsCurrentMax {
		return false
	}
	if math.IsNaN(r.TotalVoltageV) || math.IsInf(r.TotalVoltageV, 0) ||
		r.TotalVoltageV < 0 || r.TotalVoltageV > enbmsTotalVMax {
		return false
	}
	if math.IsNaN(r.PortVoltageV) || math.IsInf(r.PortVoltageV, 0) ||
		r.PortVoltageV < 0 || r.PortVoltageV > enbmsTotalVMax {
		return false
	}
	if r.Soc < 0 || r.Soc > enbmsSocMax || r.Soh < 0 || r.Soh > enbmsSocMax {
		return false
	}
	if r.TotalCapacity < 0 || r.TotalCapacity > enbmsCapMaxAh ||
		r.RemainingAh < 0 || r.RemainingAh > enbmsCapMaxAh {
		return false
	}
	return true
}

// bmsDeviceFromEnBms приводит снимок EnBMS к форме bmsDevice (ANT BMS) для
// общего API/дашборда: фронт читает те же поля. Чего в блоке Battery нет
// (MOS/балансировка, счётчик кадров) — остаётся нулевым, а kind="enbms" велит
// фронту эти блоки не показывать.
func bmsDeviceFromEnBms(s enbmsSnapshot) bmsDevice {
	// Отображаемое имя — имя + MAC: у EnBMS заводское BLE-имя не уникально,
	// MAC однозначно идентифицирует устройство (видно и на главной, и на /bms/<mac>).
	name := s.Name
	if s.MAC != "" && !strings.Contains(name, s.MAC) {
		name = strings.TrimSpace(name + " " + s.MAC)
	}
	d := bmsDevice{
		Kind:          "enbms",
		DeviceName:    name,
		Key:           s.MAC,
		Port:          "BLE",
		CellCount:     s.CellCount,
		CellsV:        s.CellsV,
		CurrentA:      s.CurrentA,
		Soc:           int(math.Round(s.Soc)),
		CapacityAh:    s.CapacityAh,
		RemainingAh:   s.RemainingAh,
		TemperaturesC: s.TemperaturesC,
		PowerW:        s.PowerW,
		MaxCellIdx:    s.MaxCellIdx,
		MaxCellV:      s.MaxCellV,
		MinCellIdx:    s.MinCellIdx,
		MinCellV:      s.MinCellV,
		AvgCellV:      s.AvgCellV,
		Soh:           s.Soh,
		Cycles:        s.Cycles,
		Model:         s.Model,
		Alarms:        s.Alarms,
	}
	if ts, err := time.Parse(time.RFC3339, s.Timestamp); err == nil {
		d.Timestamp = ts.Unix()
		d.Time = ts.Format("15:04:05")
	}
	return d
}

// bmsSeriesPointFromEnBms приводит точку ряда EnBMS к форме ANT BMS
// (bmsSeriesPoint): совпадающие поля переносятся 1:1, отсутствующие у EnBMS —
// нулевые. Используется общим API /api/bms/<mac>/series и графиками страницы BMS.
func bmsSeriesPointFromEnBms(p enbmsSeriesPoint) bmsSeriesPoint {
	return bmsSeriesPoint{
		Name:    p.Name,
		Display: p.Display,
		Ts:      p.Ts,
		bmsAveraged: bmsAveraged{
			CurrentA:     p.CurrentA,
			PowerW:       p.PowerW,
			Soc:          p.Soc,
			CapacityAh:   p.CapacityAh,
			RemainingAh:  p.RemainingAh,
			MaxCellV:     p.MaxCellV,
			MinCellV:     p.MinCellV,
			AvgCellV:     p.AvgCellV,
			CellsV:       p.CellsV,
			Temperatures: p.Temperatures,
			CellCount:    p.CellCount,
			Samples:      p.Samples,
		},
	}
}
