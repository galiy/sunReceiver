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

package modbusmap

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

// mbResp — ответ Modbus-TCP сервера теста.
type mbResp struct {
	txn  uint16
	unit byte
	data []byte // слова (байт-ячейки), func 03
}

// startMBServer поднимает минимальный Modbus-TCP сервер, который на каждый
// входящий запрос читает MBAP+PDU (12 байт), вызывает responder для вычисления
// ответа и шлёт кадр (MBAP + func 0x03 + bytecount + data).
func startMBServer(t *testing.T, responder func(req []byte) mbResp) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				req := make([]byte, 12)
				if _, err := ReadFull(c, req); err != nil {
					return
				}
				r := responder(req)
				out := make([]byte, 0, 7+1+len(r.data))
				out = binary.BigEndian.AppendUint16(out, r.txn)
				out = binary.BigEndian.AppendUint16(out, 0)
				out = binary.BigEndian.AppendUint16(out, uint16(1+1+1+len(r.data)))
				out = append(out, r.unit)
				out = append(out, 0x03)
				out = append(out, byte(len(r.data)))
				out = append(out, r.data...)
				_, _ = c.Write(out)
			}(conn)
		}
	}()
	return ln.Addr().String()
}

func reqTxn(req []byte) uint16 { return binary.BigEndian.Uint16(req[0:2]) }

func TestReadRegistersValidTxnUnit(t *testing.T) {
	addr := startMBServer(t, func(req []byte) mbResp {
		// Эхо реального txn и корректный unit — данные ячеек проходят.
		return mbResp{txn: reqTxn(req), unit: 0x01, data: []byte{0xAB, 0xCD}}
	})
	c := &Client{Address: addr, Unit: 0x01}
	got, err := c.ReadRegisters(context.Background(), 0x0400, 1)
	if err != nil {
		t.Fatalf("valid txn/unit: err=%v", err)
	}
	if len(got) != 2 || got[0] != 0xAB || got[1] != 0xCD {
		t.Fatalf("got = %#v, want [0xAB 0xCD] (cell 0x400, 0x401)", got)
	}
}

func TestReadRegistersWrongTxn(t *testing.T) {
	addr := startMBServer(t, func(req []byte) mbResp {
		// Поздний ответ от старого запроса: txn на 1 больше отправленного.
		return mbResp{txn: reqTxn(req) + 1, unit: 0x01, data: []byte{0x11, 0x22}}
	})
	c := &Client{Address: addr, Unit: 0x01}
	if _, err := c.ReadRegisters(context.Background(), 0x0400, 1); err == nil {
		t.Fatal("wrong txn: ожидали ошибку")
	}
	if c.conn != nil {
		t.Fatal("wrong txn: соединение не закрыто (late-ответ может попасть в следующий опрос)")
	}
}

func TestReadRegistersWrongUnit(t *testing.T) {
	addr := startMBServer(t, func(req []byte) mbResp {
		return mbResp{txn: reqTxn(req), unit: 0x02, data: []byte{0x33, 0x44}}
	})
	c := &Client{Address: addr, Unit: 0x01}
	if _, err := c.ReadRegisters(context.Background(), 0x0400, 1); err == nil {
		t.Fatal("wrong unit: ожидали ошибку")
	}
	if c.conn != nil {
		t.Fatal("wrong unit: соединение не закрыто")
	}
}

// stallConn — Read всегда (0, nil): ReadFull не должен зацикливаться.
type stallConn struct{}

func (stallConn) Read([]byte) (int, error)         { return 0, nil }
func (stallConn) Write(b []byte) (int, error)      { return len(b), nil }
func (stallConn) Close() error                     { return nil }
func (stallConn) LocalAddr() net.Addr              { return nil }
func (stallConn) RemoteAddr() net.Addr             { return nil }
func (stallConn) SetDeadline(time.Time) error      { return nil }
func (stallConn) SetReadDeadline(time.Time) error  { return nil }
func (stallConn) SetWriteDeadline(time.Time) error { return nil }

func TestReadFullNoProgress(t *testing.T) {
	if _, err := ReadFull(stallConn{}, make([]byte, 4)); err != io.ErrNoProgress {
		t.Fatalf("err = %v, want io.ErrNoProgress", err)
	}
}
