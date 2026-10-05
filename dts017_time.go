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
	"fmt"
	"time"
)

// Синхронизация времени счётчика DTS017M.
//
// Регистр времени — 0x0210, 3 регистра = 6 байт BCD `ss mm hh DD MM YY` (год —
// 2 цифры, 20YY). Чтение — функцией 04 (как остальная телеметрия), запись —
// функцией 10 (проверено на устройстве, см. PROTOCOL.md §5). Раз в
// dts017TimeCheckInterval проверяем время счётчика; при расхождении с локальным
// временем опрашивающего хоста более dts017TimeTolerance — корректируем.
const (
	dts017RegTime      = 0x0210
	dts017TimeCount    = 3
	dts017TimeCheck    = 30 * time.Minute
	dts017TimeTol      = 2 * time.Second
	dts017BCDYearShift = 2000
)

// dts017BCDByte декодирует один BCD-байт (оба ниббла ≤ 9).
func dts017BCDByte(b byte) (int, bool) {
	hi, lo := b>>4, b&0x0F
	if hi > 9 || lo > 9 {
		return 0, false
	}
	return int(hi)*10 + int(lo), true
}

// dts017BCDEncodeByte кодирует значение 0..99 в BCD-байт.
func dts017BCDEncodeByte(v int) byte {
	return byte((v/10)<<4 | (v % 10))
}

// dts017BcdDecode разбирает 3 регистра (6 байт `ss mm hh DD MM YY`) в локальное
// время. Возвращает ok=false при не-BCD байтах или недопустимых полях (без
// нормализации time.Date).
func dts017BcdDecode(regs []uint16) (time.Time, bool) {
	if len(regs) < dts017TimeCount {
		return time.Time{}, false
	}
	raw := []byte{
		byte(regs[0] >> 8), byte(regs[0]),
		byte(regs[1] >> 8), byte(regs[1]),
		byte(regs[2] >> 8), byte(regs[2]),
	}
	var f [6]int
	for i, b := range raw {
		v, ok := dts017BCDByte(b)
		if !ok {
			return time.Time{}, false
		}
		f[i] = v
	}
	ss, mm, hh, dd, mo, yy := f[0], f[1], f[2], f[3], f[4], f[5]
	if ss > 59 || mm > 59 || hh > 23 || dd < 1 || dd > 31 || mo < 1 || mo > 12 {
		return time.Time{}, false
	}
	t := time.Date(dts017BCDYearShift+yy, time.Month(mo), dd, hh, mm, ss, 0, time.Local)
	// time.Date нормализует недопустимые даты (напр. 31 февраля) — отклоняем.
	if t.Day() != dd || int(t.Month()) != mo || t.Year() != dts017BCDYearShift+yy {
		return time.Time{}, false
	}
	return t, true
}

// dts017BcdEncode кодирует локальное время t в 3 регистра (`ss mm hh DD MM YY`).
func dts017BcdEncode(t time.Time) []uint16 {
	t = t.In(time.Local)
	ss, mm, hh := t.Second(), t.Minute(), t.Hour()
	dd, mo, yy := t.Day(), int(t.Month()), t.Year()%100
	return []uint16{
		uint16(dts017BCDEncodeByte(ss))<<8 | uint16(dts017BCDEncodeByte(mm)),
		uint16(dts017BCDEncodeByte(hh))<<8 | uint16(dts017BCDEncodeByte(dd)),
		uint16(dts017BCDEncodeByte(mo))<<8 | uint16(dts017BCDEncodeByte(yy)),
	}
}

// checkAndSyncDts017Time читает время счётчика и, если оно расходится с now более
// чем на dts017TimeTol, записывает теперь текущее локальное время. Возвращает
// показанное счётчиком время, факт коррекции и ошибку.
func checkAndSyncDts017Time(ctx context.Context, client *meterClient, now time.Time) (meterTime time.Time, corrected bool, err error) {
	regs, err := client.ReadHoldingRegisters(ctx, dts017RegTime, dts017TimeCount)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("чтение времени: %w", err)
	}
	mt, ok := dts017BcdDecode(regs)
	if !ok {
		return time.Time{}, false, fmt.Errorf("некорректный BCD времени: %04X %04X %04X", regs[0], regs[1], regs[2])
	}
	diff := now.Sub(mt)
	if diff < 0 {
		diff = -diff
	}
	if diff <= dts017TimeTol {
		return mt, false, nil
	}
	if err := client.WriteMultipleRegisters(ctx, dts017RegTime, dts017BcdEncode(now)); err != nil {
		return mt, false, fmt.Errorf("запись времени (расхождение %s): %w", diff.Round(time.Millisecond), err)
	}
	return mt, true, nil
}
