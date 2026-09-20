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
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/galiy/sunReceiver/modbusmap"
)

// meterClient — переиспользуемое TCP-соединение к электросчётчику DDS238
// (Modbus TCP, стандартные holding registers). В отличие от modbusmap для МАП
// (гейт хранит ячейки побайтно в старшем байте слова), DDS238 отдаёт обычные
// uint16-слова (big-endian), поэтому читаем регистры как uint16.
// Соединение переиспользуется между 1-секундными опросами.
type meterClient struct {
	Address string // host:port
	Unit    byte   // Modbus-адрес устройства (обычно 1)

	mu   sync.Mutex
	conn net.Conn
	txn  uint16
}

// newMeterClient создаёт клиент к хост:port с Modbus-адресом unit.
func newMeterClient(addr string, unit byte) *meterClient {
	return &meterClient{Address: addr, Unit: unit}
}

// dial устанавливает (или переиспользует) TCP-соединение; при необходимости переподнимает.
func (c *meterClient) dial(ctx context.Context) error {
	if c.conn != nil {
		return nil
	}
	d := &net.Dialer{Timeout: 3 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", c.Address)
	if err != nil {
		return fmt.Errorf("dial %s: %w", c.Address, err)
	}
	c.conn = conn
	return nil
}

func (c *meterClient) closeConn() {
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
}

func (c *meterClient) nextTxn() uint16 { c.txn++; return c.txn }

// ReadHoldingRegisters читает count держащих регистров с адреса start (функция 03)
// и возвращает их как uint16 (big-endian). Эти же регистры возвращает
// read_holding_registers(0, N) в dds238read.py.
//
// Уважает ctx только на этапе dial (DialContext): при отмене (стоп сервиса)
// медленное подключение прерывается. Чтение ограничено 3-сек read-deadline.
func (c *meterClient) ReadHoldingRegisters(ctx context.Context, start, count uint16) ([]uint16, error) {
	if count == 0 {
		return nil, fmt.Errorf("meter: пустой count")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		if err := c.dial(ctx); err != nil {
			return nil, err
		}
	}
	// MBAP + PDU (func 03, start, count)
	req := make([]byte, 0, 12)
	req = binary.BigEndian.AppendUint16(req, c.nextTxn())
	req = binary.BigEndian.AppendUint16(req, 0) // protocol
	req = binary.BigEndian.AppendUint16(req, 6) // length
	req = append(req, c.Unit, 0x03)
	req = binary.BigEndian.AppendUint16(req, start)
	req = binary.BigEndian.AppendUint16(req, count)

	if err := c.conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		return nil, fmt.Errorf("meter set deadline: %w", err)
	}
	if _, err := c.conn.Write(req); err != nil {
		// соединение могло умереть — переподнимаем один раз и повторяем
		c.closeConn()
		if derr := c.dial(ctx); derr != nil {
			return nil, derr
		}
		if err := c.conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
			return nil, fmt.Errorf("meter set deadline: %w", err)
		}
		if _, err := c.conn.Write(req); err != nil {
			c.closeConn()
			return nil, fmt.Errorf("meter write: %w", err)
		}
	}

	hdr := make([]byte, 7)
	if _, err := modbusmap.ReadFull(c.conn, hdr); err != nil {
		c.closeConn()
		return nil, fmt.Errorf("meter read header: %w", err)
	}
	// Сверяем transaction id и unit id (как в modbusmap): поздний ответ СТАРОГО
	// запроса на переиспользуемом сокете не должен приниматься за текущий — при
	// расхождении закрываем соединение, следующий опрос идёт по чистому сокету.
	txn := binary.BigEndian.Uint16(req[:2])
	if got := binary.BigEndian.Uint16(hdr[0:2]); got != txn {
		c.closeConn()
		return nil, fmt.Errorf("meter: несовпадение transaction id: ожидался %d, получен %d", txn, got)
	}
	if binary.BigEndian.Uint16(hdr[2:4]) != 0 {
		c.closeConn()
		return nil, fmt.Errorf("meter: protocol != 0 в MBAP")
	}
	if hdr[6] != c.Unit {
		c.closeConn()
		return nil, fmt.Errorf("meter: несовпадение unit id: ожидался %d, получен %d", c.Unit, hdr[6])
	}
	mbLen := int(binary.BigEndian.Uint16(hdr[4:6]))
	// Полный ответ = unit(1) + func(1) + bytecount(1) + data(2*count) → 3+2*count.
	// Нижняя граница +2*count защищает слайс rest[2:2+bc] от выхода за пределы
	// (усечённый кадр с завышенным MBAP-length иначе дал бы панику в пулере).
	if mbLen < 3+2*int(count) || mbLen > 3+2*int(count) {
		c.closeConn()
		return nil, fmt.Errorf("meter: некорректный MBAP length=%d, ждали %d", mbLen, 3+2*int(count))
	}
	rest := make([]byte, mbLen-1) // минус unit id (уже в hdr[6])
	if _, err := modbusmap.ReadFull(c.conn, rest); err != nil {
		c.closeConn()
		return nil, fmt.Errorf("meter read pdu: %w", err)
	}
	if rest[0]&0x80 != 0 {
		return nil, fmt.Errorf("meter: modbus exception func=0x%02X code=0x%02X", rest[0], rest[1])
	}
	bc := int(rest[1])
	if bc != 2*int(count) {
		c.closeConn()
		return nil, fmt.Errorf("meter: bytecount=%d, ждали %d", bc, 2*int(count))
	}
	data := rest[2 : 2+bc]
	regs := make([]uint16, count)
	for i := 0; i < int(count); i++ {
		regs[i] = binary.BigEndian.Uint16(data[2*i:])
	}
	return regs, nil
}
