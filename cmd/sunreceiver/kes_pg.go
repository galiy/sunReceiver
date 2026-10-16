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

// Схема PG модуля КЭС: обособленная таблица kes_averages_r (5-мин средние).
// Без колонки ip; PK (name, ts). Ключ устройства восстанавливается из name
// (`MPPT-<serial>` → `mppt-<serial>`).
// (Схема КЭС — партиционированная kes_averages_r, см. avg_r.go.)

// insertKesAveragedExec пишет одну 5-минутную усреднённую точку КЭС в kes_averages_r.
func insertKesAveragedExec(q pgExecer, ctx context.Context, name string, ts time.Time, deviceSN string, vc valuesContract) error {
	return insertAvgRow(q, ctx, avgSpecByTable("sunreceiver.kes_averages_r"), name, ts,
		map[string]string{"device_sn": deviceSN}, vc)
}

// KesAverages возвращает 5-минутные усреднённые точки КЭС за [start, end]
// (ip восстанавливается в ключ `mppt-<serial>`).
func (s *pgStore) KesAverages(start, end time.Time) ([]deviceSnapshot, error) {
	out, err := s.selectAvgRows(avgSpecByTable("sunreceiver.kes_averages_r"), "", start, end)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].IP = kesKeyByName(out[i].Name)
	}
	return out, nil
}
