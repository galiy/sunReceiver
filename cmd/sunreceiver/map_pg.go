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
	"time"
)

// Схема PG модуля МАП: обособленная таблица map_averages_r (5-мин средние).
// Пишется/читается только для МАП; общая averages для МАП больше не используется
// (старые строки удаляются миграцией).
// (Схема МАП — партиционированная map_averages_r, см. avg_r.go.)

// insertMapAveragedExec пишет одну 5-минутную усреднённую точку МАП в map_averages_r.
func insertMapAveragedExec(q pgExecer, ctx context.Context, name string, ts time.Time, deviceSN string, vc valuesContract) error {
	return insertAvgRow(q, ctx, avgSpecByTable("sunreceiver.map_averages_r"), name, ts,
		map[string]string{"device_sn": deviceSN}, vc)
}

// MapAverages возвращает 5-минутные усреднённые точки МАП за [start, end].
func (s *pgStore) MapAverages(start, end time.Time) ([]deviceSnapshot, error) {
	out, err := s.selectAvgRows(avgSpecByTable("sunreceiver.map_averages_r"), "", start, end)
	if err != nil {
		return nil, err
	}
	// Пропускаем строки, у которых все метрики NULL (пустые заглушки в PG):
	// иначе они попадают в карту с пустым Values и засоряют current/TS.
	filtered := out[:0]
	for _, sn := range out {
		if len(sn.Values) == 0 {
			continue
		}
		sn.IP = mapDevKey
		filtered = append(filtered, sn)
	}
	return filtered, nil
}
