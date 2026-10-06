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
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"go.bug.st/serial"
)

// RS485-транспорт BMS EnBMS (Enjie EMU110x). В отличие от BLE (бинарный кадр с
// CRC-16/CCITT) на RS485 используется ASCII-hex диалект PACE (YD/T 1363):
//
//	Запрос:  ~ VER(2) ADR(2) 46 CID2(2) LEN(4) INFO(hex) CHK(4) CR
//	Ответ :  ~ VER(2) ADR(2) 46 RTN(2) LEN(4) INFO(hex) CHK(4) CR
//
// Телеметрию отдаёт CID2 0x42 (TeleMeter) с той же раскладкой, что и BLE-блок
// Battery (0x61), поэтому парсер общий (см. parseEnBmsBatteryPayload). Транспорт —
// либо прозрачный IP-RS485-шлюз по TCP, либо локальный COM-порт. Живой реверс —
// /home/sasha/src/energybms/protocol-485.md.

// enbmsLink — общий интерфейс транспорта EnBMS (BLE или RS485). Пулер работает
// только через него и не зависит от конкретного способа доставки.
type enbmsLink interface {
	readEnBmsBattery() ([]byte, error)
	readEnBmsBasicInfo() ([]byte, error)
	// readEnBmsState читает блок состояния/защит (RS485 CID2 0x44 TeleState).
	// По BLE недоступен (возвращает errEnBmsStateUnsupported): там состояния
	// приходят в сигнальном хвосте Battery.
	readEnBmsState() ([]byte, error)
	Close() error
}

const (
	// enBmsPaceVER — фиксированная версия протокола PACE.
	enBmsPaceVER = 0x20
	// enBmsRS485CID2Meter — TeleMeter (телеметрия; аналог BLE Battery 0x61).
	enBmsRS485CID2Meter = 0x42
	// enBmsRS485CID2Manufacture — Manufacture (инфо/модель).
	enBmsRS485CID2Manufacture = 0x51
	// enBmsRS485CID2State — TeleState (состояния/защиты: ячейки, температуры,
	// Ext_Bit, режим). Аналог части сигнального хвоста BLE Battery.
	enBmsRS485CID2State = 0x44

	// enBmsRS485ReadTimeout — ожидание ответа на кадр PACE. Шина проводная и
	// отвечает быстро; короткий таймаут не задерживает 1-секундный цикл.
	enBmsRS485ReadTimeout = 2 * time.Second
	// enBmsRS485DialTimeout — лимит установки TCP-соединения с шлюзом.
	enBmsRS485DialTimeout = 5 * time.Second
)

// enbmsByteStream — минимальный байтовый поток с настраиваемым таймаутом чтения
// (TCP-соединение или COM-порт).
type enbmsByteStream interface {
	SetReadTimeout(d time.Duration) error
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	Close() error
}

// tcpByteStream адаптирует net.Conn к enbmsByteStream (дедлайн чтения).
type tcpByteStream struct{ c net.Conn }

func (s tcpByteStream) SetReadTimeout(d time.Duration) error {
	return s.c.SetReadDeadline(time.Now().Add(d))
}
func (s tcpByteStream) Read(p []byte) (int, error)  { return s.c.Read(p) }
func (s tcpByteStream) Write(p []byte) (int, error) { return s.c.Write(p) }
func (s tcpByteStream) Close() error                { return s.c.Close() }

// serialByteStream адаптирует serial.Port к enbmsByteStream.
type serialByteStream struct{ p serial.Port }

func (s serialByteStream) SetReadTimeout(d time.Duration) error {
	return s.p.SetReadTimeout(d)
}
func (s serialByteStream) Read(p []byte) (int, error)  { return s.p.Read(p) }
func (s serialByteStream) Write(p []byte) (int, error) { return s.p.Write(p) }
func (s serialByteStream) Close() error                { return s.p.Close() }

// enbmsRS485Conn — установленное соединение с BMS по RS485 (TCP-шлюз или COM).
// Соединение постоянное: не рвётся между опросами, переустанавливается при обрыве.
type enbmsRS485Conn struct {
	stream    enbmsByteStream
	adr       byte   // поле ADR кадра PACE (= unit из конфига, как есть)
	portType  string // "rs485" (без INFO) | "rm485" (INFO=[00] для 0x42)
	ctx       context.Context
	mu        sync.Mutex    // сериализация запрос/ответ на одном канале
	closeOnce sync.Once     // идемпотентное закрытие (watcher ctx + обычный Close)
	stop      chan struct{} // закрывается при Close: гасит watcher ctx
}

// openEnBmsRS485 устанавливает соединение с BMS по RS485: TCP с прозрачным
// шлюзом или локальный COM-порт (настройки — enBmsRS485Config). Unit из конфига
// подставляется в кадр как ADR как есть (для этой BMS ADR=0).
func openEnBmsRS485(cfg *enBmsRS485Config, ctx context.Context) (*enbmsRS485Conn, error) {
	if cfg == nil {
		return nil, errors.New("RS485-конфиг не задан")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	adr := cfg.Unit

	var stream enbmsByteStream
	switch cfg.Transport {
	case "tcp":
		c, err := net.DialTimeout("tcp", cfg.Address, enBmsRS485DialTimeout)
		if err != nil {
			return nil, fmt.Errorf("подключение к шлюзу %s: %w", cfg.Address, err)
		}
		stream = tcpByteStream{c: c}
	case "com":
		mode := &serial.Mode{
			BaudRate: cfg.Baud,
			DataBits: cfg.DataBits,
			Parity:   serialParity(cfg.Parity),
			StopBits: serialStopBits(cfg.StopBits),
		}
		p, err := serial.Open(cfg.Port, mode)
		if err != nil {
			return nil, fmt.Errorf("открытие COM-порта %s: %w", cfg.Port, err)
		}
		if err := p.SetReadTimeout(enBmsRS485ReadTimeout); err != nil {
			_ = p.Close()
			return nil, fmt.Errorf("настройка таймаута %s: %w", cfg.Port, err)
		}
		stream = serialByteStream{p: p}
	default:
		return nil, fmt.Errorf("неизвестный транспорт RS485 %q", cfg.Transport)
	}
	c := &enbmsRS485Conn{
		stream: stream, adr: byte(adr), portType: cfg.PortType, ctx: ctx, stop: make(chan struct{}),
	}
	// Немедленное закрытие канала при отмене контекста (SIGTERM): разблокирует
	// in-flight чтение, чтобы graceful-shutdown не ждал таймаут.
	go func() {
		select {
		case <-ctx.Done():
			_ = c.Close()
		case <-c.stop:
		}
	}()
	return c, nil
}

// serialParity переводит строку конфига в режим чётности serial.
func serialParity(p string) serial.Parity {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "even":
		return serial.EvenParity
	case "odd":
		return serial.OddParity
	default:
		return serial.NoParity
	}
}

// serialStopBits переводит число стоп-битов в режим serial.
func serialStopBits(n int) serial.StopBits {
	if n == 2 {
		return serial.TwoStopBits
	}
	return serial.OneStopBit
}

// Close закрывает соединение (идемпотентно; также гасит watcher контекста).
func (c *enbmsRS485Conn) Close() error {
	if c == nil {
		return nil
	}
	var err error
	c.closeOnce.Do(func() {
		if c.stop != nil {
			close(c.stop)
		}
		if c.stream != nil {
			err = c.stream.Close()
		}
	})
	return err
}

// readEnBmsBattery читает телеметрию (CID2 0x42). На верхнем порту (rs485) INFO не нужен;
// на RM485 для 0x42 требуется INFO=[00].
func (c *enbmsRS485Conn) readEnBmsBattery() ([]byte, error) {
	var info []byte
	if c.portType == "rm485" {
		info = []byte{0x00}
	}
	return c.request(enBmsRS485CID2Meter, info)
}

// readEnBmsBasicInfo читает Manufacture (CID2 0x51) для модели/протокола.
func (c *enbmsRS485Conn) readEnBmsBasicInfo() ([]byte, error) {
	return c.request(enBmsRS485CID2Manufacture, nil)
}

// readEnBmsState читает состояния/защиты (CID2 0x44 TeleState). На верхнем порту
// (rs485) INFO не нужен; на RM485 для 0x44 требуется INFO=[00] (как для 0x42).
func (c *enbmsRS485Conn) readEnBmsState() ([]byte, error) {
	var info []byte
	if c.portType == "rm485" {
		info = []byte{0x00}
	}
	return c.request(enBmsRS485CID2State, info)
}

// request отправляет PACE-кадр и возвращает INFO ответа. Прерывается по
// контексту (errEnBmsClosed) и по таймауту (errEnBmsReadTimeout).
func (c *enbmsRS485Conn) request(cid2 byte, info []byte) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ctx != nil {
		select {
		case <-c.ctx.Done():
			return nil, errEnBmsClosed
		default:
		}
	}
	frame := buildEnBmsPaceFrame(c.adr, cid2, info)
	if err := c.stream.SetReadTimeout(enBmsRS485ReadTimeout); err != nil {
		return nil, fmt.Errorf("таймаут чтения: %w", err)
	}
	if _, err := c.stream.Write(frame); err != nil {
		return nil, fmt.Errorf("запись запроса 0x%02x: %w", cid2, err)
	}
	deadline := time.Now().Add(enBmsRS485ReadTimeout)
	buf := make([]byte, 0, 256)
	tmp := make([]byte, 256)
	for {
		rem := time.Until(deadline)
		if rem <= 0 {
			return nil, errEnBmsReadTimeout
		}
		if err := c.stream.SetReadTimeout(rem); err != nil {
			return nil, fmt.Errorf("таймаут чтения: %w", err)
		}
		n, err := c.stream.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if i := bytes.IndexByte(buf, 0x0d); i >= 0 {
				rtn, payload, perr := parseEnBmsPaceResponse(buf[:i+1])
				if perr != nil {
					return nil, perr
				}
				if rtn != 0 {
					return nil, fmt.Errorf("ответ BMS: RTN=0x%02X", rtn)
				}
				return payload, nil
			}
		}
		if err != nil {
			if isNetTimeout(err) {
				return nil, errEnBmsReadTimeout
			}
			return nil, err
		}
	}
}

// isNetTimeout — true, если ошибка чтения — таймаут сетевого соединения.
// (COM-порт на таймауте возвращает (0, nil), поэтому отдельно проверяется deadline.)
func isNetTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// buildEnBmsPaceFrame собирает ASCII-hex кадр PACE:
//
//	~ VER ADR 46 CID2 LEN(4) INFO(hex) CHK(4) CR
//
// LEN = LCHKSUM<<12 | len(INFO), где LCHKSUM — 4-битное дополнение суммы трёх
// нибблов LENID; CHK — 16-битное дополнение суммы ASCII-кодов символов от VER до
// конца INFO. Реализация сверена с живыми кадрами и tools/rs485_pace.py.
func buildEnBmsPaceFrame(adr, cid2 byte, info []byte) []byte {
	body := fmt.Sprintf("%02X%02X%02X%02X%04X",
		enBmsPaceVER, adr, enBmsCID1, cid2, paceLenField(len(info)))
	body += strings.ToUpper(hex.EncodeToString(info))
	chk := paceChecksum(body)
	return []byte(fmt.Sprintf("~%s%04X\r", body, chk))
}

// paceLenField формирует 16-битное поле LEN запроса PACE.
func paceLenField(n int) uint16 {
	nib := ((n >> 8) & 0xF) + ((n >> 4) & 0xF) + (n & 0xF)
	lchk := (^((nib % 16) & 0xF) + 1) & 0xF
	return uint16(lchk)<<12 | uint16(n&0x0FFF)
}

// paceChecksum — 16-битное дополнение суммы ASCII-кодов строки.
func paceChecksum(s string) uint16 {
	sum := 0
	for i := 0; i < len(s); i++ {
		sum += int(s[i])
	}
	return uint16(((^sum) & 0xFFFF) + 1)
}

// parseEnBmsPaceResponse разбирает полный кадр ответа PACE (включая ведущий '~'
// и завершающий CR): проверяет CHK, извлекает RTN и INFO.
func parseEnBmsPaceResponse(raw []byte) (rtn byte, info []byte, err error) {
	if len(raw) < 2 || raw[0] != 0x7e || raw[len(raw)-1] != 0x0d {
		return 0, nil, fmt.Errorf("неверные границы кадра PACE (len=%d)", len(raw))
	}
	t := string(raw[1 : len(raw)-1])
	// Минимум: VER(2)+ADR(2)+CID1(2)+RTN(2)+LEN(4)+CHK(4) = 16 hex-символов.
	if len(t) < 16 {
		return 0, nil, fmt.Errorf("кадр PACE слишком короткий: %d символов", len(t))
	}
	if got, want := fmt.Sprintf("%04X", paceChecksum(t[:len(t)-4])), t[len(t)-4:]; got != want {
		return 0, nil, fmt.Errorf("CHK кадра PACE не совпал: got %s want %s", want, got)
	}
	rtnB, err := parseHexByte(t[6:8])
	if err != nil {
		return 0, nil, fmt.Errorf("RTN: %w", err)
	}
	lenField, err := parseHexU16(t[8:12])
	if err != nil {
		return 0, nil, fmt.Errorf("LEN: %w", err)
	}
	infoHex := t[12 : len(t)-4]
	lenid := int(lenField & 0x0FFF) // длина INFO в hex-символах
	if lenid != len(infoHex) {
		return 0, nil, fmt.Errorf("LENID=%d не совпал с длиной INFO=%d", lenid, len(infoHex))
	}
	payload, err := hex.DecodeString(infoHex)
	if err != nil {
		return 0, nil, fmt.Errorf("INFO не hex: %w", err)
	}
	return rtnB, payload, nil
}

// parseHexByte читает один байт из двух hex-символов.
func parseHexByte(s string) (byte, error) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 1 {
		return 0, fmt.Errorf("неверный hex %q", s)
	}
	return b[0], nil
}

// parseHexU16 читает u16 из четырёх hex-символов.
func parseHexU16(s string) (uint16, error) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 2 {
		return 0, fmt.Errorf("неверный hex %q", s)
	}
	return uint16(b[0])<<8 | uint16(b[1]), nil
}
