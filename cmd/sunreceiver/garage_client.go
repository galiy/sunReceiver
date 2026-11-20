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
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// garageClient — TCP-клиент 4-канального реле relay4 (ESP-01S + STM8S103).
// Протокол — текстовые строки с '\n' (порт 5000): STATE / SET N 0|1 /
// MONITOR 0|1; ответы STATE d d d d, OK N d и OK MONITOR v d d d d. HTTP не
// используется (требование оператора). Соединение постоянное: модуль держит
// сокет и не закрывает его после каждой команды; при ошибке — сброс и
// повторное подключение на следующем обращении (одна повторная попытка сразу).
// MONITOR 1 — продление аренды устройства (watchdog): аренду продлевает только
// эта команда, ответ содержит состояние всех реле.
//
// Устройство держит только ОДНО TCP-соединение и закрывает простаивающий сокет
// через 60 с, поэтому модуль шлёт STATE не реже garage.tcp_keepalive_sec.
type garageClient struct {
	addr        string
	dialTimeout time.Duration
	opTimeout   time.Duration

	mu     sync.Mutex
	conn   net.Conn
	rd     *bufio.Reader
	lastIO time.Time
}

// garageTCPDialTimeout / garageTCPOpTimeout — таймауты подключения и одной
// команды. Устройство тратит до ~100 мс на кадр STM8; 3 с с запасом.
const (
	garageTCPDialTimeout = 3 * time.Second
	garageTCPOpTimeout   = 3 * time.Second
	garageTCPMaxLine     = 128
)

func newGarageClient(addr string) *garageClient {
	return &garageClient{
		addr:        addr,
		dialTimeout: garageTCPDialTimeout,
		opTimeout:   garageTCPOpTimeout,
	}
}

// LastIO — время последнего успешного обмена (для keepalive).
func (c *garageClient) LastIO() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastIO
}

// Close закрывает текущее соединение (идемпотентно).
func (c *garageClient) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeLocked()
}

func (c *garageClient) closeLocked() {
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
		c.rd = nil
	}
}

// connected сообщает, есть ли открытый сокет (для диагностики).
func (c *garageClient) connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn != nil
}

// ensureLocked открывает TCP-соединение, если его нет.
func (c *garageClient) ensureLocked(ctx context.Context) error {
	if c.conn != nil {
		return nil
	}
	d := net.Dialer{Timeout: c.dialTimeout}
	conn, err := d.DialContext(ctx, "tcp", c.addr)
	if err != nil {
		return fmt.Errorf("relay4 dial %s: %w", c.addr, err)
	}
	c.conn = conn
	c.rd = bufio.NewReaderSize(conn, 256)
	c.lastIO = time.Now()
	return nil
}

// command отправляет одну строку и читает ответную строку. При ошибке сокет
// закрывается: следующая команда переподключится.
func (c *garageClient) command(ctx context.Context, line string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureLocked(ctx); err != nil {
		return "", err
	}
	// Дедлайн — минимум из внешнего контекста и собственного таймаута операции:
	// длинный/нулевой внешний дедлайн не должен держать мьютекс при зависшем чтении.
	deadline := time.Now().Add(c.opTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = c.conn.SetDeadline(deadline)
	if _, err := io.WriteString(c.conn, line+"\n"); err != nil {
		c.closeLocked()
		return "", fmt.Errorf("relay4 write %q: %w", line, err)
	}
	resp, err := c.readLineLocked()
	if err != nil {
		c.closeLocked()
		return "", fmt.Errorf("relay4 read %q: %w", line, err)
	}
	c.lastIO = time.Now()
	return resp, nil
}

// readLineLocked читает одну строку ответа (до '\n' или '\r'), ограничивая
// длину garageTCPMaxLine символов (как контракт устройства).
func (c *garageClient) readLineLocked() (string, error) {
	var b strings.Builder
	for {
		ch, err := c.rd.ReadByte()
		if err != nil {
			return "", err
		}
		if ch == '\n' || ch == '\r' {
			if b.Len() == 0 {
				continue // пустые строки игнорируются
			}
			return b.String(), nil
		}
		if b.Len() >= garageTCPMaxLine {
			return "", fmt.Errorf("слишком длинная строка ответа")
		}
		b.WriteByte(ch)
	}
}

// request выполняет команду. Повторная попытка — только при ТРАНСПОРТНОЙ ошибке
// (устройство могло закрыть простаивающее соединение; команда идемпотентна).
// Ошибка проверки ответа (ERR .../несовпадение) не ретраится: соединение при этом
// закрывается, чтобы не читать рассинхронизированный буфер.
func (c *garageClient) request(ctx context.Context, line string, check func(string) error) error {
	resp, err := c.command(ctx, line)
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		if resp2, err2 := c.command(ctx, line); err2 == nil {
			return c.verify(line, resp2, check)
		}
		return err
	}
	return c.verify(line, resp, check)
}

// verify проверяет ответ; при несоответствии закрывает соединение.
func (c *garageClient) verify(line, resp string, check func(string) error) error {
	if check == nil {
		return nil
	}
	if err := check(resp); err != nil {
		c.Close()
		return fmt.Errorf("relay4 %q: %w (ответ %q)", line, err, resp)
	}
	return nil
}

// State читает состояние всех 4 каналов.
func (c *garageClient) State(ctx context.Context) ([4]bool, error) {
	var out [4]bool
	err := c.request(ctx, "STATE", func(resp string) error {
		states, err := parseGarageState(resp)
		if err != nil {
			return err
		}
		out = states
		return nil
	})
	if err != nil {
		return out, err
	}
	return out, nil
}

// Monitor включает/выключает режим периодического мониторинга (аренду) и
// возвращает состояние реле из ответа. Аренду продлевает только MONITOR 1
// (STATE/HTTP её не продлевают); при включённой аренде устройство через
// 5 минут без MONITOR 1 ставит все реле off и перезагружается.
func (c *garageClient) Monitor(ctx context.Context, on bool) ([4]bool, error) {
	var out [4]bool
	v := 0
	if on {
		v = 1
	}
	line := fmt.Sprintf("MONITOR %d", v)
	err := c.request(ctx, line, func(resp string) error {
		states, got, err := parseGarageMonitor(resp)
		if err != nil {
			return err
		}
		if got != v {
			return fmt.Errorf("подтверждение не совпадает: ждали MONITOR %d", v)
		}
		out = states
		return nil
	})
	if err != nil {
		return out, err
	}
	return out, nil
}

// Host сообщает, завершил ли хост-МК AT-инициализацию (host_ready).
// До готовности хоста кадры реле им игнорируются.
func (c *garageClient) Host(ctx context.Context) (bool, error) {
	var ready bool
	err := c.request(ctx, "HOST", func(resp string) error {
		v, err := parseGarageHost(resp)
		if err != nil {
			return err
		}
		ready = v
		return nil
	})
	return ready, err
}

// parseGarageHost разбирает ответ "HOST 0|1".
func parseGarageHost(resp string) (bool, error) {
	f := strings.Fields(strings.TrimSpace(resp))
	if len(f) != 2 || !strings.EqualFold(f[0], "HOST") {
		return false, fmt.Errorf("некорректный ответ HOST")
	}
	switch f[1] {
	case "0":
		return false, nil
	case "1":
		return true, nil
	}
	return false, fmt.Errorf("некорректное состояние HOST")
}

// Set устанавливает канал n (1..4) в состояние on.
func (c *garageClient) Set(ctx context.Context, n int, on bool) error {
	if n < 1 || n > 4 {
		return fmt.Errorf("relay4: канал %d вне 1..4", n)
	}
	v := 0
	if on {
		v = 1
	}
	line := fmt.Sprintf("SET %d %d", n, v)
	return c.request(ctx, line, func(resp string) error {
		gotN, gotV, err := parseGarageOK(resp)
		if err != nil {
			return err
		}
		if gotN != n || gotV != v {
			return fmt.Errorf("подтверждение не совпадает: ждали канал %d=%d", n, v)
		}
		return nil
	})
}

// parseGarageState разбирает ответ "STATE d d d d" (d = 0/1).
func parseGarageState(resp string) ([4]bool, error) {
	var out [4]bool
	f := strings.Fields(strings.TrimSpace(resp))
	if len(f) != 5 || !strings.EqualFold(f[0], "STATE") {
		return out, fmt.Errorf("некорректный ответ STATE")
	}
	for i := 0; i < 4; i++ {
		switch f[i+1] {
		case "0":
			out[i] = false
		case "1":
			out[i] = true
		default:
			return out, fmt.Errorf("некорректное состояние канала %d", i+1)
		}
	}
	return out, nil
}

// parseGarageMonitor разбирает ответ "OK MONITOR v d d d d".
func parseGarageMonitor(resp string) ([4]bool, int, error) {
	var out [4]bool
	f := strings.Fields(strings.TrimSpace(resp))
	if len(f) != 7 || !strings.EqualFold(f[0], "OK") || !strings.EqualFold(f[1], "MONITOR") {
		return out, 0, fmt.Errorf("некорректный ответ MONITOR")
	}
	if f[2] != "0" && f[2] != "1" {
		return out, 0, fmt.Errorf("некорректное состояние MONITOR")
	}
	v, _ := strconv.Atoi(f[2])
	for i := 0; i < 4; i++ {
		switch f[i+3] {
		case "0":
			out[i] = false
		case "1":
			out[i] = true
		default:
			return out, 0, fmt.Errorf("некорректное состояние канала %d", i+1)
		}
	}
	return out, v, nil
}

// parseGarageOK разбирает ответ "OK N d" (N = 0..4, d = 0/1).
func parseGarageOK(resp string) (int, int, error) {
	f := strings.Fields(strings.TrimSpace(resp))
	if len(f) != 3 || !strings.EqualFold(f[0], "OK") {
		return 0, 0, fmt.Errorf("некорректный ответ OK")
	}
	n, err := strconv.Atoi(f[1])
	if err != nil || n < 0 || n > 4 {
		return 0, 0, fmt.Errorf("некорректный номер канала")
	}
	if f[2] != "0" && f[2] != "1" {
		return 0, 0, fmt.Errorf("некорректное состояние канала")
	}
	v, _ := strconv.Atoi(f[2])
	return n, v, nil
}
