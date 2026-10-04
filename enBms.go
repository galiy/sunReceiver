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

// enBmsSection — раздел "enBms" sunReceiver.json: коллекция устройств BMS типа
// EnBMS. Общий disabled — ОБЯЗАТЕЛЬНОЕ поле: false — устройства опрашиваются;
// true — опрос EnBMS отключён целиком.
//
// Период опроса задаётся для КАЖДОГО метода отдельно (poll_interval_ble /
// poll_interval_rs485, секунды). Метод выбирается у КАЖДОГО устройства
// (devices[].method), поэтому в одной коллекции устройства могут одновременно
// опрашиваться разными методами (часть по BLE, часть по RS485) — каждый своим
// периодом.
type enBmsSection struct {
	Disabled          *bool                `json:"disabled"`
	PollIntervalBLE   int                  `json:"poll_interval_ble"`
	PollIntervalRS485 int                  `json:"poll_interval_rs485"`
	Devices           []enBmsDeviceSection `json:"devices"`
}

// enBmsDeviceSection — одно устройство коллекции: обязательные disabled и method
// ("ble"|"rs485"), ключ (mac, иначе name) и настройки метода. Для method=rs485
// обязателен вложенный блок rs485; для method=ble он не нужен и может
// отсутствовать (если присутствует — хранится как альтернативные настройки для
// возможного переключения на RS485).
type enBmsDeviceSection struct {
	Name     string             `json:"name"`   // отображаемое имя (необязательно)
	MAC      string             `json:"mac"`    // BD_ADDR (обязателен для BLE; ключ хранилища)
	Method   string             `json:"method"` // "ble" | "rs485" (обязательно)
	RS485    *enBmsRS485Section `json:"rs485"`  // настройки RS485 (для method=rs485)
	Disabled *bool              `json:"disabled"`
}

// enBmsRS485Section — настройки RS485-транспорта конкретного устройства.
// Транспорт — либо TCP (прозрачный IP-RS485-шлюз), либо локальный COM-порт.
// Протокол — ASCII PACE (см. enBms_rs485.go).
//
//	Transport  — "tcp" (по умолчанию) | "com"
//	Address    — host:port прозрачного TCP-шлюза (для transport=tcp)
//	Port       — имя COM-порта (для transport=com, напр. "COM3" или "/dev/ttyUSB0")
//	Baud       — скорость COM (для transport=com, по умолчанию 19200)
//	DataBits/Parity/StopBits — параметры COM (по умолчанию 8/none/1)
//	PortType   — "rs485" (верхний host-RS485 BMS: read-команды без INFO; по
//	             умолчанию) | "rm485" (инверторный RM485 BMS: для 0x42 нужен
//	             INFO=[00]). Это выбор ФИЗИЧЕСКОГО порта BMS, к которому
//	             подключён шлюз.
//	Unit       — адрес устройства в кадре PACE (поле ADR). Для этой BMS живой
//	             опрос идёт с ADR=0, поэтому unit=0. Значение подставляется в кадр
//	             как есть (без +1/−1).
type enBmsRS485Section struct {
	Transport string `json:"transport"`
	Address   string `json:"address"`
	Port      string `json:"port"`
	Baud      int    `json:"baud"`
	DataBits  int    `json:"data_bits"`
	Parity    string `json:"parity"`
	StopBits  int    `json:"stop_bits"`
	PortType  string `json:"port_type"`
	Unit      int    `json:"unit"`
}

// enBmsMethod — способ опроса EnBMS.
type enBmsMethod int

const (
	enBmsMethodBLE enBmsMethod = iota
	enBmsMethodRS485
)

// String — короткое имя метода для логов.
func (m enBmsMethod) String() string {
	if m == enBmsMethodRS485 {
		return "rs485"
	}
	return "ble"
}

// enBmsConfig — проверенный конфиг EnBMS (только активные устройства). Периоды
// цикла — по методам; устройства несут собственный метод и (для RS485) настройки.
type enBmsConfig struct {
	PollBLE   time.Duration // период цикла BLE-устройств
	PollRS485 time.Duration // период цикла RS485-устройств
	Devices   []enBmsDeviceConfig
}

// enBmsDeviceConfig — одно активное устройство EnBMS.
type enBmsDeviceConfig struct {
	Name   string            // отображаемое имя
	Key    string            // стабильный ключ устройства в Redis/PG (= MAC, иначе name)
	MAC    string            // BD_ADDR (для BLE-подключения; для RS485 может быть пустым)
	Method enBmsMethod       // метод опроса этого устройства
	RS485  *enBmsRS485Config // не nil при Method=rs485
}

// enBmsRS485Config — проверенные настройки RS485-транспорта.
type enBmsRS485Config struct {
	Transport string // "tcp" | "com"
	Address   string // tcp: host:port
	Port      string // com: имя порта
	Baud      int
	DataBits  int
	Parity    string
	StopBits  int
	PortType  string // "rs485" | "rm485"
	Unit      int    // PACE ADR (в кадр как есть)
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
// некорректных обязательных полях. Периоды задаются для каждого метода,
// метод выбирается у каждого устройства (устройства могут опрашиваться разными
// методами одновременно).
func enBmsConfigFromSection(s *enBmsSection) (*enBmsConfig, error) {
	if s == nil {
		return nil, nil
	}
	if s.Disabled != nil && *s.Disabled {
		logEnBms("disabled=true — опрос EnBMS отключён")
		return nil, nil
	}
	if s.PollIntervalBLE <= 0 {
		return nil, fmt.Errorf("enBms.poll_interval_ble должен быть > 0 (секунды)")
	}
	if s.PollIntervalRS485 <= 0 {
		return nil, fmt.Errorf("enBms.poll_interval_rs485 должен быть > 0 (секунды)")
	}

	devs := make([]enBmsDeviceConfig, 0, len(s.Devices))
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
		var method enBmsMethod
		switch strings.ToLower(strings.TrimSpace(d.Method)) {
		case "ble":
			method = enBmsMethodBLE
		case "rs485":
			method = enBmsMethodRS485
		default:
			return nil, fmt.Errorf("в разделе enBms у устройства %q не задано/некорректно обязательное поле method (ожидается ble|rs485, получено %q)", d.Name, d.Method)
		}
		// Ключ хранилища стабилен между методами: MAC (если задан), иначе name.
		key := strings.TrimSpace(d.MAC)
		if key == "" {
			key = strings.TrimSpace(d.Name)
		}
		if key == "" {
			return nil, fmt.Errorf("в разделе enBms у устройства %d не задан ни mac, ни name (нужен ключ устройства)", i+1)
		}
		if method == enBmsMethodBLE && d.MAC == "" {
			return nil, fmt.Errorf("в разделе enBms у устройства %q (method=ble) не задан обязательный mac (нужен для BLE-подключения)", d.Name)
		}
		if prev, dup := seen[key]; dup {
			return nil, fmt.Errorf("в разделе enBms дублирующийся ключ устройства %s (%q и %q)", key, prev, d.Name)
		}
		seen[key] = d.Name
		name := d.Name
		if name == "" {
			// Метка по умолчанию — по ключу: заводское BLE-имя (BP00) у всех
			// одинаково и как имя бесполезно.
			name = "BMS " + key
		}
		dev := enBmsDeviceConfig{Name: name, Key: key, MAC: d.MAC, Method: method}
		if d.RS485 != nil {
			// Настройки RS485 валидируются и для method=ble (хранятся как
			// альтернативные — при переключении метода в конфиге).
			c, err := enBmsRS485ConfigFromSection(d.RS485)
			if err != nil {
				return nil, fmt.Errorf("устройство %q: %w", name, err)
			}
			if method == enBmsMethodRS485 {
				dev.RS485 = c
			}
		} else if method == enBmsMethodRS485 {
			return nil, fmt.Errorf("в разделе enBms у устройства %q (method=rs485) не задан блок rs485", d.Name)
		}
		devs = append(devs, dev)
	}
	if len(devs) == 0 {
		return nil, nil
	}
	return &enBmsConfig{
		PollBLE:   time.Duration(s.PollIntervalBLE) * time.Second,
		PollRS485: time.Duration(s.PollIntervalRS485) * time.Second,
		Devices:   devs,
	}, nil
}

// enBmsRS485ConfigFromSection проверяет и применяет настройки RS485-транспорта
// одного устройства.
func enBmsRS485ConfigFromSection(s *enBmsRS485Section) (*enBmsRS485Config, error) {
	transport := strings.ToLower(strings.TrimSpace(s.Transport))
	if transport == "" {
		transport = "tcp"
	}
	if s.Unit < 0 || s.Unit > 255 {
		return nil, fmt.Errorf("enBms.rs485.unit=%d вне диапазона 0..255 (PACE ADR)", s.Unit)
	}
	c := &enBmsRS485Config{Transport: transport, Unit: s.Unit}
	switch transport {
	case "tcp":
		if strings.TrimSpace(s.Address) == "" {
			return nil, fmt.Errorf("enBms.rs485.transport=tcp: не задан address (host:port прозрачного TCP-шлюза)")
		}
		c.Address = strings.TrimSpace(s.Address)
	case "com":
		if strings.TrimSpace(s.Port) == "" {
			return nil, fmt.Errorf("enBms.rs485.transport=com: не задан port (имя COM-порта)")
		}
		c.Port = strings.TrimSpace(s.Port)
		c.Baud = s.Baud
		if c.Baud <= 0 {
			c.Baud = 19200
		}
		c.DataBits = s.DataBits
		if c.DataBits <= 0 {
			c.DataBits = 8
		}
		c.Parity = strings.ToLower(strings.TrimSpace(s.Parity))
		if c.Parity == "" {
			c.Parity = "none"
		}
		c.StopBits = s.StopBits
		if c.StopBits <= 0 {
			c.StopBits = 1
		}
	default:
		return nil, fmt.Errorf("enBms.rs485.transport: неизвестное значение %q (ожидается tcp|com)", s.Transport)
	}
	portType := strings.ToLower(strings.TrimSpace(s.PortType))
	if portType == "" {
		portType = "rs485"
	}
	if portType != "rs485" && portType != "rm485" {
		return nil, fmt.Errorf("enBms.rs485.port_type: неизвестное значение %q (ожидается rs485|rm485)", s.PortType)
	}
	c.PortType = portType
	return c, nil
}

// describeEnBmsConfig — строка-описание конфига для лога.
func describeEnBmsConfig(c *enBmsConfig) string {
	if c == nil {
		return ""
	}
	parts := make([]string, 0, len(c.Devices))
	for _, d := range c.Devices {
		parts = append(parts, fmt.Sprintf("%s (ключ %s, %s)", d.Name, d.Key, d.Method))
	}
	return fmt.Sprintf("периоды: ble=%s rs485=%s; устройств=%d: %v",
		c.PollBLE, c.PollRS485, len(c.Devices), parts)
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

// parseEnBmsBattery декодирует payload блока Battery BLE (CID2 0x61) с разбором
// сигнального хвоста (см. parseEnBmsBatteryPayload).
func parseEnBmsBattery(p []byte) (enbmsParsed, error) {
	return parseEnBmsBatteryPayload(p, true)
}

// parseEnBmsBatteryPayload декодирует payload телеметрии Telemeter/Battery.
// Раскладка телеметрии у BLE-блока Battery (0x61) и RS485-блока TeleMeter (0x42)
// до поля portvoltage/busvoltage совпадает, поэтому парсер общий. withTail
// управляет разбором сигнального хвоста предупреждений: он есть ТОЛЬКО у BLE
// Battery (106 Б, хвост 39 Б); у RS485 TeleMeter (75 Б) после напряжения клемм
// идут другие поля (temp-drift/энергии), поэтому хвост не разбирается.
// Многобайтовые поля — big-endian; customerp/field count занимает ровно 1 байт.
// Требует минимум 67 байт (данные до portvoltage включительно).
func parseEnBmsBatteryPayload(p []byte, withTail bool) (enbmsParsed, error) {
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
	if withTail {
		// Сигнальный хвост (защиты ячеек/датчиков, Ext_Bit, балансировка, режим).
		// Раскладка из приложения (16S_V20_ADDR_EN.xml, teleSignal_Group): хвост =
		// 16+6+2+14+1 = 39 байт. Смещение Ext_Bit подтверждается живым кадром лишь
		// косвенно (см. BACKLOG) — возможны ложные срабатывания, калибруем по сырому логу.
		r.Tail = parseEnBmsTail(p)
	}
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
	// Ключ хранилища — cfg.Key (MAC, иначе name); fallback на MAC для прямых
	// литералов enBmsDeviceConfig (тесты/иные вызовы без разбора конфига).
	key := cfg.Key
	if key == "" {
		key = cfg.MAC
	}
	s := enbmsSnapshot{
		Name:          cfg.Name,
		MAC:           key,
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
//
//	[0..15]  список по ячейкам (batterynum=16 байт)
//	[16..19] список по температурам (tempnum-2=4 байта)
//	[20] envtempwarn, [21] powertempwarn, [22] chargecurrentwarn
//	[23] customerwarnp, далее статусы (ключи/баланс/режим) — в норме ненулевые.
//
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

// enbmsCellWarnBits раскладывает байт предупреждений ячейки в тексты битов.
// Имена и порядок — из APK (ble_data.dart, определения warn-битов ячейки):
// 0=Cell high voltage warning, 1=Cell over voltage protection,
// 2=Cell low voltage warning, 3=Cell under voltage protection. Прочие биты — номер.
func enbmsCellWarnBits(v int) []string {
	if v == 0 {
		return nil
	}
	names := map[int]string{
		0: "высокое напряжение (предупреждение)",
		1: "защита от перенапряжения",
		2: "низкое напряжение (предупреждение)",
		3: "защита от пониженного напряжения",
	}
	var out []string
	for bit := 0; bit < 8; bit++ {
		if v&(1<<uint(bit)) == 0 {
			continue
		}
		if n, ok := names[bit]; ok {
			out = append(out, n)
		} else {
			out = append(out, fmt.Sprintf("бит %d", bit))
		}
	}
	return out
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
		for _, s := range enbmsCellWarnBits(v) {
			t.Alarms = append(t.Alarms, fmt.Sprintf("Ячейка %d: %s", i+1, s))
		}
	}
	for i, v := range t.TempWarn {
		if v != 0 {
			t.Alarms = append(t.Alarms, fmt.Sprintf("Датчик %d: предупреждение (0x%02x)", i+1, v))
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
