// Package modbusmap — минимальный Modbus-TCP клиент для МАП Титанатор («КЭС»).
//
// МАП хранит ячейки побайтно и отвечает на функ 03 (чтение регистров) словами:
// слово = 2 байт-ячейки, значение ячейки лежит в СТАРШЕМ байте слова, младший
// байт — значение следующей ячейки. Т.е. чтение N регистров с адреса A возвращает
// байт-ячейки A..A+2N-1 (регистры идут подряд, адрес шагает на 2 за слово).
// Адреса ячеек и/или их масштабы описаны в «protocol_MAP_cells_2023_06_27.doc».
//
// Гейт (192.168.13.74:502) отвечает медленно: первый ответ может прийти через
// десятки секунд, после «прогрева» — быстро. Соединение переиспользуется между
// опросами, чтобы не платить за повторный прогрев.
package modbusmap

import (
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"time"
)

var (
	DefaultPort    = "502"
	ReadTimeout    = 3 * time.Second // таймаут чтения ответа гейта (по mapread.py работает при таймауте 3с)
	ConnectTimeout = 3 * time.Second
)

// Client — переиспользуемое TCP-соединение к МАП-гейту (Modbus TCP).
// Рассчитан на последовательное использование одним опрашивающим.
type Client struct {
	Address string // host:port
	Unit    byte   // Modbus-адрес устройства (обычно 0x01)

	mu   sync.Mutex
	conn net.Conn
	txn  uint16
}

// Dial устанавливает (или переиспользует) TCP-соединение к гейту.
// Если существующее соединение мертво — переподнимает.
func (c *Client) Dial() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		return nil
	}
	conn, err := net.DialTimeout("tcp", c.Address, ConnectTimeout)
	if err != nil {
		return fmt.Errorf("dial %s: %w", c.Address, err)
	}
	c.conn = conn
	return nil
}

// ReadRegisters читает registerCount регистров (слов) с адреса start, что
// соответствует count*2 байт-ячейкам начиная с start. Возвращает байт-ячейки:
// cell[start+i] = raw[2*i], cell[start+1+i] = raw[2*i+1].
func (c *Client) ReadRegisters(start uint16, count uint16) ([]byte, error) {
	if count > 120 {
		return nil, fmt.Errorf("count %d слишком велик для МАП (макс 120)", count)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		if err := c.dialLocked(); err != nil {
			return nil, err
		}
	}
	// MBAP + PDU (func 03, start, count)
	lenField := 6
	req := make([]byte, 0, 12)
	req = binary.BigEndian.AppendUint16(req, c.nextTxn()) // transaction id
	req = binary.BigEndian.AppendUint16(req, 0)           // protocol
	req = binary.BigEndian.AppendUint16(req, uint16(lenField)) // length
	req = append(req, c.Unit)
	req = append(req, 0x03)
	req = binary.BigEndian.AppendUint16(req, start)
	req = binary.BigEndian.AppendUint16(req, count)

	if err := c.conn.SetDeadline(time.Now().Add(ReadTimeout)); err != nil {
		return nil, fmt.Errorf("set deadline: %w", err)
	}
	if _, err := c.conn.Write(req); err != nil {
		// соединение могло умереть — переподнимаем один раз и повторяем
		c.closeConn()
		if derr := c.dialLocked(); derr != nil {
			return nil, fmt.Errorf("reconnect: %w", derr)
		}
		if err := c.conn.SetDeadline(time.Now().Add(ReadTimeout)); err != nil {
			return nil, fmt.Errorf("set deadline: %w", err)
		}
		if _, err := c.conn.Write(req); err != nil {
			c.closeConn()
			return nil, fmt.Errorf("write: %w", err)
		}
	}

	// Читаем MBAP (7 байт) затем остальное.
	hdr := make([]byte, 7)
	if _, err := readFull(c.conn, hdr); err != nil {
		c.closeConn()
		return nil, fmt.Errorf("read header: %w", err)
	}
	if binary.BigEndian.Uint16(hdr[2:4]) != 0 {
		c.closeConn()
		return nil, fmt.Errorf("не protocol=0 в MBAP")
	}
	mbLen := int(binary.BigEndian.Uint16(hdr[4:6]))
	if mbLen < 3 || mbLen > 2+1+1+2*int(count) {
		c.closeConn()
		return nil, fmt.Errorf("некорректный MBAP length=%d", mbLen)
	}
	rest := make([]byte, mbLen-1) // минус unit id (уже в hdr[6])
	if _, err := readFull(c.conn, rest); err != nil {
		c.closeConn()
		return nil, fmt.Errorf("read pdu: %w", err)
	}
	funcID := rest[0]
	if funcID&0x80 != 0 {
		return nil, fmt.Errorf("modbus exception func=0x%02X code=0x%02X", funcID, rest[1])
	}
	bc := int(rest[1])
	if bc != 2*int(count) {
		return nil, fmt.Errorf("bytecount=%d, ждали %d", bc, 2*int(count))
	}
	data := rest[2 : 2+bc]
	out := make([]byte, bc)
	copy(out, data)
	return out, nil
}

func (c *Client) nextTxn() uint16 { c.txn++; return c.txn }

func (c *Client) dialLocked() error {
	conn, err := net.DialTimeout("tcp", c.Address, ConnectTimeout)
	if err != nil {
		return fmt.Errorf("dial %s: %w", c.Address, err)
	}
	c.conn = conn
	return nil
}

func (c *Client) closeConn() {
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
}

func readFull(conn net.Conn, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := conn.Read(buf[total:])
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}