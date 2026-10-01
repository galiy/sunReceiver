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
	"errors"
	"log"
	"time"
)

// Пулер ОШИБОК МАП (Титанатор). Отдельный цикл, читает только сырые ячейки-ошибки
// МАП и пишет появление ошибок в PG device_errors. Телеметрию не трогает.
//
// Источник сырых значений — по выбранному в конфиге способу мониторинга:
//   - Modbus (map.rs485.disabled=false) — через mapgateway/Modbus TCP;
//   - веб-API ПАК «Малина» (true) — read_memory.php?offset=<ячейка>&count=<n>.
//
// Разметка — из protocol_MAP_cells_2026_07_15.doc (SVEN POWER MANAGER II).
const mapErrPollInterval = 30 * time.Second

type mapErrBit struct {
	mask byte
	text string
}

type mapErrCell struct {
	addr uint16
	name string
	bits []mapErrBit
}

// mapErrCells — ячейки-ошибки МАП и расшифровка битов.
var mapErrCells = []mapErrCell{
	{0x42A, "_RSErrSis", []mapErrBit{
		{2, "МАП: ошибка записи в EEPROM"},
		{4, "МАП: системное прерывание"},
		{8, "МАП: не работает датчик температуры"},
		{0x10, "МАП: другая системная ошибка"},
		{0x20, "МАП: несоответствие перемычек"},
		{0x40, "МАП: неверная полярность тора"},
		{0x80, "МАП: не работает часовой кварц 32768"},
	}},
	{0x42B, "_RSErrJobM", []mapErrBit{
		{1, "АКБ: критически низкое напряжение"},
		{2, "АКБ: превышение напряжения"},
		{4, "АКБ: КЗ при заряде"},
		{8, "АКБ: КЗ"},
		{0x10, "Сеть: возможно залипло основное реле"},
		{0x20, "Сеть: КЗ 220В"},
		{0x40, "Выход: постороннее напряжение"},
		{0x80, "МАП: сброс программы (помеха)"},
	}},
	{0x42C, "_RSErrJob", []mapErrBit{
		{1, "АКБ разряжен (отключение через ~1 мин)"},
		{2, "Перегрузка по АКБ"},
		{4, "Нагрузка выше номинальной мощности"},
		{8, "Перегрев (пауза генерации/заряда)"},
		{0x10, "Вентилятор не работает"},
		{0x20, "Топливный генератор не запускается"},
		{0x40, "Сбой режима работы"},
		{0x80, "Многократные КЗ по заряду"},
	}},
	{0x42D, "_RSWarning", []mapErrBit{
		{1, "Кнопка: нет действия (можно игнорировать)"},
		{2, "Сеть: напряжение вышло за пределы"},
		{4, "Выход: постороннее напряжение/выбросы"},
		{8, "Сеть: выбросы напряжения по входу"},
		{0x10, "Кнопка залипла"},
		{0x20, "Нет сети для перехода на заряд"},
		{0x40, "Нагрузка выше макс. мощности"},
		{0x80, "Сеть нестабильна"},
	}},
	{0x41C, "_F_AccOver", []mapErrBit{
		{1, "Отключение по перегрузке по току АКБ"},
		{2, "Отключение по перегрузке по току АКБ при заряде"},
		{4, "Отключение по критически низкому напряжению АКБ"},
		{8, "Отключение по неисправности вентилятора"},
		{0x10, "Отключение по мощности выше номинальной (>30 мин)"},
		{0x20, "Отключение по полному разряду АКБ"},
		{0x40, "Отключение по выходу за пределы датчика температуры"},
		{0x80, "Полное отключение по многократным перегрузкам по АКБ"},
	}},
	{0x41D, "_F_NETOver", []mapErrBit{
		{1, "Отключение генерации: постороннее напряжение на выходе/реле"},
		{2, "Переход на генерацию: перегрузка по сети"},
		{8, "Выключение по многократным перегрузкам по сети"},
	}},
	{0x448, "_I2C_Err", []mapErrBit{ // адрес не подтверждён (вероятно 0x448)
		{1, "I2C: ошибка подтверждения (Ack)"},
		{2, "I2C: ошибка контрольной суммы"},
		{4, "I2C: ошибка размера данных"},
		{8, "I2C: ошибка протокола"},
	}},
	{0x447, "_RSErrDop", []mapErrBit{
		{1, "3ф: нет связи с предыдущей фазой"},
		{2, "3ф: выход за окно синхронизации"},
		{4, "Ошибка I2C BMS"},
		{8, "Ошибка I2C MPPT"},
		{0x10, "Устаревшее ПО MPPT (<4.0)"},
		{0x20, "Ошибка синхронизации параллельных МАП"},
		{0x40, "Ведомый МАП без сети"},
	}},
}

// decodeMapErrors возвращает список активных ошибок МАП по сырым ячейкам.
func decodeMapErrors(cells map[uint16]byte) []string {
	var out []string
	for _, c := range mapErrCells {
		v := cells[c.addr]
		if v == 0 {
			continue
		}
		for _, b := range c.bits {
			if v&b.mask != 0 {
				out = append(out, b.text)
			}
		}
	}
	return out
}

var errNoMapErrSource = errors.New("источник МАП не задан")

// readMapErrCells читает сырые ячейки ошибок МАП. t != nil — Modbus-путь;
// иначе веб-API (mppt).
func readMapErrCells(ctx context.Context, t *invTarget) (map[uint16]byte, error) {
	cells := map[uint16]byte{}
	if t != nil {
		mc := mapClientFor(t.IP, t.Unit)
		// ReadRegisters(start,count) читает count слов и возвращает 2*count байт,
		// которые соответствуют байт-ячейкам start..start+2*count-1.
		// Один блок 0x41C..0x449 покрывает _F_AccOver(0x41C), _F_NETOver(0x41D),
		// _RSErrSis/JobM/Job/Warning(0x42A..0x42D), _RSErrDop(0x447), _I2C_Err(0x448).
		if b, err := mc.ReadRegisters(ctx, 0x041C, 0x17); err != nil {
			return nil, err
		} else {
			for i := 0; i < len(b); i++ {
				cells[0x041C+uint16(i)] = b[i]
			}
		}
		return cells, nil
	}
	if mppt == nil {
		return nil, errNoMapErrSource
	}
	m1, err := mppt.FetchMemory(ctx, 0x041C, 0x2E)
	if err != nil {
		return nil, err
	}
	for k, v := range m1 {
		if k >= 0x41C && k <= 0x449 {
			cells[uint16(k)] = byte(v)
		}
	}
	return cells, nil
}

// runMapErrorPoll — отдельный цикл опроса ошибок МАП. Не чаще 1 раза в 30 с.
// Пишет появление ошибок в PG device_errors (device="map", kind="map"); дубли на
// каждом чтении не создаются.
func runMapErrorPoll(pg *pgStore, t *invTarget, ctx context.Context) {
	if pg == nil {
		return
	}
	last := map[string]bool{}
	poll := func() {
		cells, err := readMapErrCells(ctx, t)
		if err != nil {
			log.Printf("map errors: %v", err)
			return
		}
		cur := map[string]bool{}
		for _, a := range decodeMapErrors(cells) {
			cur[a] = true
		}
		now := time.Now()
		for a := range cur {
			if !last[a] {
				if e := pg.InsertDeviceError("map", "map", a, a, now); e != nil {
					log.Printf("map errors pg: %v", e)
				}
			}
		}
		last = cur
	}
	poll()
	ticker := time.NewTicker(mapErrPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			poll()
		case <-ctx.Done():
			return
		}
	}
}
