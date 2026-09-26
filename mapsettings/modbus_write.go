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
	"time"
)

// Запись ячеек МАП по Modbus TCP (модуль map-settings).
//
// По документации МАП (protocol_MAP_cells_2026_07_15.doc):
//   - 0x06 — запись ОДНОГО БАЙТА: адрес побайтовый, значение в МЛАДШЕМ байте
//     слова (старший байт игнорируется);
//   - 0x10 — запись страницы (кратно словам), побайтовая адресация как у чтения;
//   - команды (ComMAP_*) передаются как запись байта по адресу 0x0000.
//
// Служебное обрамление записи (как делает mapd): команда ComMAP_EEPromWR=3 →
// запись ячеек → команда ComMAP_Call_load_EEProm=7.

// Modbus-команды МАП (запись байта по адресу 0x0000).
const (
	ComMAPOFF            = 1 // Выключить (только для МАП)
	ComMAPON             = 2 // Включить (только для МАП)
	ComMAPEEPromWR       = 3 // Разрешение записи в EEProm/RAM (ставить ПЕРЕД записью)
	ComMAPChargeOFF      = 4 // Выключить заряд (только для МАП)
	ComMAPChargeON       = 5 // Включить заряд (только для МАП)
	ComMAPReset          = 6 // Сброс контроллера
	ComMAPCallLoadEEProm = 7 // Инициализация данных из EEProm (после записи)
	ComMAPStatReset      = 8 // Сброс статистики (только для МАП)
	ComMAPDischOff       = 9 // Выключение генерации по полному разряду (только для МАП)
)

// transact отправляет Modbus-PDU (без MBAP) и возвращает PDU ответа (после unit
// id, т.е. начиная с function code). Транзакция — под тем же мьютексом, что и
// ReadRegisters, соединение переиспользуется; при обрыве — один реконнект.
func (c *Client) transact(ctx context.Context, pdu []byte) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		if err := c.dialLocked(ctx); err != nil {
			return nil, err
		}
	}
	lenField := len(pdu) + 1 // unit + pdu
	req := make([]byte, 0, 7+len(pdu))
	txn := c.nextTxn()
	req = binary.BigEndian.AppendUint16(req, txn)
	req = binary.BigEndian.AppendUint16(req, 0)
	req = binary.BigEndian.AppendUint16(req, uint16(lenField))
	req = append(req, c.Unit)
	req = append(req, pdu...)

	if err := c.conn.SetDeadline(time.Now().Add(ReadTimeout)); err != nil {
		return nil, fmt.Errorf("set deadline: %w", err)
	}
	if _, err := c.conn.Write(req); err != nil {
		c.closeConn()
		if derr := c.dialLocked(ctx); derr != nil {
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

	hdr := make([]byte, 7)
	if _, err := ReadFull(c.conn, hdr); err != nil {
		c.closeConn()
		return nil, fmt.Errorf("read header: %w", err)
	}
	if got := binary.BigEndian.Uint16(hdr[0:2]); got != txn {
		c.closeConn()
		return nil, fmt.Errorf("несовпадение transaction id: ожидался %d, получен %d", txn, got)
	}
	if binary.BigEndian.Uint16(hdr[2:4]) != 0 {
		c.closeConn()
		return nil, fmt.Errorf("не protocol=0 в MBAP")
	}
	if hdr[6] != c.Unit {
		c.closeConn()
		return nil, fmt.Errorf("несовпадение unit id: ожидался %d, получен %d", c.Unit, hdr[6])
	}
	mbLen := int(binary.BigEndian.Uint16(hdr[4:6]))
	if mbLen < 3 {
		c.closeConn()
		return nil, fmt.Errorf("некорректный MBAP length=%d (минимум 3)", mbLen)
	}
	rest := make([]byte, mbLen-1) // минус unit id
	if _, err := ReadFull(c.conn, rest); err != nil {
		c.closeConn()
		return nil, fmt.Errorf("read pdu: %w", err)
	}
	if rest[0]&0x80 != 0 {
		return nil, fmt.Errorf("modbus exception func=0x%02X code=0x%02X", rest[0], rest[1])
	}
	return rest, nil
}

// WriteCell записывает один байт value в побайтовую ячейку addr (функция 0x06).
func (c *Client) WriteCell(ctx context.Context, addr uint16, value byte) error {
	pdu := []byte{0x06, byte(addr >> 8), byte(addr), 0x00, value}
	rest, err := c.transact(ctx, pdu)
	if err != nil {
		return err
	}
	if len(rest) != 5 || rest[0] != 0x06 {
		return fmt.Errorf("неожиданный ответ на 0x06: % x", rest)
	}
	gotAddr := binary.BigEndian.Uint16(rest[1:3])
	gotVal := binary.BigEndian.Uint16(rest[3:5])
	if gotAddr != addr || gotVal != uint16(value) {
		return fmt.Errorf("эхо 0x06 не совпало: addr=0x%04X val=%d (ждали 0x%04X %d)", gotAddr, gotVal, addr, value)
	}
	return nil
}

// WriteCommand передаёт команду МАП ComMAP_* (запись байта по адресу 0x0000).
func (c *Client) WriteCommand(ctx context.Context, cmd byte) error {
	return c.WriteCell(ctx, 0x0000, cmd)
}

// WritePage записывает страницу байтов (функция 0x10) начиная с побайтовой
// ячейки start. len(data) должен быть чётным (страница кратна словам): нечётный
// хвост дополняется нулём. Возвращает число записанных слов.
func (c *Client) WritePage(ctx context.Context, start uint16, data []byte) (int, error) {
	if len(data) == 0 {
		return 0, fmt.Errorf("пустая страница")
	}
	if len(data)%2 != 0 {
		data = append(append([]byte(nil), data...), 0x00)
	}
	qty := len(data) / 2
	if qty > 120 {
		return 0, fmt.Errorf("страница %d слов слишком велика для МАП (макс 120)", qty)
	}
	pdu := make([]byte, 0, 6+len(data))
	pdu = append(pdu, 0x10, byte(start>>8), byte(start), byte(qty>>8), byte(qty), byte(len(data)))
	pdu = append(pdu, data...)
	rest, err := c.transact(ctx, pdu)
	if err != nil {
		return 0, err
	}
	if len(rest) != 5 || rest[0] != 0x10 {
		return 0, fmt.Errorf("неожиданный ответ на 0x10: % x", rest)
	}
	gotAddr := binary.BigEndian.Uint16(rest[1:3])
	gotQty := binary.BigEndian.Uint16(rest[3:5])
	if gotAddr != start || int(gotQty) != qty {
		return 0, fmt.Errorf("эхо 0x10 не совпало: addr=0x%04X qty=%d (ждали 0x%04X %d)", gotAddr, gotQty, start, qty)
	}
	return qty, nil
}
