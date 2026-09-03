package solarman

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

const (
	StartMarker byte = 0xA5
	EndMarker   byte = 0x15

	ReqControlCode uint16 = 0x4510
	ResControlCode uint16 = 0x1510

	framePrefixLen = 11 // A5 + len(2) + control(2) + serial(2) + devSN(4)
	frameTailLen   = 2  // checksum + end marker
)

type Frame struct {
	ControlCode uint16
	Serial      uint16
	DeviceSN    uint32
	Payload     []byte
	ChecksumOK  bool
	Valid       bool
}

func Checksum8(b []byte) byte {
	sum := 0
	for _, x := range b {
		sum += int(x)
	}
	return byte(sum & 0xFF)
}

func CRC16Modbus(data []byte) uint16 {
	crc := uint16(0xFFFF)
	for _, b := range data {
		crc ^= uint16(b)
		for i := 0; i < 8; i++ {
			if crc&1 != 0 {
				crc = (crc >> 1) ^ 0xA001
			} else {
				crc >>= 1
			}
		}
	}
	return crc
}

// BuildReadFrame — кадр запроса чтения регистров (Modbus func 03).
// deviceSN — серийник логгера (0 принимается Sofar LSW-3).
func BuildReadFrame(deviceSN uint32, startReg, regCount uint16) []byte {
	pdu := make([]byte, 6)
	pdu[0] = 0x01
	pdu[1] = 0x03
	binary.BigEndian.PutUint16(pdu[2:4], startReg)
	binary.BigEndian.PutUint16(pdu[4:6], regCount)
	crc := CRC16Modbus(pdu)

	payload := make([]byte, 0, 20)
	payload = append(payload, 0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00)
	payload = append(payload, pdu...)
	payload = append(payload, byte(crc), byte(crc>>8))

	frame := make([]byte, 0, framePrefixLen+len(payload)+frameTailLen)
	frame = append(frame, StartMarker)
	frame = append(frame, byte(len(payload)), byte(len(payload)>>8))
	frame = append(frame, byte(ReqControlCode&0xFF), byte(ReqControlCode>>8))
	frame = append(frame, 0x00, 0x00)
	var sn [4]byte
	binary.LittleEndian.PutUint32(sn[:], deviceSN)
	frame = append(frame, sn[:]...)
	frame = append(frame, payload...)
	frame = append(frame, Checksum8(frame[1:]))
	frame = append(frame, EndMarker)
	return frame
}

// BuildDeyeReadFrame — кадр чтения регистров для Deye-даталоггеров (Solarman V5,
// но 14-байтный datafield-заголовок, как в kbialek/deye-inverter-mqtt).
// deviceSN — реальный SN логгера; unit — Modbus-адрес устройства (обычно 0x01).
func BuildDeyeReadFrame(deviceSN, unit uint32, startReg, regCount uint16) []byte {
	pdu := make([]byte, 8)
	pdu[0] = byte(unit)
	pdu[1] = 0x03
	binary.BigEndian.PutUint16(pdu[2:4], startReg)
	binary.BigEndian.PutUint16(pdu[4:6], regCount)
	crc := CRC16Modbus(pdu[:6])
	pdu[6] = byte(crc)
	pdu[7] = byte(crc >> 8)

	// datafield 15 байт: 02 + 14 нулей (в kbialek/deye-inverter-mqtt)
	datafield := make([]byte, 15)
	datafield[0] = 0x02
	payload := append(datafield, pdu...)

	frame := make([]byte, 0, framePrefixLen+len(payload)+frameTailLen)
	frame = append(frame, StartMarker)
	frame = append(frame, byte(len(payload)), byte(len(payload)>>8))
	frame = append(frame, byte(ReqControlCode&0xFF), byte(ReqControlCode>>8))
	frame = append(frame, 0x00, 0x00)
	var sn [4]byte
	binary.LittleEndian.PutUint32(sn[:], deviceSN)
	frame = append(frame, sn[:]...)
	frame = append(frame, payload...)
	frame = append(frame, Checksum8(frame[1:]))
	frame = append(frame, EndMarker)
	return frame
}

// DeyeErrorCode возвращает код ошибки из 29-байтного heartbeat-ответа логгера
// (payload 16: frameType, status, uptime u32, cnt u32, sn u32, 06 00).
// 0x00 — нет ошибки (обычный heartbeat), 0x05 — адрес устройства, 0x06 — SN не совпадает.
func DeyeErrorCode(frame Frame) (byte, bool) {
	if frame.ControlCode != ResControlCode || len(frame.Payload) != 16 || frame.Payload[0] != 0x02 {
		return 0, false
	}
	// payload 16: 02 01 <uptime u32> <cnt u32> <sn u32> <код_ошибки> 00 — код на offset 14
	// (wire offset 25 = 11 заголовка + 14 в payload).
	return frame.Payload[14], true
}

// SplitFrames дробит сырой TCP-ответ на кадры.
// Длина кадра = 11 + PayloadLen + 2.
func SplitFrames(raw []byte) []Frame {
	var frames []Frame
	i := 0
	for i < len(raw) {
		if raw[i] != StartMarker {
			i++
			continue
		}
		if i+framePrefixLen+frameTailLen > len(raw) {
			break
		}
		plen := int(binary.LittleEndian.Uint16(raw[i+1 : i+3]))
		total := framePrefixLen + plen + frameTailLen
		if i+total > len(raw) {
			next := i + 1
			for next < len(raw) && raw[next] != StartMarker {
				next++
			}
			if next >= len(raw) {
				break
			}
			i = next
			continue
		}
		f := Frame{
			ControlCode: binary.LittleEndian.Uint16(raw[i+3 : i+5]),
			Serial:      binary.BigEndian.Uint16(raw[i+5 : i+7]),
			DeviceSN:    binary.LittleEndian.Uint32(raw[i+7 : i+11]),
			Payload:     append([]byte{}, raw[i+framePrefixLen:i+framePrefixLen+plen]...),
		}
		f.ChecksumOK = raw[i+framePrefixLen+plen] == Checksum8(raw[i+1:i+framePrefixLen+plen])
		f.Valid = f.ChecksumOK && raw[i+total-1] == EndMarker && f.ControlCode == ResControlCode
		frames = append(frames, f)
		i += total
	}
	return frames
}

// ModbusPDU — вложенный Modbus-ответ внутри payload кадра.
type ModbusPDU struct {
	Offset     int // позиция PDU в payload
	Function   uint8
	ByteCount  uint8
	Values     []uint16 // регистры, big-endian по 2 байта
	CRC        uint16
	CRCCalc    uint16
}

// ParseModbusPDU находит Modbus-ответ (01 03 | bytecount | data | crc16 LE)
// в payload начиная с offsets. Возвращает все найденные PDU.
func ParseModbusPDU(payload []byte, offsets ...int) []ModbusPDU {
	var starts []int
	if len(offsets) > 0 {
		starts = offsets
	} else {
		starts = []int{14}
	}
	var pdus []ModbusPDU
	for _, s := range starts {
		if s+5 > len(payload) {
			continue
		}
		if payload[s] != 0x01 || (payload[s+1] != 0x03 && payload[s+1] != 0x04) {
			continue
		}
		vlen := int(payload[s+2])
		if s+3+vlen+2 > len(payload) {
			continue
		}
		p := ModbusPDU{Offset: s, Function: payload[s+1], ByteCount: uint8(vlen)}
		n := vlen / 2
		p.Values = make([]uint16, n)
		for k := 0; k < n; k++ {
			p.Values[k] = binary.BigEndian.Uint16(payload[s+3+k*2 : s+5+k*2])
		}
		p.CRC = binary.LittleEndian.Uint16(payload[s+3+vlen : s+5+vlen])
		p.CRCCalc = CRC16Modbus(payload[s : s+3+vlen])
		pdus = append(pdus, p)
	}
	return pdus
}

// ParsePayload разбивает payload ответа на заголовок и бизнес-часть.
type ResponseHeader struct {
	FrameType    uint8
	Status       uint8
	DeliveryTime uint32
	PowerOnTime  uint32
	OffsetTime   uint32
}

func ParseResponseHeader(payload []byte) (ResponseHeader, bool) {
	if len(payload) < 14 {
		return ResponseHeader{}, false
	}
	h := ResponseHeader{
		FrameType:    payload[0],
		Status:       payload[1],
		DeliveryTime: binary.LittleEndian.Uint32(payload[2:6]),
		PowerOnTime:  binary.LittleEndian.Uint32(payload[6:10]),
		OffsetTime:   binary.LittleEndian.Uint32(payload[10:14]),
	}
	return h, true
}

func FrameHexDump(frame Frame) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "control=0x%04X serial=0x%04X sn=%08x len=%d cksum_ok=%v",
		frame.ControlCode, frame.Serial, frame.DeviceSN, len(frame.Payload), frame.ChecksumOK)
	return b.String()
}
