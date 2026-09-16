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

package solarman

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"
)

// Client — TCP-клиент Solarman V5. Логгеры отвечают медленно и могут оставлять
// «висящее» соединение (когда по нему приходят только heartbeat-кадры). Чтобы
// каждый запрос шёл на свежем сокете и гарантированно закрывался после получения
// данных, Client открывает НОВОЕ TCP-соединение на каждый запрос (Exchange) и
// закрывает его сразу после чтения ответа (включая ошибки). Один Client рассчитан
// на последовательное чтение одним опрашивающим (инвертором).
type Client struct {
	Address    string
	DeviceSN   uint32
	Timeout    time.Duration
	IdleWindow time.Duration
	// MaxTotal — общий лимит приёма ОДНОГО ответа (от первого байта до конца).
	// Защита от «зомби»-логгера: капающий байтами (с интервалом < IdleWindow)
	// без общего лимита держал бы Exchange до 4096-байтового потолка (часы).
	// 0 — без общего лимита (legacy-поведение, только IdleWindow).
	MaxTotal time.Duration

	// серийный номер кадра — инкрементируется на каждый запрос.
	mu     sync.Mutex
	serial uint16
}

// dial открывает свежее TCP-соединение без кэширования. Уважает ctx: при
// отмене (стоп сервиса) dial прерывается, а уже открытое соединение закрывается
// наблюдателем в Exchange.
func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	d := &net.Dialer{Timeout: c.Timeout}
	conn, err := d.DialContext(ctx, "tcp", c.Address)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	return conn, nil
}

// readAll собирает все кадры ответа с соединения до «тишины» (IdleWindow) либо
// общего MaxTotal-лимита приёма. Возвращает полученные байты.
func (c *Client) readAll(conn net.Conn) []byte {
	var raw []byte
	buf := make([]byte, 1024)
	firstByte := true
	// Общий дедлайн приёма ответа: «капающий» логгер (байт с интервалом <
	// IdleWindow) не должен держать Exchange часами — после MaxTotal приём
	// прекращается, даже если байты ещё приходят.
	var deadline time.Time
	if c.MaxTotal > 0 {
		deadline = time.Now().Add(c.MaxTotal)
	}
	for {
		// Первый байт может прийти через 8-15 с (pacing логгера) — ждём Timeout.
		// Дальше — тишина IdleWindow означает конец ответа.
		idle := c.IdleWindow
		if firstByte {
			idle = c.Timeout
		}
		if !deadline.IsZero() {
			if remaining := deadline.Sub(time.Now()); remaining <= 0 {
				break // общий лимит приёма исчерпан
			} else if idle > remaining {
				idle = remaining
			}
		}
		if err := conn.SetReadDeadline(time.Now().Add(idle)); err != nil {
			break
		}
		n, _ := conn.Read(buf)
		if n > 0 {
			firstByte = false
			raw = append(raw, buf[:n]...)
			if len(raw) > 4096 {
				break
			}
			continue
		}
		// idle таймаут или ошибка соединения — конец приёма
		break
	}
	return raw
}

// nextSerial возвращает следующий порядковый номер кадра (LE u16) под mutex.
func (c *Client) nextSerial() uint16 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.serial++
	return c.serial
}

// Exchange отправляет один запрос через СВЕЖЕЕ соединение и собирает все кадры
// ответа. Соединение гарантированно закрывается после чтения ответа или любой
// ошибки. При отмене ctx (стоп сервиса) наблюдатель закрывает соединение, чтобы
// блокирующий conn.Read в readAll мгновенно завершился — иначе опрос медленного
// логгера (до ~15 c на первый байт) удерживал бы graceful-shutdown.
func (c *Client) Exchange(ctx context.Context, req []byte) ([]Frame, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	// Наблюдатель: при отмене ctx закрывает сокет, чтобы блокирующий conn.Read
	// в readAll немедленно завершился ошибкой, а не ждал read-deadline. Сокет
	// локальный для Exchange, поэтому Close из наблюдателя безопасен.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()

	if err := conn.SetDeadline(time.Now().Add(c.Timeout)); err != nil {
		return nil, fmt.Errorf("set deadline: %w", err)
	}
	if _, err := conn.Write(req); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}
	raw := c.readAll(conn)
	if len(raw) == 0 {
		return nil, fmt.Errorf("no data from %s", c.Address)
	}
	return SplitFrames(raw), nil
}

// ReadRegisters — запрос чтения startReg..startReg+regCount-1 (12-байтный
// datafield). Legacy: в проде не используется — оба типа логгеров (Deye и
// Sofar) отвечают только на 15-байтный кадр (ReadRegistersDeyeFn); оставлено
// для тестов/диагностики. Возвращает распарсенные PDU (может быть несколько)
// и кадры ответа.
func (c *Client) ReadRegisters(ctx context.Context, startReg, regCount uint16) ([]ModbusPDU, []Frame, error) {
	frames, err := c.Exchange(ctx, BuildReadFrame(c.DeviceSN, c.nextSerial(), startReg, regCount))
	if err != nil {
		return nil, nil, err
	}
	return parsePDUs(frames), frames, nil
}

// ReadRegistersDeye — запрос чтения для Deye-даталоггеров (15-байтный datafield,
// реальный SN логгера обязателен). Unit — Modbus-адрес устройства (обычно 0x01).
func (c *Client) ReadRegistersDeye(ctx context.Context, startReg, regCount uint16, unit uint32) ([]ModbusPDU, []Frame, error) {
	return c.ReadRegistersDeyeFn(ctx, startReg, regCount, unit, 0x03)
}

// ReadRegistersDeyeFn — ReadRegistersDeye с произвольной Modbus-функцией чтения
// (0x03 holding / 0x04 input). Для HW-диапазона Sofar нужен func 04.
func (c *Client) ReadRegistersDeyeFn(ctx context.Context, startReg, regCount uint16, unit uint32, fn byte) ([]ModbusPDU, []Frame, error) {
	frames, err := c.Exchange(ctx, BuildDeyeReadFrameFn(c.DeviceSN, unit, c.nextSerial(), startReg, regCount, fn))
	if err != nil {
		return nil, nil, err
	}
	return parsePDUs(frames), frames, nil
}

// parsePDUs собирает все Modbus-PDU из кадров.
func parsePDUs(frames []Frame) []ModbusPDU {
	var pdus []ModbusPDU
	for _, f := range frames {
		pdus = append(pdus, ParseModbusPDU(f.Payload)...)
	}
	return pdus
}
