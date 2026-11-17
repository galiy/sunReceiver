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
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"go.bug.st/serial"
)

// PZEM-017 (Modbus RTU, 9600 8N1, только чтение). Подробности и происхождение
// протокола — docs/modules/agm-monitor.md §2.1. Порт приходит по usbip как
// локальный USB-serial; поднимает/переподнимает его внешний watchdog, демон
// только находит, проверяет активной пробой и удерживает открытым.

const (
	agmPZEMBaud      = 9600
	agmPZEMRegs      = 8
	agmPZEMFrameLen  = 3 + 2*agmPZEMRegs + 2 // unit + fn + bytecount + данные + CRC
	agmPZEMReadStart = 0x0000
)

// Таймауты обмена и поиска порта (константы: в конфиг не выносятся).
const (
	agmPZEMProbeTimeout  = 300 * time.Millisecond
	agmPZEMReadTimeout   = 300 * time.Millisecond
	agmPZEMScanInterval  = 10 * time.Second
	agmPZEMForeignDelay  = 30 * time.Second
	agmPZEMInterFrameGap = 4 * time.Millisecond // t3.5 при 9600
)

var (
	// errAGMPZEMFrame — ответ не похож на PZEM (длина/CRC/поля).
	errAGMPZEMFrame = errors.New("некорректный ответ PZEM-017")
	// errAGMPZEMLost — порт не удалось восстановить; уходим в режим поиска.
	errAGMPZEMLost = errors.New("PZEM-017 недоступен, порт потерян")
)

// agmModbusCRC16 — CRC16 Modbus (poly 0xA001, init 0xFFFF).
func agmModbusCRC16(b []byte) uint16 {
	crc := uint16(0xFFFF)
	for _, c := range b {
		crc ^= uint16(c)
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

// buildAGMPZEMRequest собирает запрос чтения 8 input-регистров.
func buildAGMPZEMRequest(slave byte) []byte {
	b := []byte{slave, 0x04, byte(agmPZEMReadStart >> 8), byte(agmPZEMReadStart), 0x00, agmPZEMRegs}
	crc := agmModbusCRC16(b)
	return append(b, byte(crc), byte(crc>>8))
}

// parseAGMPZEMResponse разбирает успешный ответ (21 байт). strict=true (активная
// проба при поиске порта) требует, чтобы регистры аварий были строго 0x0000/
// 0xFFFF — иначе это не PZEM. В рабочем чтении такие значения не отбрасываем:
// аварией считаем любой ненулевой регистр.
func parseAGMPZEMResponse(b []byte, slave byte, strict bool) (agmPZEMReading, error) {
	if len(b) != agmPZEMFrameLen {
		return agmPZEMReading{}, fmt.Errorf("%w: длина %d", errAGMPZEMFrame, len(b))
	}
	if b[0] != slave {
		return agmPZEMReading{}, fmt.Errorf("%w: slave %d != %d", errAGMPZEMFrame, b[0], slave)
	}
	if b[1] != 0x04 || b[2] != 2*agmPZEMRegs {
		return agmPZEMReading{}, fmt.Errorf("%w: fn=%#x bc=%d", errAGMPZEMFrame, b[1], b[2])
	}
	crc := binary.LittleEndian.Uint16(b[len(b)-2:])
	if agmModbusCRC16(b[:len(b)-2]) != crc {
		return agmPZEMReading{}, fmt.Errorf("%w: CRC", errAGMPZEMFrame)
	}
	d := b[3 : 3+2*agmPZEMRegs]
	reg := func(i int) uint16 { return binary.BigEndian.Uint16(d[2*i:]) }
	if strict {
		// Флаги аварий по протоколу только 0x0000/0xFFFF; иное — не PZEM.
		for _, i := range []int{6, 7} {
			if v := reg(i); v != 0 && v != 0xFFFF {
				return agmPZEMReading{}, fmt.Errorf("%w: регистр аварии %d = %#x", errAGMPZEMFrame, i, v)
			}
		}
	}
	powerRaw := uint32(reg(2)) | uint32(reg(3))<<16
	energyRaw := uint32(reg(4)) | uint32(reg(5))<<16
	return agmPZEMReading{
		VoltageV:  float64(reg(0)) / 100,
		CurrentA:  float64(reg(1)) / 100,
		PowerW:    float64(powerRaw) / 10,
		EnergyWh:  energyRaw,
		AlarmHigh: reg(6) != 0,
		AlarmLow:  reg(7) != 0,
	}, nil
}

// agmPZEMPlausible — правдоподобность ответа для активной пробы порта.
func agmPZEMPlausible(r agmPZEMReading) bool {
	if r.VoltageV < 0.05 || r.VoltageV > 300 {
		return false
	}
	if r.CurrentA > 300 {
		return false
	}
	if r.PowerW > 120000 {
		return false
	}
	return true
}

// agmPZEMException — валидный ответ-исключение Modbus (чужое устройство/адрес).
type agmPZEMException struct{ code byte }

func (e agmPZEMException) Error() string {
	return fmt.Sprintf("PZEM-017: исключение Modbus %#x", e.code)
}

// agmPZEMStart ищет начало ответа в буфере: (slave, 0x04, 0x10) или (slave, 0x84).
// -1 — начала нет.
func agmPZEMStart(b []byte, slave byte) int {
	for i := 0; i+1 < len(b); i++ {
		if b[i] != slave {
			continue
		}
		if b[i+1] == 0x84 || (b[i+1] == 0x04 && i+2 < len(b) && b[i+2] == 2*agmPZEMRegs) {
			return i
		}
	}
	return -1
}

// writeAGMPZEM дописывает кадр до конца: USB-serial может принять только часть.
func writeAGMPZEM(p serial.Port, b []byte) error {
	for len(b) > 0 {
		n, err := p.Write(b)
		if err != nil {
			return err
		}
		if n <= 0 {
			return errors.New("PZEM-017: запись не принята")
		}
		b = b[n:]
	}
	return nil
}

// agmPZEMParseBuffer пытается выделить из накопленного буфера ответ или
// исключение. done=true — результат окончательный (успех, исключение или ошибка
// кадра); done=false — данных ещё мало либо мусор отброшен, читаем дальше.
func agmPZEMParseBuffer(buf *[]byte, slave byte, strict bool) (agmPZEMReading, error, bool) {
	b := *buf
	if i := agmPZEMStart(b, slave); i > 0 {
		b = append(b[:0], b[i:]...)
	} else if i < 0 {
		// Начала нет: держим только хвост, где магия может быть разрезана.
		if len(b) > 64 {
			b = b[len(b)-64:]
		}
		*buf = b
		return agmPZEMReading{}, nil, false
	}
	*buf = b
	if len(b) >= 2 && b[0] == slave && b[1] == 0x84 {
		if len(b) >= 5 {
			if binary.LittleEndian.Uint16(b[3:5]) == agmModbusCRC16(b[:3]) {
				return agmPZEMReading{}, agmPZEMException{code: b[2]}, true
			}
			*buf = b[1:] // битый кадр исключения — ищем дальше
		}
		return agmPZEMReading{}, nil, false
	}
	if len(b) >= agmPZEMFrameLen && b[0] == slave && b[1] == 0x04 && b[2] == 2*agmPZEMRegs {
		r, err := parseAGMPZEMResponse(b[:agmPZEMFrameLen], slave, strict)
		if err == nil {
			return r, nil, true
		}
		if errors.Is(err, errAGMPZEMFrame) {
			// Битая CRC/поля: сдвигаемся на байт и ищем следующее начало.
			*buf = b[1:]
			return agmPZEMReading{}, nil, false
		}
		return agmPZEMReading{}, err, true
	}
	return agmPZEMReading{}, nil, false
}

// readAGMPZEM отправляет кадр и читает ответ до дедлайна. strict — активная
// проба (проверка «это PZEM»), а не рабочее чтение.
func readAGMPZEM(p serial.Port, slave byte, timeout time.Duration, strict bool) (agmPZEMReading, error) {
	if p == nil {
		return agmPZEMReading{}, errAGMPZEMLost
	}
	if err := p.ResetInputBuffer(); err != nil {
		return agmPZEMReading{}, err
	}
	// t3.5 при 9600 — около 4 мс межкадрового интервала.
	time.Sleep(agmPZEMInterFrameGap)
	if err := writeAGMPZEM(p, buildAGMPZEMRequest(slave)); err != nil {
		return agmPZEMReading{}, err
	}
	deadline := time.Now().Add(timeout)
	buf := make([]byte, 0, agmPZEMFrameLen+8)
	tmp := make([]byte, 32)
	for {
		// Разбор накопленного — независимо от того, пришли ли новые байты.
		if r, err, done := agmPZEMParseBuffer(&buf, slave, strict); done {
			return r, err
		}
		rem := time.Until(deadline)
		if rem <= 0 {
			return agmPZEMReading{}, fmt.Errorf("%w: таймаут ответа", errAGMPZEMFrame)
		}
		if err := p.SetReadTimeout(rem); err != nil {
			return agmPZEMReading{}, err
		}
		n, err := p.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			return agmPZEMReading{}, err
		}
		if n == 0 {
			// SetReadTimeout истёк — последний разбор накопленного.
			if r, perr, done := agmPZEMParseBuffer(&buf, slave, strict); done {
				return r, perr
			}
			return agmPZEMReading{}, fmt.Errorf("%w: таймаут ответа", errAGMPZEMFrame)
		}
	}
}

// openAGMSerial открывает порт на 9600 8N1.
func openAGMSerial(path string) (serial.Port, error) {
	p, err := serial.Open(path, &serial.Mode{
		BaudRate: agmPZEMBaud,
		DataBits: 8,
		Parity:   serial.NoParity,
		StopBits: serial.OneStopBit,
	})
	if err != nil {
		return nil, err
	}
	return p, nil
}

// agmPZEMClient — закреплённый порт PZEM-017. Порт открыт постоянно: линия занята
// на всё время мониторинга.
type agmPZEMClient struct {
	mu      sync.Mutex
	port    serial.Port
	portRef atomic.Pointer[agmPortRef] // для Close без ожидания mu
	path    string
	slave   byte
	timeout time.Duration
}

// agmPortRef — обёртка порта для lock-free Close (typed-nil не кладём в atomic).
type agmPortRef struct{ p serial.Port }

// setPort запоминает порт под mu и обновляет lock-free ссылку.
func (c *agmPZEMClient) setPort(p serial.Port) {
	c.port = p
	if p == nil {
		c.portRef.Store(nil)
		return
	}
	c.portRef.Store(&agmPortRef{p: p})
}

// Path — путь закреплённого порта.
func (c *agmPZEMClient) Path() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.path
}

// Read читает измерения. При ошибке обмена один раз переоткрывает порт; если и
// это не удалось — возвращает errAGMPZEMLost (демон уходит в режим поиска).
// Ответ-исключение Modbus — валидный ответ чужого устройства: порт не
// переоткрываем.
func (c *agmPZEMClient) Read() (agmPZEMReading, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.port == nil {
		return agmPZEMReading{}, errAGMPZEMLost
	}
	r, err := readAGMPZEM(c.port, c.slave, c.timeout, false)
	if err == nil {
		return r, nil
	}
	var ex agmPZEMException
	if errors.As(err, &ex) {
		return agmPZEMReading{}, err
	}
	log.Printf("agm: PZEM-017: обмен не удался (%v) — переоткрываю %s", err, c.path)
	_ = c.port.Close()
	c.setPort(nil)

	p, oerr := openAGMSerial(c.path)
	if oerr != nil {
		return agmPZEMReading{}, fmt.Errorf("%w: %v", errAGMPZEMLost, oerr)
	}
	c.setPort(p)
	r, err = readAGMPZEM(c.port, c.slave, c.timeout, false)
	if err != nil {
		_ = c.port.Close()
		c.setPort(nil)
		// Исключение Modbus на повторе — такой же валидный ответ чужого
		// устройства, как и на первом чтении: не выдаём за потерю порта.
		if errors.As(err, &ex) {
			return agmPZEMReading{}, err
		}
		return agmPZEMReading{}, fmt.Errorf("%w: %v", errAGMPZEMLost, err)
	}
	log.Printf("agm: PZEM-017: связь восстановлена на %s", c.path)
	return r, nil
}

// Close закрывает порт (освобождает линию). Ожидание мьютекса ограничено:
// если обмен завис сверх таймаутов, Close не должен вешать остановку фазы.
func (c *agmPZEMClient) Close() {
	// Принудительное закрытие порта без ожидания c.mu: serial.Port.Close
	// безопасен параллельно с Read и разблокирует зависший обмен, иначе
	// следующая фаза получит PortBusy и порт не освободится.
	if ref := c.portRef.Load(); ref != nil && ref.p != nil {
		_ = ref.p.Close()
	}
	done := make(chan struct{})
	go func() {
		c.mu.Lock()
		if c.port != nil {
			_ = c.port.Close()
			c.setPort(nil)
		}
		c.mu.Unlock()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * agmPZEMReadTimeout):
		log.Printf("agm: PZEM-017: Close не дождался завершения обмена за %s", 2*agmPZEMReadTimeout)
	}
}

// agmPZEMCandidates — уникальные пути-кандидаты по шаблонам конфига. Симлинки
// by-id разрешаются, чтобы не проверять один и тот же порт дважды.
func agmPZEMCandidates(globs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, g := range globs {
		matches, err := filepath.Glob(g)
		if err != nil {
			continue
		}
		for _, path := range matches {
			st, err := os.Stat(path)
			if err != nil || st.IsDir() {
				continue
			}
			// Кандидат — символьное устройство (Linux); прочие типы файлов не
			// проверяем.
			if st.Mode()&os.ModeCharDevice == 0 {
				continue
			}
			key := path
			if resolved, err := filepath.EvalSymlinks(path); err == nil {
				key = resolved
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, path)
		}
	}
	return out
}

// agmSerialBusy — порт занят другим процессом (PortBusy).
func agmSerialBusy(err error) bool {
	var pe *serial.PortError
	return errors.As(err, &pe) && pe.Code() == serial.PortBusy
}

// findAGMPZEM ищет свободный USB-serial порт с PZEM-017 и закрепляет его. Порты,
// не ответившие как PZEM, повторно не проверяются agmPZEMForeignDelay. Проход
// поиска — раз в agmPZEMScanInterval. ok=false — остановка по контексту.
func findAGMPZEM(ctx context.Context, cfg *agmConfig, errs *agmErrTracker) (*agmPZEMClient, bool) {
	backoff := make(map[string]time.Time)
	log.Printf("agm: состояние: поиск PZEM-017 на USB-serial (usbip %s)", cfg.PZEM.Usbip)
	for {
		for _, path := range agmPZEMCandidates(cfg.PZEM.PathGlobs) {
			if ctx.Err() != nil {
				return nil, false
			}
			key := path
			if resolved, err := filepath.EvalSymlinks(path); err == nil {
				key = resolved
			}
			if t, ok := backoff[key]; ok && time.Now().Before(t) {
				continue
			}
			p, err := openAGMSerial(path)
			if err != nil {
				// Порт занят другим процессом (например, ещё держит прежний
				// экземпляр) — не считаем это «чужим устройством»: тихо ждём
				// следующего прохода, без бэкоффа и лога.
				if agmSerialBusy(err) {
					continue
				}
				log.Printf("agm: PZEM-017: открыть %s не удалось: %v", path, err)
				backoff[key] = time.Now().Add(agmPZEMForeignDelay)
				continue
			}
			r, err := readAGMPZEM(p, cfg.PZEM.Slave, agmPZEMProbeTimeout, true)
			if err != nil || !agmPZEMPlausible(r) {
				_ = p.Close()
				backoff[key] = time.Now().Add(agmPZEMForeignDelay)
				log.Printf("agm: PZEM-017: %s не ответил как PZEM (%v) — пауза %s",
					path, err, agmPZEMForeignDelay)
				continue
			}
			if ctx.Err() != nil {
				_ = p.Close()
				return nil, false
			}
			if errs != nil {
				errs.clear(agmPZEMErrKey)
			}
			log.Printf("agm: PZEM-017 найден на %s (slave %d), напряжение %.2f В — порт закреплён",
				path, cfg.PZEM.Slave, r.VoltageV)
			// Небольшая пауза после активной пробы: порт только что отдал
			// ответ, первый рабочий обмен без неё может уйти в таймаут.
			time.Sleep(100 * time.Millisecond)
			_ = p.ResetInputBuffer()
			cl := &agmPZEMClient{path: path, slave: cfg.PZEM.Slave, timeout: agmPZEMReadTimeout}
			cl.setPort(p)
			return cl, true
		}
		// Ни один кандидат не ответил как PZEM: фиксируем ошибку связи (однократно,
		// до успешного закрепления порта).
		if errs != nil {
			errs.report(agmPZEMErrKey, fmt.Sprintf("PZEM-017 не найден (usbip %s)", cfg.PZEM.Usbip))
		}
		if !agmSleepCtx(ctx, agmPZEMScanInterval) {
			return nil, false
		}
	}
}

// agmSleepCtx ждёт d или отмены контекста. false — контекст отменён.
func agmSleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
