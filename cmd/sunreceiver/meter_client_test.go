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
	"io"
	"net"
	"testing"
	"time"

	"github.com/galiy/sunReceiver/internal/solarman"
)

// startFakeMeterServer поднимает TCP-сервер, который на один запрос отвечает
// заранее заданным обработчиком. Возвращает адрес и функцию остановки.
func startFakeMeterServer(t *testing.T, handle func(conn net.Conn)) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		handle(conn)
	}()
	return ln.Addr().String(), func() {
		ln.Close()
		<-done
	}
}

// TestMeterClientRTU проверяет опрос через прозрачный шлюз (Modbus RTU поверх TCP):
// запрос — сырой кадр unit+PDU+CRC16 (без MBAP), ответ — так же.
func TestMeterClientRTU(t *testing.T) {
	const unit = 1
	wantRegs := []uint16{0x015A, 0xA467, 0x0036}

	addr, stop := startFakeMeterServer(t, func(conn net.Conn) {
		req := make([]byte, 8)
		if _, err := readFullForTest(conn, req); err != nil {
			t.Errorf("чтение запроса: %v", err)
			return
		}
		// Проверяем форму запроса: unit, func 03, start=0, count=3 + CRC16.
		wantReq := []byte{unit, 0x03, 0, 0, 0, 3}
		crc := solarman.CRC16Modbus(wantReq)
		wantReq = append(wantReq, byte(crc), byte(crc>>8))
		if string(req) != string(wantReq) {
			t.Errorf("запрос = % x, want % x", req, wantReq)
			return
		}
		// Ответ: unit, func 03, bytecount, data, CRC16.
		resp := []byte{unit, 0x03, byte(2 * len(wantRegs))}
		for _, r := range wantRegs {
			resp = binary.BigEndian.AppendUint16(resp, r)
		}
		rcrc := solarman.CRC16Modbus(resp)
		resp = append(resp, byte(rcrc), byte(rcrc>>8))
		conn.Write(resp)
	})
	defer stop()

	c := newMeterClient(addr, unit, true)
	regs, err := c.ReadHoldingRegisters(context.Background(), 0, uint16(len(wantRegs)))
	if err != nil {
		t.Fatalf("ReadHoldingRegisters: %v", err)
	}
	if len(regs) != len(wantRegs) {
		t.Fatalf("regs=%v, want %v", regs, wantRegs)
	}
	for i := range wantRegs {
		if regs[i] != wantRegs[i] {
			t.Errorf("regs[%d]=0x%04X, want 0x%04X", i, regs[i], wantRegs[i])
		}
	}
}

// TestMeterClientRTUException: ответ-исключение разбирается и возвращается ошибка.
func TestMeterClientRTUException(t *testing.T) {
	const unit = 1
	addr, stop := startFakeMeterServer(t, func(conn net.Conn) {
		req := make([]byte, 8)
		if _, err := readFullForTest(conn, req); err != nil {
			return
		}
		resp := []byte{unit, 0x83, 0x02}
		crc := solarman.CRC16Modbus(resp)
		resp = append(resp, byte(crc), byte(crc>>8))
		conn.Write(resp)
	})
	defer stop()

	c := newMeterClient(addr, unit, true)
	if _, err := c.ReadHoldingRegisters(context.Background(), 0, 3); err == nil {
		t.Fatal("ожидали ошибку modbus exception")
	}
}

// TestMeterClientRTUBadCRC: неверный CRC приводит к ошибке (соединение закрывается).
func TestMeterClientRTUBadCRC(t *testing.T) {
	const unit = 1
	addr, stop := startFakeMeterServer(t, func(conn net.Conn) {
		req := make([]byte, 8)
		if _, err := readFullForTest(conn, req); err != nil {
			return
		}
		resp := []byte{unit, 0x03, 2, 0x01, 0x5A, 0x00, 0x00} // CRC заведомо неверный
		conn.Write(resp)
	})
	defer stop()

	c := newMeterClient(addr, unit, true)
	if _, err := c.ReadHoldingRegisters(context.Background(), 0, 1); err == nil {
		t.Fatal("ожидали ошибку CRC")
	}
}

func readFullForTest(conn net.Conn, buf []byte) (int, error) {
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	return io.ReadFull(conn, buf)
}
