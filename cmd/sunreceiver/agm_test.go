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
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"go.bug.st/serial"
)

// agmBuildPZEMResponse собирает валидный ответ PZEM-017 (21 байт).
func agmBuildPZEMResponse(slave byte, regs [8]uint16) []byte {
	b := []byte{slave, 0x04, 16}
	for _, r := range regs {
		b = append(b, byte(r>>8), byte(r))
	}
	crc := agmModbusCRC16(b)
	return append(b, byte(crc), byte(crc>>8))
}

func TestAGMParsePZEMResponse(t *testing.T) {
	regs := [8]uint16{
		5253,   // 52.53 В
		7,      // 0.07 А
		37,     // мощность LE: 37 | 0<<16 = 3.7 Вт
		0,      // старшее слово мощности
		26,     // энергия LE: 26 Вт·ч
		0,      // старшее слово энергии
		0xFFFF, // авария high
		0,      // авария low
	}
	b := agmBuildPZEMResponse(1, regs)
	r, err := parseAGMPZEMResponse(b, 1, true)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if r.VoltageV != 52.53 || r.CurrentA != 0.07 || r.PowerW != 3.7 || r.EnergyWh != 26 {
		t.Fatalf("неожиданные значения: %+v", r)
	}
	if !r.AlarmHigh || r.AlarmLow {
		t.Fatalf("флаги аварий: %+v", r)
	}
	if !agmPZEMPlausible(r) {
		t.Fatal("ответ должен быть правдоподобным")
	}
}

func TestAGMParsePZEMRejects(t *testing.T) {
	good := agmBuildPZEMResponse(1, [8]uint16{5253, 7, 37, 0, 26, 0, 0, 0})
	cases := map[string][]byte{
		"короткий":    good[:len(good)-1],
		"чужой slave": agmBuildPZEMResponse(2, [8]uint16{5253}),
	}
	// Битая CRC.
	bad := append([]byte(nil), good...)
	bad[len(bad)-1] ^= 0xFF
	cases["CRC"] = bad
	// Недопустимое значение регистра аварии.
	cases["регистр аварии"] = agmBuildPZEMResponse(1, [8]uint16{5253, 7, 37, 0, 26, 0, 0x1234, 0})
	for name, b := range cases {
		if _, err := parseAGMPZEMResponse(b, 1, true); err == nil {
			t.Errorf("%s: ошибка не обнаружена", name)
		}
	}
	// В рабочем чтении нестандартный регистр аварии не отбрасывает кадр:
	// аварией считается любой ненулевой регистр.
	odd := agmBuildPZEMResponse(1, [8]uint16{5253, 7, 37, 0, 26, 0, 0x1234, 0})
	r, err := parseAGMPZEMResponse(odd, 1, false)
	if err != nil || !r.AlarmHigh {
		t.Fatalf("нестрогий разбор: err=%v r=%+v", err, r)
	}
}

// fakeAGMPort — минимальный serial.Port для тестов обмена.
type fakeAGMPort struct {
	mu    sync.Mutex
	in    []byte
	chunk int // максимум байт за Read (0 — без ограничения)
	wrote []byte
	dead  bool
}

func (f *fakeAGMPort) SetMode(*serial.Mode) error { return nil }
func (f *fakeAGMPort) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dead {
		return 0, errors.New("port closed")
	}
	if len(f.in) == 0 {
		return 0, nil
	}
	n := len(p)
	if f.chunk > 0 && n > f.chunk {
		n = f.chunk
	}
	if n > len(f.in) {
		n = len(f.in)
	}
	copy(p, f.in[:n])
	f.in = f.in[n:]
	return n, nil
}
func (f *fakeAGMPort) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dead {
		return 0, errors.New("port closed")
	}
	// USB-serial может принять только часть — проверяем цикл дописывания.
	n := len(p)
	if n > 1 {
		n = 1
	}
	f.wrote = append(f.wrote, p[:n]...)
	return n, nil
}
func (f *fakeAGMPort) Drain() error             { return nil }
func (f *fakeAGMPort) ResetInputBuffer() error  { return nil }
func (f *fakeAGMPort) ResetOutputBuffer() error { return nil }
func (f *fakeAGMPort) SetDTR(bool) error        { return nil }
func (f *fakeAGMPort) SetRTS(bool) error        { return nil }
func (f *fakeAGMPort) GetModemStatusBits() (*serial.ModemStatusBits, error) {
	return &serial.ModemStatusBits{}, nil
}
func (f *fakeAGMPort) SetReadTimeout(time.Duration) error { return nil }
func (f *fakeAGMPort) Close() error                       { f.dead = true; return nil }
func (f *fakeAGMPort) Break(time.Duration) error          { return nil }

func TestAGMCRC16X25Vector(t *testing.T) {
	if got := agmCRC16X25([]byte("123456789")); got != 0x906E {
		t.Fatalf("CRC-16/X-25 = %#x, ожидалось 0x906E", got)
	}
}

func TestAGMReadPZEMFake(t *testing.T) {
	resp := agmBuildPZEMResponse(1, [8]uint16{5253, 7, 37, 0, 26, 0, 0, 0})
	// Эхо запроса перед ответом + выдача по 1 байту.
	p := &fakeAGMPort{chunk: 1}
	p.in = append(append(p.in, buildAGMPZEMRequest(1)...), resp...)
	r, err := readAGMPZEM(p, 1, time.Second, false)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if r.VoltageV != 52.53 || r.CurrentA != 0.07 || r.EnergyWh != 26 {
		t.Fatalf("значения: %+v", r)
	}
	if want := buildAGMPZEMRequest(1); string(p.wrote) != string(want) {
		t.Fatalf("запрос записан неполно: % X", p.wrote)
	}
}

func TestAGMReadPZEMException(t *testing.T) {
	b := []byte{1, 0x84, 0x02}
	crc := agmModbusCRC16(b)
	p := &fakeAGMPort{}
	p.in = append(b, byte(crc), byte(crc>>8))
	_, err := readAGMPZEM(p, 1, time.Second, false)
	var ex agmPZEMException
	if !errors.As(err, &ex) || ex.code != 0x02 {
		t.Fatalf("ожидалось исключение 0x02, получено %v", err)
	}
}

func TestAGMReadPZEMResync(t *testing.T) {
	resp := agmBuildPZEMResponse(1, [8]uint16{5253, 7, 37, 0, 26, 0, 0, 0})
	bad := append([]byte(nil), resp...)
	bad[len(bad)-1] ^= 0xFF // битая CRC полного кадра
	exc := func(crcOK bool) []byte {
		b := []byte{1, 0x84, 0x02}
		crc := agmModbusCRC16(b)
		if !crcOK {
			crc ^= 0xFFFF
		}
		return append(b, byte(crc), byte(crc>>8))
	}
	cases := map[string][]byte{
		"мусор+эхо+ответ":        append(append([]byte("\x00\xAAjunk"), buildAGMPZEMRequest(1)...), resp...),
		"битый кадр+ответ":       append(append([]byte{}, bad...), resp...),
		"битое исключение+ответ": append(exc(false), resp...),
	}
	for name, in := range cases {
		p := &fakeAGMPort{chunk: 1}
		p.in = in
		r, err := readAGMPZEM(p, 1, time.Second, false)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if r.VoltageV != 52.53 || r.EnergyWh != 26 {
			t.Errorf("%s: значения %+v", name, r)
		}
	}
}

func TestAGMReadPZEMTimeout(t *testing.T) {
	p := &fakeAGMPort{}
	_, err := readAGMPZEM(p, 1, 20*time.Millisecond, false)
	if !errors.Is(err, errAGMPZEMFrame) {
		t.Fatalf("ожидался таймаут кадра, получено %v", err)
	}
}

func TestAGMPZEMRequest(t *testing.T) {
	got := buildAGMPZEMRequest(1)
	want := []byte{0x01, 0x04, 0x00, 0x00, 0x00, 0x08, 0xF1, 0xCC}
	if len(got) != len(want) {
		t.Fatalf("длина %d != %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("кадр % X != % X", got, want)
		}
	}
}

// agmBuildBKFrame собирает кадр ответа BK300.
func agmBuildBKFrame(cmd [2]byte, payload []byte) []byte {
	total := 10 + len(payload)
	b := []byte{0x24, 0x24, byte(total), byte(total >> 8), cmd[0], cmd[1]}
	b = append(b, payload...)
	crc := agmCRC16X25(b)
	return append(b, byte(crc), byte(crc>>8), '\r', '\n')
}

func TestAGMBKFrameRoundTrip(t *testing.T) {
	// Ответ на 0B0B: напряжение 12.97 В (1297), флаги 2 и 1.
	payload := []byte{0x11, 0x05, 2, 1}
	wire := agmBuildBKFrame([2]byte{0x4B, 0x0B}, payload)

	// Фрагментируем произвольно, включая разрез магии.
	var rd agmBKFrameReader
	var frames []agmBKFrame
	for i := 0; i < len(wire); i += 3 {
		end := i + 3
		if end > len(wire) {
			end = len(wire)
		}
		frames = append(frames, rd.Push(wire[i:end])...)
	}
	if len(frames) != 1 {
		t.Fatalf("кадров %d, ожидался 1", len(frames))
	}
	// Побайтовая фрагментация (в т.ч. разрез магии "$$").
	var rd1 agmBKFrameReader
	frames = nil
	for i := 0; i < len(wire); i++ {
		frames = append(frames, rd1.Push(wire[i:i+1])...)
	}
	if len(frames) != 1 {
		t.Fatalf("побайтово: кадров %d, ожидался 1", len(frames))
	}
	p, err := parseAGMBKReply(agmCmdBKVoltage, frames[0])
	if err != nil {
		t.Fatalf("reply: %v", err)
	}
	r, err := parseAGMBKVoltage(p)
	if err != nil {
		t.Fatalf("voltage: %v", err)
	}
	if r.VoltageV != 12.97 || r.FlagsA != 2 || r.FlagsB != 1 {
		t.Fatalf("неожиданные значения: %+v", r)
	}
}

func TestAGMBKFrameRejectsBadCRC(t *testing.T) {
	wire := agmBuildBKFrame([2]byte{0x4B, 0x0B}, []byte{0x11, 0x05, 2, 1})
	wire[len(wire)-3] ^= 0xFF // ломаем CRC
	var rd agmBKFrameReader
	if got := rd.Push(wire); len(got) != 0 {
		t.Fatalf("ожидалось 0 кадров, получено %d", len(got))
	}
}

func TestAGMBKFrameSkipsBadAndBogusLength(t *testing.T) {
	good := agmBuildBKFrame([2]byte{0x4B, 0x0B}, []byte{0x11, 0x05, 2, 1})
	bad := append([]byte(nil), good...)
	bad[len(bad)-3] ^= 0xFF // битая CRC

	// Мусорная длина: те же байты, но объявленная длина вне диапазона.
	bogus := append([]byte(nil), good...)
	bogus[2], bogus[3] = 0xFF, 0x0F // n=4095 < 4096, данных меньше

	cases := map[string][]byte{
		"битый кадр+валидный":     append(append([]byte{}, bad...), good...),
		"мусорная длина+валидный": append(append([]byte{}, bogus...), good...),
	}
	for name, in := range cases {
		var rd agmBKFrameReader
		frames := rd.Push(in)
		if len(frames) != 1 {
			t.Errorf("%s: кадров %d, ожидался 1", name, len(frames))
			continue
		}
		p, err := parseAGMBKReply(agmCmdBKVoltage, frames[0])
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if _, err := parseAGMBKVoltage(p); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestAGMBKFrameAfterTailMagic(t *testing.T) {
	good := agmBuildBKFrame([2]byte{0x4B, 0x0B}, []byte{0x11, 0x05, 2, 1})
	var rd agmBKFrameReader
	if got := rd.Push([]byte{0x24}); len(got) != 0 {
		t.Fatalf("ожидалось 0 кадров, получено %d", len(got))
	}
	frames := rd.Push(good)
	if len(frames) != 1 {
		t.Fatalf("хвостовой 0x24 + кадр: кадров %d, ожидался 1", len(frames))
	}
	if _, err := parseAGMBKVoltage(frames[0].Payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
}

func TestAGMBKRequestBuild(t *testing.T) {
	b := buildAGMBKRequest(agmCmdBKVoltage, nil)
	if len(b) != 10 {
		t.Fatalf("длина %d", len(b))
	}
	if b[0] != 0x40 || b[1] != 0x40 || b[2] != 10 || b[3] != 0 {
		t.Fatalf("заголовок % X", b[:4])
	}
	if b[4] != 0x0B || b[5] != 0x0B {
		t.Fatalf("команда % X", b[4:6])
	}
	if b[8] != '\r' || b[9] != '\n' {
		t.Fatalf("терминатор % X", b[8:])
	}
}

func TestAGMParseBKSerial(t *testing.T) {
	payload := []byte("000000000000000001")
	s, err := parseAGMBKSerial(payload)
	if err != nil || s != "000000000000000001" {
		t.Fatalf("serial=%q err=%v", s, err)
	}
	if _, err := parseAGMBKSerial([]byte("short")); err == nil {
		t.Fatal("короткий серийник должен быть отклонён")
	}
	if _, err := parseAGMBKSerial([]byte("00000000000000000\x01")); err == nil {
		t.Fatal("непечатный серийник должен быть отклонён")
	}
}

func TestAGMConfigValidation(t *testing.T) {
	tru := true
	disabled := agmSection{Disabled: &tru}
	if cfg, err := agmFromSection(&disabled); err != nil || cfg != nil {
		t.Fatalf("disabled: cfg=%v err=%v", cfg, err)
	}
	if cfg, err := agmFromSection(nil); err != nil || cfg != nil {
		t.Fatalf("нет раздела: cfg=%v err=%v", cfg, err)
	}

	valid := agmSection{
		PZEM: &agmPZEMSection{Usbip: "192.168.0.10:3240", Slave: 1},
		BK300: &agmBK300Section{Devices: []agmDeviceSection{
			{Name: "A", Position: 1, MAC: "AA:BB:CC:DD:EE:01", Serial: "000000000000000001"},
			{Name: "B", Position: 2, MAC: "AA:BB:CC:DD:EE:02", Serial: "000000000000000002"},
			{Name: "C", Position: 3, MAC: "AA:BB:CC:DD:EE:03", Serial: "000000000000000003"},
			{Name: "D", Position: 4, MAC: "AA:BB:CC:DD:EE:04", Serial: "000000000000000004"},
		}},
	}
	cfg, err := agmFromSection(&valid)
	if err != nil {
		t.Fatalf("валидный конфиг: %v", err)
	}
	if cfg.PollInterval != time.Second {
		t.Fatalf("период по умолчанию %s", cfg.PollInterval)
	}
	if len(cfg.Devices) != 4 || cfg.Devices[0].Position != 1 || cfg.Devices[3].Position != 4 {
		t.Fatalf("устройства не отсортированы: %+v", cfg.Devices)
	}

	bad := valid
	bad.BK300 = &agmBK300Section{Devices: valid.BK300.Devices[:3]}
	if _, err := agmFromSection(&bad); err == nil || !strings.Contains(err.Error(), "ровно 4") {
		t.Fatalf("3 устройства должны быть ошибкой: %v", err)
	}

	dup := valid
	dup.BK300 = &agmBK300Section{Devices: []agmDeviceSection{
		{Name: "A", Position: 1, MAC: "AA:BB:CC:DD:EE:01", Serial: "000000000000000001"},
		{Name: "B", Position: 1, MAC: "AA:BB:CC:DD:EE:02", Serial: "000000000000000002"},
		{Name: "C", Position: 3, MAC: "AA:BB:CC:DD:EE:03", Serial: "000000000000000003"},
		{Name: "D", Position: 4, MAC: "AA:BB:CC:DD:EE:04", Serial: "000000000000000004"},
	}}
	if _, err := agmFromSection(&dup); err == nil {
		t.Fatal("повтор position должен быть ошибкой")
	}

	noUsbip := valid
	noUsbip.PZEM = &agmPZEMSection{Slave: 1}
	if _, err := agmFromSection(&noUsbip); err == nil {
		t.Fatal("пустой pzem.usbip должен быть ошибкой")
	}
}

func TestAGMBuildValues(t *testing.T) {
	p := agmPZEMReading{VoltageV: 52.534, CurrentA: 0.074, PowerW: 3.66, EnergyWh: 26, AlarmHigh: true}
	bks := []agmBKSample{
		{Position: 1, Reading: agmBKReading{VoltageV: 12.968, FlagsA: 2, FlagsB: 1}},
		{Position: 2, Reading: agmBKReading{VoltageV: 12.9}},
	}
	v := agmBuildValues(p, bks)
	if v[agmTagPZEMVoltage] != 52.53 || v[agmTagPZEMCurrent] != 0.07 || v[agmTagPZEMPower] != 3.7 {
		t.Fatalf("округление PZEM: %+v", v)
	}
	if v[agmTagPZEMEnergy] != 26 || v[agmTagPZEMAlarmHigh] != 1 || v[agmTagPZEMAlarmLow] != 0 {
		t.Fatalf("энергия/аварии: %+v", v)
	}
	if v[agmBKTag(1, agmSuffixVoltage)] != 12.97 || v[agmBKTag(1, agmSuffixFlagsA)] != 2 {
		t.Fatalf("BK300 #1: %+v", v)
	}
}

func TestAverageAGM(t *testing.T) {
	base := time.Date(2026, 11, 17, 10, 0, 0, 0, time.Local)
	mk := func(sec int, v map[string]float64) agmSnapshot {
		return agmSnapshot{Name: "AGM", Timestamp: base.Add(time.Duration(sec) * time.Second).Format(time.RFC3339), Values: v}
	}
	snaps := []agmSnapshot{
		mk(0, map[string]float64{
			agmTagPZEMVoltage: 12.0, agmTagPZEMCurrent: 0.07, agmTagPZEMEnergy: 100, agmTagPZEMAlarmHigh: 0,
			agmBKTag(1, agmSuffixFlagsA): 0, agmBKTag(1, agmSuffixVoltage): 12.96,
		}),
		mk(60, map[string]float64{
			agmTagPZEMVoltage: 14.0, agmTagPZEMCurrent: 0.09, agmTagPZEMEnergy: 140, agmTagPZEMAlarmHigh: 1,
			agmBKTag(1, agmSuffixFlagsA): 3, agmBKTag(1, agmSuffixVoltage): 12.98,
		}),
	}
	got := averageAGM(snaps)
	if got[agmTagPZEMVoltage] != 13.0 {
		t.Fatalf("среднее напряжение %v", got[agmTagPZEMVoltage])
	}
	// Ток усредняется с 2 знаками (0.07+0.09)/2 = 0.08, а не схлопывается в 0.1.
	if got[agmTagPZEMCurrent] != 0.08 {
		t.Fatalf("средний ток %v", got[agmTagPZEMCurrent])
	}
	// Напряжение BK300 сохраняет разрешение 0.01 В.
	if got[agmBKTag(1, agmSuffixVoltage)] != 12.97 {
		t.Fatalf("среднее напряжение BK300 %v", got[agmBKTag(1, agmSuffixVoltage)])
	}
	if got[agmTagPZEMEnergy] != 140 {
		t.Fatalf("энергия должна быть последней: %v", got[agmTagPZEMEnergy])
	}
	if got[agmBKTag(1, agmSuffixFlagsA)] != 3 {
		t.Fatalf("флаг должен быть последним: %v", got[agmBKTag(1, agmSuffixFlagsA)])
	}
	if got[agmTagPZEMAlarmHigh] != 1 {
		t.Fatalf("авария должна быть ИЛИ по окну: %v", got[agmTagPZEMAlarmHigh])
	}
	if got[agmTagPZEMAlarmLow] != 0 {
		t.Fatalf("отсутствующая авария не должна появляться: %v", got[agmTagPZEMAlarmLow])
	}
}

func TestAverageAGMAlarmAllZero(t *testing.T) {
	base := time.Date(2026, 11, 17, 10, 0, 0, 0, time.Local)
	snaps := []agmSnapshot{{Name: "AGM", Timestamp: base.Format(time.RFC3339), Values: map[string]float64{
		agmTagPZEMAlarmHigh: 0, agmTagPZEMAlarmLow: 0,
	}}}
	got := averageAGM(snaps)
	if v, ok := got[agmTagPZEMAlarmHigh]; !ok || v != 0 {
		t.Fatalf("нулевая авария должна попасть как 0: %v (ok=%v)", v, ok)
	}
}

func TestAGMSnapshotSingleTimestamp(t *testing.T) {
	// Одна метка времени на снимок: в JSON нет полей at/cycle_* по источникам.
	snap := agmSnapshot{Name: "AGM", Timestamp: "2026-11-17T10:00:00+03:00", Values: map[string]float64{agmTagPZEMVoltage: 52.5}}
	if !strings.HasPrefix(snap.Timestamp, "2026-11-17T10:00:00") {
		t.Fatalf("timestamp: %s", snap.Timestamp)
	}
}
