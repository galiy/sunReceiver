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
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// Кадровый слой BK300 (KONNWEI, BLE serial-over-GATT). Протокол и происхождение
// — docs/modules/agm-monitor.md §2.2.
//
// Запрос: "@@" + len(LE) + cmd(2) + payload + CRC-16/X-25(LE) + "\r\n".
// Ответ:  "$$" + len(LE) + cmd(2) + payload + CRC + "\r\n"; ответ на 0B0B —
// команда 4B 0B, payload: напряжение (LE, ×0.01 В), флаг A, флаг B.

var (
	errAGMBKFrame = errors.New("некорректный кадр BK300")
	errAGMBKReply = errors.New("ответ BK300 не соответствует запросу")
)

// Команды BK300 (двухбайтовые).
var (
	agmCmdBKVoltage = [2]byte{0x0B, 0x0B}
	agmCmdBKSerial  = [2]byte{0x02, 0x01}
)

// agmCRC16X25 — CRC-16/X-25 (poly 0x1021 отражённо 0x8408, init 0xFFFF,
// xorout 0xFFFF).
func agmCRC16X25(b []byte) uint16 {
	crc := uint16(0xFFFF)
	for _, c := range b {
		crc ^= uint16(c)
		for i := 0; i < 8; i++ {
			if crc&1 != 0 {
				crc = (crc >> 1) ^ 0x8408
			} else {
				crc >>= 1
			}
		}
	}
	return crc ^ 0xFFFF
}

// buildAGMBKRequest собирает запрос BK300.
func buildAGMBKRequest(cmd [2]byte, payload []byte) []byte {
	total := 10 + len(payload)
	b := make([]byte, 0, total)
	b = append(b, 0x40, 0x40, byte(total), byte(total>>8), cmd[0], cmd[1])
	b = append(b, payload...)
	crc := agmCRC16X25(b)
	b = append(b, byte(crc), byte(crc>>8), '\r', '\n')
	return b
}

// agmBKFrame — разобранный кадр ответа.
type agmBKFrame struct {
	Cmd     [2]byte
	Payload []byte
}

// agmBKFrameReader собирает кадры из фрагментов нотификаций.
type agmBKFrameReader struct {
	buf []byte
}

// Reset очищает накопленный буфер перед новым запросом.
func (r *agmBKFrameReader) Reset() { r.buf = r.buf[:0] }

// Push добавляет фрагмент и возвращает все полностью собранные и валидные кадры.
func (r *agmBKFrameReader) Push(p []byte) []agmBKFrame {
	if len(p) == 0 {
		return nil
	}
	r.buf = append(r.buf, p...)
	var out []agmBKFrame
	for {
		i := agmBKIndexMagic(r.buf)
		if i < 0 {
			// Сохраняем одиночный завершающий 0x24: магия может быть разрезана
			// между двумя нотификациями.
			if len(r.buf) > 0 && r.buf[len(r.buf)-1] == 0x24 {
				r.buf = r.buf[len(r.buf)-1:]
			} else {
				r.buf = r.buf[:0]
			}
			return out
		}
		if i > 0 {
			r.buf = r.buf[i:]
		}
		if n, ok := agmBKFrameLenAt(r.buf); ok {
			frame := r.buf[:n]
			payload := append([]byte(nil), frame[6:n-4]...)
			out = append(out, agmBKFrame{Cmd: [2]byte{frame[4], frame[5]}, Payload: payload})
			r.buf = r.buf[n:]
			continue
		}
		// Кадр в начале буфера неполон или битый. Если дальше уже лежит
		// законченный валидный кадр — объявленная длина была мусором, доверяем
		// более поздней магии (не ждём таймаута с пересозданием соединения).
		if j := agmBKNextValid(r.buf[1:]); j >= 0 {
			r.buf = r.buf[1+j:]
			continue
		}
		if len(r.buf) < 4 {
			return out
		}
		n := int(binary.LittleEndian.Uint16(r.buf[2:4]))
		// Длины хватает, но кадр битый (терминатор/CRC) — сдвигаемся на байт;
		// объявленная длина вне диапазона — тоже мусор.
		if n < 10 || n > 4096 || len(r.buf) >= n {
			r.buf = r.buf[1:]
			continue
		}
		// Кадр обещает больше байт, чем пришло, и валидной альтернативы нет —
		// ждём следующую нотификацию.
		return out
	}
}

// agmBKFrameLenAt проверяет, что в начале b лежит полный валидный кадр, и
// возвращает его длину.
func agmBKFrameLenAt(b []byte) (int, bool) {
	if len(b) < 4 || b[0] != 0x24 || b[1] != 0x24 {
		return 0, false
	}
	n := int(binary.LittleEndian.Uint16(b[2:4]))
	if n < 10 || n > 4096 || len(b) < n {
		return 0, false
	}
	if b[n-2] != '\r' || b[n-1] != '\n' {
		return 0, false
	}
	if agmCRC16X25(b[:n-4]) != binary.LittleEndian.Uint16(b[n-4:n-2]) {
		return 0, false
	}
	return n, true
}

// agmBKNextValid ищет индекс (с 0) полной валидной магии в b. Вызывается с
// buf[1:], поэтому кандидат с абсолютным индексом 1 (хвостовой 0x24 + кадр)
// тоже проверяется.
func agmBKNextValid(b []byte) int {
	for i := 0; i+1 < len(b); i++ {
		if b[i] == 0x24 && b[i+1] == 0x24 {
			if _, ok := agmBKFrameLenAt(b[i:]); ok {
				return i
			}
		}
	}
	return -1
}

// agmBKIndexMagic ищет магию ответа "$$".
func agmBKIndexMagic(b []byte) int {
	for i := 0; i+1 < len(b); i++ {
		if b[i] == 0x24 && b[i+1] == 0x24 {
			return i
		}
	}
	return -1
}

// parseAGMBKReply проверяет команду ответа (к первому байту запроса добавлен бит
// 0x40) и возвращает payload.
func parseAGMBKReply(cmd [2]byte, f agmBKFrame) ([]byte, error) {
	want := [2]byte{cmd[0] | 0x40, cmd[1]}
	if f.Cmd != want {
		return nil, fmt.Errorf("%w: команда %02X %02X, ожидалась %02X %02X",
			errAGMBKReply, f.Cmd[0], f.Cmd[1], want[0], want[1])
	}
	return f.Payload, nil
}

// parseAGMBKVoltage разбирает payload ответа 4B 0B (строго 4 байта).
func parseAGMBKVoltage(payload []byte) (agmBKReading, error) {
	if len(payload) != 4 {
		return agmBKReading{}, fmt.Errorf("%w: payload %d байт", errAGMBKFrame, len(payload))
	}
	cv := binary.LittleEndian.Uint16(payload[0:2])
	return agmBKReading{VoltageV: float64(cv) / 100, FlagsA: payload[2], FlagsB: payload[3]}, nil
}

// parseAGMBKSerial разбирает payload ответа 42 01 (18 ASCII).
func parseAGMBKSerial(payload []byte) (string, error) {
	if len(payload) != 18 {
		return "", fmt.Errorf("%w: серийник %d байт", errAGMBKFrame, len(payload))
	}
	for _, b := range payload {
		if b < 0x20 || b > 0x7E {
			return "", fmt.Errorf("%w: серийник не ASCII", errAGMBKFrame)
		}
	}
	s := strings.TrimSpace(string(payload))
	if s == "" {
		return "", fmt.Errorf("%w: пустой серийник", errAGMBKFrame)
	}
	return s, nil
}
