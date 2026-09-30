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
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

// Живой payload Battery (0x61), 106 Б, устройства BP00 (MAC заменён на плейсхолдер)
// из /home/sasha/src/energybms/DEVICE_SNAPSHOT.md. Кадр ответа собирается в
// тесте (в документе полный кадр приведён с несовпадающим CRC — вероятна
// опечатка при копировании; декодированные значения payload совпадают).
const enbmsLiveBatteryPayload = "00 00 10 0c bd 0c bb 0c bb 0c bb 0c bb 0c ba 0c bb 0c bc 0c b8 0c ba 0c ba 0c bb 0c ba 0c bc 0c bb 0c bb 06 0b a3 0b 9b 0b 98 0b a5 0b cb 0b ad fb 2e 14 5e 27 f6 06 7a a8 01 45 7a a8 00 02 03 e8 14 61 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 01 03 08 00 00 00 00 00 00 00 00 00 00 00"

// enbmsResponseFrame собирает кадр ОТВЕТА BMS (7E 14 ADR CID2 LEN(2) INFO
// CHKSUM(2) 0D) с корректным CRC-16/CCITT — для тестов.
func enbmsResponseFrame(cid2 byte, payload []byte) []byte {
	body := []byte{0x14, 0x00, cid2, byte(len(payload) >> 8), byte(len(payload))}
	body = append(body, payload...)
	crc := crc16CCITT(body)
	fr := append([]byte{0x7e}, body...)
	return append(fr, byte(crc>>8), byte(crc), 0x0d)
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		t.Fatalf("hex: %v", err)
	}
	return b
}

func TestEnBmsBuildRequestFrame(t *testing.T) {
	got := buildEnBmsFrame(enBmsCID2Battery, []byte{0x00})
	want := "7e 10 00 46 61 00 01 00 f7 c1 0d"
	if h := hex.EncodeToString(got); h != strings.ReplaceAll(want, " ", "") {
		t.Fatalf("request frame = %s, want %s", h, want)
	}
}

func TestEnBmsExtractFrameByLength(t *testing.T) {
	// payload с 0x0D внутри: кадр должен собираться по длине, а не резаться по EOI.
	payload := []byte{0x0d, 0x0d, 0x00, 0x7e, 0x0d, 0x00}
	frame := enbmsResponseFrame(enBmsCID2Battery, payload)
	// Префикс-мусор + кадр + хвостовой мусор.
	buf := append([]byte{0x00, 0xff}, frame...)
	buf = append(buf, 0x11, 0x22)

	got, rest, ok := extractEnBmsFrame(buf)
	if !ok {
		t.Fatalf("кадр не найден в % x", buf)
	}
	if string(got) != string(frame) {
		t.Fatalf("извлечён неверный кадр:\n got % x\nwant % x", got, frame)
	}
	if string(rest) != string([]byte{0x11, 0x22}) {
		t.Fatalf("остаток = % x, want 11 22", rest)
	}
}

func TestEnBmsExtractIncomplete(t *testing.T) {
	frame := enbmsResponseFrame(enBmsCID2Battery, []byte{0x00})
	_, rest, ok := extractEnBmsFrame(frame[:len(frame)-1])
	if ok {
		t.Fatalf("неполный кадр не должен извлекаться")
	}
	if len(rest) == 0 {
		t.Fatalf("неполный кадр должен сохраняться в rest")
	}
}

func TestEnBmsParseBatteryLiveFrame(t *testing.T) {
	payload := mustHex(t, enbmsLiveBatteryPayload)
	if len(payload) != 106 {
		t.Fatalf("payload len = %d, want 106", len(payload))
	}
	frame := enbmsResponseFrame(enBmsCID2Battery, payload)
	if !crc16Valid(frame) {
		t.Fatalf("CRC тестового кадра неверен")
	}
	if lenid := int(frame[4])<<8 | int(frame[5]); lenid != 106 {
		t.Fatalf("LENID = %d, want 106", lenid)
	}
	// Кадр должен извлекаться из потока notify-фрагментов по длине.
	got, _, ok := extractEnBmsFrame(append([]byte{0x01, 0x02}, frame...))
	if !ok || string(got) != string(frame) {
		t.Fatalf("кадр не извлечён из потока")
	}
	r, err := parseEnBmsBattery(payload)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if r.BatteryNum != 16 || len(r.CellsV) != 16 {
		t.Fatalf("ячеек %d/%d, want 16", r.BatteryNum, len(r.CellsV))
	}
	if r.CellsV[0] != 3.261 || r.CellsV[15] != 3.259 {
		t.Fatalf("ячейки: first=%v last=%v", r.CellsV[0], r.CellsV[15])
	}
	if len(r.TemperaturesC) != 6 || r.TemperaturesC[0] != 24.8 {
		t.Fatalf("температуры: %v", r.TemperaturesC)
	}
	if r.CurrentA != -12.34 {
		t.Fatalf("ток = %v, want -12.34", r.CurrentA)
	}
	if r.TotalVoltageV != 52.14 {
		t.Fatalf("напряжение сборки = %v, want 52.14", r.TotalVoltageV)
	}
	if r.RemainingAh != 102.30 {
		t.Fatalf("остаток = %v, want 102.30", r.RemainingAh)
	}
	if r.CustomerP != 6 {
		t.Fatalf("customerp = %d, want 6", r.CustomerP)
	}
	if r.TotalCapacity != 314.00 {
		t.Fatalf("ёмкость = %v, want 314.00", r.TotalCapacity)
	}
	if r.Soc != 32.5 {
		t.Fatalf("SOC = %v, want 32.5", r.Soc)
	}
	if r.Cycles != 2 {
		t.Fatalf("циклы = %d, want 2", r.Cycles)
	}
	if r.Soh != 100.0 {
		t.Fatalf("SOH = %v, want 100.0", r.Soh)
	}
	if r.PortVoltageV != 52.17 {
		t.Fatalf("клеммы = %v, want 52.17", r.PortVoltageV)
	}
	if !enbmsParsedValid(r) {
		t.Fatalf("живые показания признаны невалидными")
	}

	cfg := enBmsDeviceConfig{Name: "BP00", MAC: "AA:BB:CC:DD:EE:00"}
	snap := enbmsSnapshotFromParsed(cfg, r, time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC))
	if snap.MaxCellIdx != 1 || snap.MinCellIdx != 9 {
		// max ячейка — 0x0cbd=3261 (индекс 1); min — 0x0cb8=3256 (индекс 9).
		t.Fatalf("max/min индексы = %d/%d, want 1/9", snap.MaxCellIdx, snap.MinCellIdx)
	}
	if snap.MAC != cfg.MAC || snap.CellCount != 16 {
		t.Fatalf("снимок: mac=%q cellcount=%d", snap.MAC, snap.CellCount)
	}
	if want := -643.4; snap.PowerW != want {
		t.Fatalf("мощность = %v, want %v", snap.PowerW, want)
	}
}

func TestEnBmsParsedValidRejectsGarbage(t *testing.T) {
	bad := enbmsParsed{BatteryNum: 1, CellsV: []float64{99.0}}
	if enbmsParsedValid(bad) {
		t.Fatalf("мусорные показания признаны валидными")
	}
}

func TestEnBmsConfigFromSection(t *testing.T) {
	// nil-раздел.
	if c, err := enBmsConfigFromSection(nil); err != nil || c != nil {
		t.Fatalf("nil: c=%v err=%v", c, err)
	}
	// Общий disabled=true.
	tr := true
	if c, err := enBmsConfigFromSection(&enBmsSection{Disabled: &tr}); err != nil || c != nil {
		t.Fatalf("disabled: c=%v err=%v", c, err)
	}
	// Устройство без disabled — ошибка.
	if _, err := enBmsConfigFromSection(&enBmsSection{Disabled: boolPtr(false), Devices: []enBmsDeviceSection{{MAC: "AA:BB:CC:DD:EE:FF"}}}); err == nil {
		t.Fatalf("устройство без disabled должно давать ошибку")
	}
	// Активное устройство без mac — ошибка.
	if _, err := enBmsConfigFromSection(&enBmsSection{Disabled: boolPtr(false), Devices: []enBmsDeviceSection{{Name: "x", Disabled: boolPtr(false)}}}); err == nil {
		t.Fatalf("активное устройство без mac должно давать ошибку")
	}
	// Дубликат MAC — ошибка.
	dup := &enBmsSection{Disabled: boolPtr(false), Devices: []enBmsDeviceSection{
		{MAC: "AA:BB:CC:DD:EE:FF", Disabled: boolPtr(false)},
		{MAC: "AA:BB:CC:DD:EE:FF", Disabled: boolPtr(false)},
	}}
	if _, err := enBmsConfigFromSection(dup); err == nil {
		t.Fatalf("дубликат mac должен давать ошибку")
	}
	// Валидный конфиг: отключённое устройство пропускается, имя по умолчанию.
	c, err := enBmsConfigFromSection(&enBmsSection{Disabled: boolPtr(false), Devices: []enBmsDeviceSection{
		{Name: "BP00", MAC: "AA:BB:CC:DD:EE:00", Disabled: boolPtr(false)},
		{MAC: "11:22:33:44:55:66", Disabled: boolPtr(true)},
	}})
	if err != nil {
		t.Fatalf("valid: %v", err)
	}
	if c == nil || len(c.Devices) != 1 {
		t.Fatalf("valid: devices=%v", c)
	}
	if c.Devices[0].Name != "BP00" || c.Devices[0].MAC != "AA:BB:CC:DD:EE:00" {
		t.Fatalf("valid device = %+v", c.Devices[0])
	}
	// Пустое имя → имя по умолчанию.
	c2, _ := enBmsConfigFromSection(&enBmsSection{Devices: []enBmsDeviceSection{
		{MAC: "AA:BB:CC:DD:EE:FF", Disabled: boolPtr(false)},
	}})
	if c2 == nil || c2.Devices[0].Name != "EnBMS AA:BB:CC:DD:EE:FF" {
		t.Fatalf("default name = %v", c2)
	}
}

func TestEnBmsAccumulatorBucket(t *testing.T) {
	start := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	acc := newEnBmsAccumulator()
	mk := func(ts time.Time, cur, soc float64) enbmsSnapshot {
		return enbmsSnapshot{
			Name: "BP00", MAC: "AA:BB:CC:DD:EE:00", Timestamp: ts.Format(time.RFC3339),
			CellCount: 2, CellsV: []float64{3.2, 3.3}, TemperaturesC: []float64{25.0},
			CurrentA: cur, Soc: soc, CapacityAh: 100, RemainingAh: 50,
			TotalVoltageV: 52.0, PortVoltageV: 52.1, Soh: 100, Cycles: 3,
			MaxCellV: 3.3, MinCellV: 3.2, AvgCellV: 3.25,
		}
	}
	acc.add(mk(start.Add(1*time.Second), 10, 50), start.Add(1*time.Second))
	acc.add(mk(start.Add(2*time.Second), 20, 52), start.Add(2*time.Second))
	// Снимок следующего 5-минутного окна закрывает предыдущий бакет.
	acc.add(mk(start.Add(5*time.Minute+1*time.Second), 30, 54), start.Add(5*time.Minute+1*time.Second))
	closed := acc.closed(start.Add(5*time.Minute + 2*time.Second))
	found := false
	for _, p := range closed {
		if p.start.Equal(start) {
			found = true
			if p.avg.Samples != 2 {
				t.Fatalf("samples = %d, want 2", p.avg.Samples)
			}
			if p.avg.CurrentA != 15.0 || p.avg.Soc != 51.0 {
				t.Fatalf("avg = %+v", p.avg)
			}
			if len(p.avg.CellsV) != 2 || p.avg.CellsV[1] != 3.3 {
				t.Fatalf("cells = %v", p.avg.CellsV)
			}
		}
	}
	if !found {
		t.Fatalf("закрытый бакет не найден: %+v", closed)
	}
}
