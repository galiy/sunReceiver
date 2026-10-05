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
	"github.com/galiy/sunReceiver/solarman"
)

// meterClient — переиспользуемое TCP-соединение к электросчётчику DDS238
// (стандартные holding registers). В отличие от modbusmap для МАП (гейт хранит
// ячейки побайтно в старшем байте слова), DDS238 отдаёт обычные uint16-слова
// (big-endian), поэтому читаем регистры как uint16.
// Соединение переиспользуется между 1-секундными опросами.
//
// Поддерживаются два транспорта (поле RTU):
//   - Modbus TCP (RTU=false) — MBAP-заголовок + PDU, без CRC (прямой опрос);
//   - Modbus RTU поверх TCP (RTU=true) — сырой серийный кадр с CRC16, без MBAP
//     (прозрачный шлюз, напр. USR-DR164 в режиме Modbus OFF).
type meterClient struct {
	Address string // host:port
	Unit    byte   // Modbus-адрес устройства (обычно 1)
	RTU     bool   // true — Modbus RTU поверх TCP (прозрачный шлюз)
	// Func — код функции чтения (0x03 holding / 0x04 input). 0 = 0x03 по
	// умолчанию (DDS238). Счётчик DTS017M документирован под 0x04.
	Func byte

	mu   sync.Mutex
	conn net.Conn
	txn  uint16
}

// fn возвращает код функции чтения: Func, если задан, иначе 0x03.
func (c *meterClient) fn() byte {
	if c.Func == 0 {
		return 0x03
	}
	return c.Func
}

// newMeterClient создаёт клиент к хост:port с Modbus-адресом unit.
// rtu=true — опрос по Modbus RTU поверх TCP (прозрачный шлюз).
func newMeterClient(addr string, unit byte, rtu bool) *meterClient {
	return &meterClient{Address: addr, Unit: unit, RTU: rtu}
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

// writeReqLocked отправляет готовый кадр по соединению. При ошибке записи
// переподнимает соединение один раз и повторяет отправку.
func (c *meterClient) writeReqLocked(ctx context.Context, req []byte) error {
	if err := c.conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		return fmt.Errorf("meter set deadline: %w", err)
	}
	if _, err := c.conn.Write(req); err != nil {
		// соединение могло умереть — переподнимаем один раз и повторяем
		c.closeConn()
		if derr := c.dial(ctx); derr != nil {
			return derr
		}
		if err := c.conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
			return fmt.Errorf("meter set deadline: %w", err)
		}
		if _, err := c.conn.Write(req); err != nil {
			c.closeConn()
			return fmt.Errorf("meter write: %w", err)
		}
	}
	return nil
}

// drainLocked коротким read-deadline вычитывает и отбрасывает возможные «хвосты»
// предыдущих ответов в сокете. Для RTU это важно: у кадров нет transaction id,
// и задержавшийся ответ мог бы быть принят за ответ на текущий запрос.
func (c *meterClient) drainLocked() {
	_ = c.conn.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
	buf := make([]byte, 256)
	for {
		if _, err := c.conn.Read(buf); err != nil {
			break
		}
	}
	_ = c.conn.SetReadDeadline(time.Time{})
}

// ReadHoldingRegisters читает count держащих регистров с адреса start (функция 03)
// и возвращает их как uint16 (big-endian). Эти же регистры возвращает
// read_holding_registers(0, N) в dds238read.py. Транспорт выбирается полем RTU
// (Modbus TCP / Modbus RTU поверх TCP).
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
	if c.RTU {
		return c.readRTULocked(ctx, start, count)
	}
	return c.readTCPLocked(ctx, start, count)
}

// WriteMultipleRegisters записывает values (uint16, big-endian) в holding-регистры,
// начиная с start, функцией 0x10. Транспорт — как у чтения (TCP/RTU). Используется
// для коррекции времени счётчика DTS017M (регистр 0x0210).
func (c *meterClient) WriteMultipleRegisters(ctx context.Context, start uint16, values []uint16) error {
	if len(values) == 0 {
		return fmt.Errorf("meter: пустой блок записи")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		if err := c.dial(ctx); err != nil {
			return err
		}
	}
	if c.RTU {
		return c.writeRTULocked(ctx, start, values)
	}
	return c.writeTCPLocked(ctx, start, values)
}

// writeTCPLocked выполняет запись Modbus TCP (MBAP + PDU, функция 0x10).
func (c *meterClient) writeTCPLocked(ctx context.Context, start uint16, values []uint16) error {
	count := uint16(len(values))
	dataLen := 2 * len(values)
	// MBAP length = unit(1) + func(1) + start(2) + count(2) + bytecount(1) + data.
	respLenField := 7 + dataLen
	req := make([]byte, 0, 12+dataLen)
	req = binary.BigEndian.AppendUint16(req, c.nextTxn())
	req = binary.BigEndian.AppendUint16(req, 0) // protocol
	req = binary.BigEndian.AppendUint16(req, uint16(respLenField))
	req = append(req, c.Unit, 0x10)
	req = binary.BigEndian.AppendUint16(req, start)
	req = binary.BigEndian.AppendUint16(req, count)
	req = append(req, byte(dataLen))
	for _, v := range values {
		req = binary.BigEndian.AppendUint16(req, v)
	}
	if err := c.writeReqLocked(ctx, req); err != nil {
		return err
	}

	txn := binary.BigEndian.Uint16(req[:2])
	hdr := make([]byte, 7)
	if _, err := modbusmap.ReadFull(c.conn, hdr); err != nil {
		c.closeConn()
		return fmt.Errorf("meter write read header: %w", err)
	}
	if got := binary.BigEndian.Uint16(hdr[0:2]); got != txn {
		c.closeConn()
		return fmt.Errorf("meter write: несовпадение transaction id: ожидался %d, получен %d", txn, got)
	}
	if binary.BigEndian.Uint16(hdr[2:4]) != 0 {
		c.closeConn()
		return fmt.Errorf("meter write: protocol != 0 в MBAP")
	}
	if hdr[6] != c.Unit {
		c.closeConn()
		return fmt.Errorf("meter write: несовпадение unit id: ожидался %d, получен %d", c.Unit, hdr[6])
	}
	mbLen := int(binary.BigEndian.Uint16(hdr[4:6]))
	if mbLen < 3 {
		c.closeConn()
		return fmt.Errorf("meter write: некорректный MBAP length=%d (минимум 3)", mbLen)
	}
	rest := make([]byte, mbLen-1) // минус unit id
	if _, err := modbusmap.ReadFull(c.conn, rest); err != nil {
		c.closeConn()
		return fmt.Errorf("meter write read pdu: %w", err)
	}
	if rest[0]&0x80 != 0 {
		return fmt.Errorf("meter write: modbus exception func=0x%02X code=0x%02X", rest[0], rest[1])
	}
	// Ответ на fn10: unit + func + start(2) + count(2) = length 6.
	if mbLen != 6 {
		c.closeConn()
		return fmt.Errorf("meter write: некорректный MBAP length=%d, ждали 6", mbLen)
	}
	if rest[0] != 0x10 {
		c.closeConn()
		return fmt.Errorf("meter write: неожиданная функция 0x%02X", rest[0])
	}
	if got := binary.BigEndian.Uint16(rest[1:3]); got != start {
		c.closeConn()
		return fmt.Errorf("meter write: эхо start=%d, ждали %d", got, start)
	}
	if got := binary.BigEndian.Uint16(rest[3:5]); got != count {
		c.closeConn()
		return fmt.Errorf("meter write: эхо count=%d, ждали %d", got, count)
	}
	return nil
}

// writeRTULocked выполняет запись Modbus RTU поверх TCP (unit + PDU + CRC16).
func (c *meterClient) writeRTULocked(ctx context.Context, start uint16, values []uint16) error {
	c.drainLocked()
	count := uint16(len(values))
	dataLen := 2 * len(values)
	req := make([]byte, 0, 9+dataLen)
	req = append(req, c.Unit, 0x10)
	req = binary.BigEndian.AppendUint16(req, start)
	req = binary.BigEndian.AppendUint16(req, count)
	req = append(req, byte(dataLen))
	for _, v := range values {
		req = binary.BigEndian.AppendUint16(req, v)
	}
	crc := solarman.CRC16Modbus(req)
	req = append(req, byte(crc), byte(crc>>8))

	if err := c.writeReqLocked(ctx, req); err != nil {
		return err
	}

	// Ответ: unit, func, start(2), count(2), CRC(2) = 8 байт (или exception).
	hdr := make([]byte, 3)
	if _, err := modbusmap.ReadFull(c.conn, hdr); err != nil {
		c.closeConn()
		return fmt.Errorf("meter write rtu read header: %w", err)
	}
	if hdr[0] != c.Unit {
		c.closeConn()
		return fmt.Errorf("meter write rtu: несовпадение unit id: ожидался %d, получен %d", c.Unit, hdr[0])
	}
	if hdr[1]&0x80 != 0 {
		tail := make([]byte, 2)
		if _, err := modbusmap.ReadFull(c.conn, tail); err != nil {
			c.closeConn()
			return fmt.Errorf("meter write rtu read exception crc: %w", err)
		}
		frame := append(hdr, tail...)
		if !meterRTUCRCOK(frame) {
			c.closeConn()
			return fmt.Errorf("meter write rtu: некорректный CRC16 в exception-кадре")
		}
		return fmt.Errorf("meter write: modbus exception func=0x%02X code=0x%02X", hdr[1], hdr[2])
	}
	if hdr[1] != 0x10 {
		c.closeConn()
		return fmt.Errorf("meter write rtu: неожиданная функция 0x%02X", hdr[1])
	}
	// hdr[2] — старший байт start; дочитываем start lo, count hi/lo, CRC(2).
	rest := make([]byte, 5)
	if _, err := modbusmap.ReadFull(c.conn, rest); err != nil {
		c.closeConn()
		return fmt.Errorf("meter write rtu read data: %w", err)
	}
	frame := append(hdr, rest...)
	if !meterRTUCRCOK(frame) {
		c.closeConn()
		return fmt.Errorf("meter write rtu: некорректный CRC16")
	}
	gotStart := binary.BigEndian.Uint16([]byte{hdr[2], rest[0]})
	gotCount := binary.BigEndian.Uint16(rest[1:3])
	if gotStart != start {
		c.closeConn()
		return fmt.Errorf("meter write rtu: эхо start=%d, ждали %d", gotStart, start)
	}
	if gotCount != count {
		c.closeConn()
		return fmt.Errorf("meter write rtu: эхо count=%d, ждали %d", gotCount, count)
	}
	return nil
}

// readTCPLocked выполняет один запрос Modbus TCP (MBAP + PDU, без CRC).
func (c *meterClient) readTCPLocked(ctx context.Context, start, count uint16) ([]uint16, error) {
	// MBAP + PDU (func 03, start, count)
	req := make([]byte, 0, 12)
	req = binary.BigEndian.AppendUint16(req, c.nextTxn())
	req = binary.BigEndian.AppendUint16(req, 0) // protocol
	req = binary.BigEndian.AppendUint16(req, 6) // length
	req = append(req, c.Unit, c.fn())
	req = binary.BigEndian.AppendUint16(req, start)
	req = binary.BigEndian.AppendUint16(req, count)

	if err := c.writeReqLocked(ctx, req); err != nil {
		return nil, err
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
	// Проверяем только минимум (нужно прочитать funcID и байт кода/bytecount):
	// exception-кадр (func с 0x80) всегда mbLen==3 и обрабатывается ниже, поэтому
	// строгую проверку полной длины делаем ПОСЛЕ разбора исключения — иначе
	// Modbus-exception маскировался бы под «некорректный MBAP length».
	if mbLen < 3 {
		c.closeConn()
		return nil, fmt.Errorf("meter: некорректный MBAP length=%d (минимум 3)", mbLen)
	}
	rest := make([]byte, mbLen-1) // минус unit id (уже в hdr[6])
	if _, err := modbusmap.ReadFull(c.conn, rest); err != nil {
		c.closeConn()
		return nil, fmt.Errorf("meter read pdu: %w", err)
	}
	if rest[0]&0x80 != 0 {
		return nil, fmt.Errorf("meter: modbus exception func=0x%02X code=0x%02X", rest[0], rest[1])
	}
	// Данные: ровно unit+func+bytecount+data = 3+2*count. Строгая проверка, чтобы
	// слайс rest[2:2+bc] не вышел за буфер (усечённый кадр с завышенным bytecount
	// иначе дал бы панику в пулере).
	if mbLen != 3+2*int(count) {
		c.closeConn()
		return nil, fmt.Errorf("meter: некорректный MBAP length=%d, ждали %d", mbLen, 3+2*int(count))
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

// meterRTUCRCOK проверяет CRC16/MODBUS кадра RTU (CRC — два младших байта,
// little-endian, в конце кадра).
func meterRTUCRCOK(frame []byte) bool {
	if len(frame) < 4 {
		return false
	}
	want := binary.LittleEndian.Uint16(frame[len(frame)-2:])
	return solarman.CRC16Modbus(frame[:len(frame)-2]) == want
}

// readRTULocked выполняет один запрос Modbus RTU поверх TCP: сырой серийный кадр
// (unit + PDU + CRC16, без MBAP). Так работает прозрачный шлюз USR-DR164.
func (c *meterClient) readRTULocked(ctx context.Context, start, count uint16) ([]uint16, error) {
	// Перед запросом осушаем сокет: у RTU нет transaction id, задержавшийся
	// ответ предыдущего опроса мог бы быть принят за текущий.
	c.drainLocked()

	// PDU: unit + func (03/04) + start + count + CRC16.
	req := make([]byte, 0, 8)
	req = append(req, c.Unit, c.fn())
	req = binary.BigEndian.AppendUint16(req, start)
	req = binary.BigEndian.AppendUint16(req, count)
	crc := solarman.CRC16Modbus(req)
	req = append(req, byte(crc), byte(crc>>8))

	if err := c.writeReqLocked(ctx, req); err != nil {
		return nil, err
	}

	// Заголовок ответа: unit, func, bytecount/exception-code.
	hdr := make([]byte, 3)
	if _, err := modbusmap.ReadFull(c.conn, hdr); err != nil {
		c.closeConn()
		return nil, fmt.Errorf("meter rtu read header: %w", err)
	}
	if hdr[0] != c.Unit {
		c.closeConn()
		return nil, fmt.Errorf("meter rtu: несовпадение unit id: ожидался %d, получен %d", c.Unit, hdr[0])
	}
	if hdr[1]&0x80 != 0 {
		// Exception: unit, func|0x80, код, CRC(2).
		tail := make([]byte, 2)
		if _, err := modbusmap.ReadFull(c.conn, tail); err != nil {
			c.closeConn()
			return nil, fmt.Errorf("meter rtu read exception crc: %w", err)
		}
		frame := append(hdr, tail...)
		if !meterRTUCRCOK(frame) {
			c.closeConn()
			return nil, fmt.Errorf("meter rtu: некорректный CRC16 в exception-кадре")
		}
		return nil, fmt.Errorf("meter: modbus exception func=0x%02X code=0x%02X", hdr[1], hdr[2])
	}
	if hdr[1] != c.fn() {
		c.closeConn()
		return nil, fmt.Errorf("meter rtu: неожиданная функция 0x%02X (ждали 0x%02X)", hdr[1], c.fn())
	}
	bc := int(hdr[2])
	if bc != 2*int(count) {
		c.closeConn()
		return nil, fmt.Errorf("meter rtu: bytecount=%d, ждали %d", bc, 2*int(count))
	}
	// Данные + CRC(2).
	rest := make([]byte, bc+2)
	if _, err := modbusmap.ReadFull(c.conn, rest); err != nil {
		c.closeConn()
		return nil, fmt.Errorf("meter rtu read data: %w", err)
	}
	frame := append(hdr, rest...)
	if !meterRTUCRCOK(frame) {
		c.closeConn()
		return nil, fmt.Errorf("meter rtu: некорректный CRC16")
	}
	data := rest[:bc]
	regs := make([]uint16, count)
	for i := 0; i < int(count); i++ {
		regs[i] = binary.BigEndian.Uint16(data[2*i:])
	}
	return regs, nil
}
