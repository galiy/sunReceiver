package solarman

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// TestCRC16ModbusVector — стандартный вектор CRC-16/MODBUS (init 0xFFFF,
// poly 0xA001 отражённый, без invert): "123456789" -> 0x4B37.
func TestCRC16ModbusVector(t *testing.T) {
	if got := CRC16Modbus([]byte("123456789")); got != 0x4B37 {
		t.Fatalf("CRC16Modbus(\"123456789\")=0x%04X, want 0x4B37", got)
	}
}

func TestChecksum8(t *testing.T) {
	// 1+2+253 = 256 -> 0
	if got := Checksum8([]byte{0x01, 0x02, 0xFD}); got != 0 {
		t.Fatalf("Checksum8=[%d], want 0", got)
	}
	if got := Checksum8([]byte{0x01, 0x02, 0xFC}); got != 0xFF {
		t.Fatalf("Checksum8=[%d], want 0xFF", got)
	}
}

// buildRespFrame собирает ответный кадр (control 0x1510, serial BE) для
// проверки SplitFrames.
func buildRespFrame(serial uint16, sn uint32, payload []byte) []byte {
	frame := []byte{StartMarker, byte(len(payload)), byte(len(payload) >> 8)}
	frame = append(frame, byte(ResControlCode&0xFF), byte(ResControlCode>>8))
	frame = append(frame, byte(serial>>8), byte(serial))
	var snb [4]byte
	binary.LittleEndian.PutUint32(snb[:], sn)
	frame = append(frame, snb[:]...)
	frame = append(frame, payload...)
	frame = append(frame, Checksum8(frame[1:]))
	frame = append(frame, EndMarker)
	return frame
}

func TestSplitFrames(t *testing.T) {
	p1 := []byte{0x02, 0x01, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x06, 0x00} // heartbeat 16
	f1 := buildRespFrame(0x0102, 0xAFA7A753, p1)
	p2 := []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x01}
	f2 := buildRespFrame(0x0304, 7, p2)
	// Мусор до и между кадрами (без байтов 0xA5) — должен перескакиваться.
	raw := append([]byte{0x00, 0xFF, 0xAB}, f1...)
	raw = append(raw, 0x00)
	raw = append(raw, f2...)

	frames := SplitFrames(raw)
	if len(frames) != 2 {
		t.Fatalf("SplitFrames: %d кадров, want 2", len(frames))
	}
	f0, f1v := frames[0], frames[1]
	if !f0.ChecksumOK || !f0.Valid {
		t.Fatalf("кадр 1: checksum_ok=%v valid=%v", f0.ChecksumOK, f0.Valid)
	}
	if f0.ControlCode != ResControlCode || f0.Serial != 0x0102 || f0.DeviceSN != 0xAFA7A753 {
		t.Fatalf("кадр 1: control=0x%04X serial=0x%04X sn=%08x", f0.ControlCode, f0.Serial, f0.DeviceSN)
	}
	if !bytes.Equal(f0.Payload, p1) {
		t.Fatalf("кадр 1 payload: % x, want % x", f0.Payload, p1)
	}
	if !f1v.ChecksumOK || !f1v.Valid || f1v.Serial != 0x0304 || f1v.DeviceSN != 7 {
		t.Fatalf("кадр 2: %+v", f1v)
	}
}

// TestSplitFramesChecksumCorrupt — кадр с битым checksum помечается
// ChecksumOK=false/Valid=false, но дробление продолжается.
func TestSplitFramesChecksumCorrupt(t *testing.T) {
	p := []byte{0x02, 0x01, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x00, 0x00}
	f := buildRespFrame(1, 2, p)
	f[len(f)-3]++ // портим checksum-байт
	frames := SplitFrames(f)
	if len(frames) != 1 {
		t.Fatalf("кадров: %d, want 1", len(frames))
	}
	if frames[0].ChecksumOK || frames[0].Valid {
		t.Fatalf("битый checksum не замечен: %+v", frames[0])
	}
}

func TestBuildReadFrame(t *testing.T) {
	// Sofar: 12-байтный datafield (02 + 11 нулей) + 6-байтная PDU + CRC16 LE.
	// PayloadLen = 12+6+2 = 20 -> "14 00" LE (как задокументировано).
	f := BuildReadFrame(0x12345678, 7, 0x0000, 40)
	if len(f) != 11+20+2 {
		t.Fatalf("длина %d, want 33", len(f))
	}
	if f[0] != StartMarker || f[1] != 20 || f[2] != 0 {
		t.Fatalf("начало: % x", f[:3])
	}
	// control 0x4510 LE, serial 7 LE
	if f[3] != 0x10 || f[4] != 0x45 || f[5] != 7 || f[6] != 0 {
		t.Fatalf("control/serial: % x", f[3:7])
	}
	// DeviceSN LE
	wantSN := []byte{0x78, 0x56, 0x34, 0x12}
	if !bytes.Equal(f[7:11], wantSN) {
		t.Fatalf("SN: % x, want % x", f[7:11], wantSN)
	}
	// datafield: 02 + 11 нулей
	if f[11] != 0x02 {
		t.Fatalf("datafield[0]=0x%02X, want 0x02", f[11])
	}
	if !bytes.Equal(f[12:23], make([]byte, 11)) {
		t.Fatalf("datafield не нулевой: % x", f[12:23])
	}
	// PDU: 01 03 | start BE | count BE
	if !bytes.Equal(f[23:29], []byte{0x01, 0x03, 0x00, 0x00, 0x00, 0x28}) {
		t.Fatalf("PDU: % x", f[23:29])
	}
	// CRC16 LE по 6 байтам PDU
	crc := CRC16Modbus(f[23:29])
	if !bytes.Equal(f[29:31], []byte{byte(crc), byte(crc >> 8)}) {
		t.Fatalf("CRC: % x", f[29:31])
	}
	// Checksum8 и end-маркер
	if f[31] != Checksum8(f[1:31]) || f[32] != EndMarker {
		t.Fatalf("checksum/end: % x", f[31:33])
	}
}

func TestBuildDeyeReadFrameFn(t *testing.T) {
	// Deye: 15-байтный datafield (02 + 14 нулей) + 8-байтная PDU (unit+fn+2+2+CRC2).
	// PayloadLen = 15+8 = 23 -> "17 00" LE (как задокументировано).
	f := BuildDeyeReadFrameFn(0x12345678, 1, 7, 0x0000, 40, 0x03)
	if len(f) != 11+23+2 {
		t.Fatalf("длина %d, want 36", len(f))
	}
	if f[1] != 23 || f[3] != 0x10 || f[4] != 0x45 {
		t.Fatalf("len/control: % x", f[1:5])
	}
	if f[11] != 0x02 || !bytes.Equal(f[12:26], make([]byte, 14)) {
		t.Fatalf("datafield 15: % x", f[11:26])
	}
	// PDU: unit fn | start BE | count BE | crc LE
	pdu := f[26:34]
	if pdu[0] != 1 || pdu[1] != 0x03 || !bytes.Equal(pdu[2:6], []byte{0x00, 0x00, 0x00, 0x28}) {
		t.Fatalf("PDU: % x", pdu)
	}
	crc := CRC16Modbus(pdu[:6])
	if !bytes.Equal(pdu[6:8], []byte{byte(crc), byte(crc >> 8)}) {
		t.Fatalf("PDU CRC: % x", pdu[6:8])
	}
	if f[34] != Checksum8(f[1:34]) || f[35] != EndMarker {
		t.Fatalf("checksum/end: % x", f[34:36])
	}
}

func TestDeyeErrorCode(t *testing.T) {
	hb := []byte{0x02, 0x01, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x06, 0x00}
	fr := Frame{ControlCode: ResControlCode, Payload: hb}
	code, ok := DeyeErrorCode(fr)
	if !ok || code != 0x06 {
		t.Fatalf("код=0x%02X ok=%v, want 0x06", code, ok)
	}
	// Обычный heartbeat (0x00)
	hb[14] = 0x00
	if code, _ = DeyeErrorCode(fr); code != 0 {
		t.Fatalf("код=0x%02X, want 0x00", code)
	}
	// Не heartbeat (длина != 16) — false
	fr.Payload = hb[:10]
	if _, ok = DeyeErrorCode(fr); ok {
		t.Fatalf("для non-heartbeat ok=true")
	}
}

func TestParseModbusPDU(t *testing.T) {
	// PDU func 03: 01 03 04 | 0102 0304 | crc16 LE, в payload с offset 14.
	pdu := []byte{0x01, 0x03, 0x04, 0x01, 0x02, 0x03, 0x04, 0x00, 0x00}
	crc := CRC16Modbus(pdu[:7])
	pdu[7], pdu[8] = byte(crc), byte(crc>>8)
	payload := []byte{0x02, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	payload = append(payload, pdu...)

	pdus := ParseModbusPDU(payload)
	if len(pdus) != 1 {
		t.Fatalf("PDU: %d, want 1", len(pdus))
	}
	p := pdus[0]
	if p.Offset != 14 || p.Function != 0x03 || p.ByteCount != 4 {
		t.Fatalf("PDU: offset=%d fn=0x%02X len=%d", p.Offset, p.Function, p.ByteCount)
	}
	if len(p.Values) != 2 || p.Values[0] != 0x0102 || p.Values[1] != 0x0304 {
		t.Fatalf("Values: % x", p.Values)
	}
	if p.CRC != p.CRCCalc {
		t.Fatalf("CRC 0x%04X != расчёт 0x%04X", p.CRC, p.CRCCalc)
	}

	// ПДУ в нестандартном месте (15) — ищется только по явному offset.
	payload15 := []byte{0x02, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	payload15 = append(payload15, pdu...)
	if got := ParseModbusPDU(payload15); len(got) != 0 {
		t.Fatalf("по умолч. offset: найдено %d PDU", len(got))
	}
	if got := ParseModbusPDU(payload15, 15); len(got) != 1 || got[0].Offset != 15 {
		t.Fatalf("по offset 15: %d PDU", len(got))
	}
}

func TestParseResponseHeader(t *testing.T) {
	payload := make([]byte, 14)
	payload[0] = 0x02
	payload[1] = 0x01
	binary.LittleEndian.PutUint32(payload[2:6], 0x0A0B0C0D)
	binary.LittleEndian.PutUint32(payload[6:10], 1)
	binary.LittleEndian.PutUint32(payload[10:14], 2)
	h, ok := ParseResponseHeader(payload)
	if !ok || h.FrameType != 0x02 || h.Status != 0x01 ||
		h.DeliveryTime != 0x0A0B0C0D || h.PowerOnTime != 1 || h.OffsetTime != 2 {
		t.Fatalf("header: %+v ok=%v", h, ok)
	}
	if _, ok := ParseResponseHeader(payload[:10]); ok {
		t.Fatalf("короткий payload: ok=true")
	}
}
