package solarman

import (
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

	// серийный номер кадра — инкрементируется на каждый запрос.
	mu     sync.Mutex
	serial uint16
}

// dial открывает свежее TCP-соединение без кэширования.
func (c *Client) dial() (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", c.Address, c.Timeout)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	return conn, nil
}

// readAll собирает все кадры ответа с соединения до «тишины» (IdleWindow) либо общего
// Timeout (для первого байта). Возвращает полученные байты.
func (c *Client) readAll(conn net.Conn) []byte {
	var raw []byte
	buf := make([]byte, 1024)
	firstByte := true
	for {
		// Первый байт может прийти через 8-15 с (pacing логгера) — ждём Timeout.
		// Дальше — тишина IdleWindow означает конец ответа.
		idle := c.IdleWindow
		if firstByte {
			idle = c.Timeout
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
// ошибки.
func (c *Client) Exchange(req []byte) ([]Frame, error) {
	conn, err := c.dial()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

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

// ReadRegisters — запрос чтения startReg..startReg+regCount-1.
// Возвращает распарсенные PDU (может быть несколько) и кадры ответа.
func (c *Client) ReadRegisters(startReg, regCount uint16) ([]ModbusPDU, []Frame, error) {
	frames, err := c.Exchange(BuildReadFrame(c.DeviceSN, c.nextSerial(), startReg, regCount))
	if err != nil {
		return nil, nil, err
	}
	return parsePDUs(frames), frames, nil
}

// ReadRegistersDeye — запрос чтения для Deye-даталоггеров (15-байтный datafield,
// реальный SN логгера обязателен). Unit — Modbus-адрес устройства (обычно 0x01).
func (c *Client) ReadRegistersDeye(startReg, regCount uint16, unit uint32) ([]ModbusPDU, []Frame, error) {
	return c.ReadRegistersDeyeFn(startReg, regCount, unit, 0x03)
}

// ReadRegistersDeyeFn — ReadRegistersDeye с произвольной Modbus-функцией чтения
// (0x03 holding / 0x04 input). Для HW-диапазона Sofar нужен func 04.
func (c *Client) ReadRegistersDeyeFn(startReg, regCount uint16, unit uint32, fn byte) ([]ModbusPDU, []Frame, error) {
	frames, err := c.Exchange(BuildDeyeReadFrameFn(c.DeviceSN, unit, c.nextSerial(), startReg, regCount, fn))
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
