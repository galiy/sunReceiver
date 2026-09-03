package solarman

import (
	"fmt"
	"net"
	"sync"
	"time"
)

// Client — TCP-клиент Solarman V5. Держит одно TCP-соединение и переиспользует его
// между опросами, чтобы не выполнять TCP-handshake каждые 10 секунд (логгеры отвечают
// медленно — лишний переподъём сокета не нужен). Соединение открывается лениво при
// первом запросе и автоматически пересоздаётся, если разорвано. Один Client рассчитан
// на последовательное чтение одним опрашивающим (инвертором).
type Client struct {
	Address    string
	DeviceSN   uint32
	Timeout    time.Duration
	IdleWindow time.Duration

	conn net.Conn
	mu   sync.Mutex
}

// open возвращает подключённое TCP-соединение, переиспользуя существующее.
func (c *Client) open() (net.Conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		return c.conn, nil
	}
	conn, err := net.DialTimeout("tcp", c.Address, c.Timeout)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	c.conn = conn
	return conn, nil
}

// Close закрывает текущее соединение (если открыто).
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	return err
}

// markBroken закрывает соединение и очищает указатель, если это текущее — следующий
// open() выполнит новый dial.
func (c *Client) markBroken(conn net.Conn) {
	if conn == nil {
		return
	}
	conn.Close()
	c.mu.Lock()
	if c.conn == conn {
		c.conn = nil
	}
	c.mu.Unlock()
}

// readAll собирает все кадры ответа с соединения до «тишины» (IdleWindow) либо общего
// Timeout (для первого байта). Возвращает true, если что-то получено.
func (c *Client) readAll(conn net.Conn) ([]byte, bool) {
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
	return raw, len(raw) > 0
}

// Exchange отправляет один запрос через переиспользуемое соединение и собирает все
// кадры ответа. При разрыве соединения производится новый dial. Если ответа нет
// вовсе, соединение помечается закрытым, чтобы следующий вызов переподключился.
func (c *Client) Exchange(req []byte) ([]Frame, error) {
	conn, err := c.open()
	if err != nil {
		return nil, err
	}
	if err := conn.SetDeadline(time.Now().Add(c.Timeout)); err != nil {
		c.markBroken(conn)
		return nil, fmt.Errorf("set deadline: %w", err)
	}
	if _, err := conn.Write(req); err != nil {
		c.markBroken(conn)
		return nil, fmt.Errorf("write: %w", err)
	}
	raw, ok := c.readAll(conn)
	if !ok {
		// Пусто — возможно, соединение оборвано (логгер закрыл простаивающий сокет).
		c.markBroken(conn)
		return nil, fmt.Errorf("no data from %s", c.Address)
	}
	return SplitFrames(raw), nil
}

// ReadRegisters — запрос чтения startReg..startReg+regCount-1.
// Возвращает распарсенные PDU (может быть несколько) и кадры ответа.
func (c *Client) ReadRegisters(startReg, regCount uint16) ([]ModbusPDU, []Frame, error) {
	frames, err := c.Exchange(BuildReadFrame(c.DeviceSN, startReg, regCount))
	if err != nil {
		return nil, nil, err
	}
	return parsePDUs(frames), frames, nil
}

// ReadRegistersDeye — запрос чтения для Deye-даталоггеров (15-байтный datafield,
// реальный SN логгера обязателен). Unit — Modbus-адрес устройства (обычно 0x01).
// Несколько вызовов на один инвертор делят одно TCP-соединение, так как ответы
// разделяются паузой в тишине (pacing) между диапазонами.
func (c *Client) ReadRegistersDeye(startReg, regCount uint16, unit uint32) ([]ModbusPDU, []Frame, error) {
	frames, err := c.Exchange(BuildDeyeReadFrame(c.DeviceSN, unit, startReg, regCount))
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
