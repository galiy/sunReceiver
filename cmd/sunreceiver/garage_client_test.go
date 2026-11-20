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
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

func TestParseGarageStateOK(t *testing.T) {
	st, err := parseGarageState("STATE 1 0 1 0")
	if err != nil {
		t.Fatal(err)
	}
	if !st[0] || st[1] || !st[2] || st[3] {
		t.Fatalf("неверный разбор: %v", st)
	}
	if _, err := parseGarageState("STATE 1 0 1"); err == nil {
		t.Fatal("ожидали ошибку для неполного ответа")
	}
	if _, err := parseGarageState("OK 1 1"); err == nil {
		t.Fatal("ожидали ошибку для чужой команды")
	}
	if _, err := parseGarageState("STATE 1 0 2 0"); err == nil {
		t.Fatal("ожидали ошибку для некорректного состояния")
	}
}

func TestParseGarageOK(t *testing.T) {
	n, v, err := parseGarageOK("OK 2 1")
	if err != nil || n != 2 || v != 1 {
		t.Fatalf("parseGarageOK: n=%d v=%d err=%v", n, v, err)
	}
	if _, _, err := parseGarageOK("ERR channel"); err == nil {
		t.Fatal("ожидали ошибку для ERR")
	}
}

// startRelay4Mock поднимает минимальный TCP-сервер протокола relay4: STATE и SET.
// acceptN — сколько соединений обслужить (проверка переподключения).
func startRelay4Mock(t *testing.T, acceptN int) (addr string, done chan struct{}) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done = make(chan struct{})
	go func() {
		defer ln.Close()
		defer close(done)
		var relays [4]bool
		for i := 0; i < acceptN; i++ {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			func() {
				defer conn.Close()
				sc := bufio.NewScanner(conn)
				for sc.Scan() {
					f := strings.Fields(sc.Text())
					if len(f) == 0 {
						continue
					}
					switch strings.ToUpper(f[0]) {
					case "STATE":
						writeState(conn, relays)
					case "HOST":
						_, _ = conn.Write([]byte("HOST 1\n"))
					case "MONITOR":
						v := "0"
						if len(f) >= 2 && f[1] == "1" {
							v = "1"
						}
						out := "OK MONITOR " + v
						for _, r := range relays {
							if r {
								out += " 1"
							} else {
								out += " 0"
							}
						}
						_, _ = conn.Write([]byte(out + "\n"))
					case "SET":
						if len(f) == 3 {
							n := int(f[1][0] - '0')
							on := f[2] == "1"
							if n >= 1 && n <= 4 {
								relays[n-1] = on
								v := "0"
								if on {
									v = "1"
								}
								_, _ = conn.Write([]byte("OK " + f[1] + " " + v + "\n"))
							}
						}
					}
				}
			}()
		}
	}()
	return ln.Addr().String(), done
}

func writeState(conn net.Conn, relays [4]bool) {
	parts := make([]string, 0, 5)
	parts = append(parts, "STATE")
	for _, r := range relays {
		if r {
			parts = append(parts, "1")
		} else {
			parts = append(parts, "0")
		}
	}
	_, _ = conn.Write([]byte(strings.Join(parts, " ") + "\n"))
}

func TestGarageClientStateSetReconnect(t *testing.T) {
	addr, _ := startRelay4Mock(t, 2)
	cli := newGarageClient(addr)
	defer cli.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	st, err := cli.State(ctx)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st != [4]bool{} {
		t.Fatalf("начальное состояние: %v", st)
	}
	if err := cli.Set(ctx, 2, true); err != nil {
		t.Fatalf("Set: %v", err)
	}
	st, err = cli.State(ctx)
	if err != nil {
		t.Fatalf("State after set: %v", err)
	}
	if !st[1] {
		t.Fatalf("реле 2 должно быть on: %v", st)
	}

	// Имитируем закрытие соединения устройством: клиент должен переподключиться.
	cli.Close()
	st, err = cli.State(ctx)
	if err != nil {
		t.Fatalf("State after reconnect: %v", err)
	}
	if !st[1] {
		t.Fatalf("после переподключения состояние реле должно сохраниться: %v", st)
	}
}

// Ответ ERR на SET — ошибка без ретрая, соединение закрывается (следующий STATE
// переподключается), терминатор \r тоже принимается.
func TestGarageClientErrorReplyClosesConn(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for i := 0; i < 2; i++ {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				sc := bufio.NewScanner(conn)
				for sc.Scan() {
					f := strings.Fields(sc.Text())
					if len(f) == 0 {
						continue
					}
					if strings.EqualFold(f[0], "SET") {
						_, _ = conn.Write([]byte("ERR channel\r"))
						continue
					}
					_, _ = conn.Write([]byte("STATE 0 0 0 0\r\n"))
				}
			}(conn)
		}
	}()
	cli := newGarageClient(ln.Addr().String())
	defer cli.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := cli.Set(ctx, 2, true); err == nil {
		t.Fatal("ожидали ошибку на ERR channel")
	}
	if cli.connected() {
		t.Fatal("после ERR-ответа соединение должно быть закрыто")
	}
	if _, err := cli.State(ctx); err != nil {
		t.Fatalf("после ERR клиент должен переподключиться: %v", err)
	}
}

// MONITOR 1 продлевает аренду и возвращает состояние реле одним ответом.
func TestGarageClientMonitor(t *testing.T) {
	addr, _ := startRelay4Mock(t, 1)
	cli := newGarageClient(addr)
	defer cli.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := cli.Set(ctx, 3, true); err != nil {
		t.Fatalf("Set: %v", err)
	}
	st, err := cli.Monitor(ctx, true)
	if err != nil {
		t.Fatalf("Monitor: %v", err)
	}
	if !st[2] {
		t.Fatalf("MONITOR должен вернуть состояние реле: %v", st)
	}
	if _, err := cli.Monitor(ctx, false); err != nil {
		t.Fatalf("Monitor off: %v", err)
	}
}

func TestParseGarageMonitor(t *testing.T) {
	st, v, err := parseGarageMonitor("OK MONITOR 1 0 1 0 1")
	if err != nil || v != 1 {
		t.Fatalf("v=%d err=%v", v, err)
	}
	if st[0] || !st[1] || st[2] || !st[3] {
		t.Fatalf("неверный разбор: %v", st)
	}
	if _, _, err := parseGarageMonitor("OK 1 1"); err == nil {
		t.Fatal("ожидали ошибку формата")
	}
	if _, _, err := parseGarageMonitor("OK MONITOR 2 0 0 0 0"); err == nil {
		t.Fatal("ожидали ошибку для v вне 0/1")
	}
	if _, _, err := parseGarageMonitor("OK MONITOR 1 0 0 0"); err == nil {
		t.Fatal("ожидали ошибку для неполного ответа")
	}
	if _, _, err := parseGarageMonitor("OK MONITOR 1 0 0 0 5"); err == nil {
		t.Fatal("ожидали ошибку для некорректного состояния канала")
	}
}

// Несовпадение режима в ответе MONITOR — ошибка, сокет закрывается, без ретрая.
func TestGarageClientMonitorMismatch(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		sc := bufio.NewScanner(conn)
		for sc.Scan() {
			_, _ = conn.Write([]byte("OK MONITOR 0 0 0 0 0\n"))
		}
	}()
	cli := newGarageClient(ln.Addr().String())
	defer cli.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := cli.Monitor(ctx, true); err == nil {
		t.Fatal("ожидали ошибку несовпадения режима")
	}
	if cli.connected() {
		t.Fatal("после несовпадения соединение должно быть закрыто")
	}
}

func TestParseGarageHost(t *testing.T) {
	v, err := parseGarageHost("HOST 1")
	if err != nil || !v {
		t.Fatalf("HOST 1: v=%v err=%v", v, err)
	}
	v, err = parseGarageHost("HOST 0")
	if err != nil || v {
		t.Fatalf("HOST 0: v=%v err=%v", v, err)
	}
	if _, err := parseGarageHost("HOST 2"); err == nil {
		t.Fatal("ожидали ошибку для HOST 2")
	}
	if _, err := parseGarageHost("STATE 0 0 0 0"); err == nil {
		t.Fatal("ожидали ошибку формата")
	}
}

// HOST возвращает готовность хоста по TCP-ответу.
func TestGarageClientHost(t *testing.T) {
	addr, _ := startRelay4Mock(t, 1)
	cli := newGarageClient(addr)
	defer cli.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ready, err := cli.Host(ctx)
	if err != nil {
		t.Fatalf("Host: %v", err)
	}
	if !ready {
		t.Fatal("мок отвечает HOST 1")
	}
}

// HOST 0 и некорректный ответ: готовность false, ошибка закрывает сокет.
func TestGarageClientHostCases(t *testing.T) {
	// HOST 0
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		sc := bufio.NewScanner(conn)
		for sc.Scan() {
			_, _ = conn.Write([]byte("HOST 0\n"))
		}
	}()
	cli := newGarageClient(ln.Addr().String())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ready, err := cli.Host(ctx)
	if err != nil || ready {
		t.Fatalf("HOST 0: ready=%v err=%v", ready, err)
	}
	cli.Close()
	ln.Close()

	// ERR command
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln2.Close()
	go func() {
		conn, err := ln2.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		sc := bufio.NewScanner(conn)
		for sc.Scan() {
			_, _ = conn.Write([]byte("ERR command\n"))
		}
	}()
	cli2 := newGarageClient(ln2.Addr().String())
	defer cli2.Close()
	if _, err := cli2.Host(ctx); err == nil {
		t.Fatal("ожидали ошибку на ERR command")
	}
	if cli2.connected() {
		t.Fatal("после ERR соединение должно быть закрыто")
	}
}
