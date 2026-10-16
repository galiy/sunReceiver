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

// Схема ПГ модуля DDS238: обособленные таблицы dds238_averages_r (5-мин средние)
// и dds238_daily_tariffs (посуточные тарифные границы). Пишутся/читаются только
// для DDS238; общая averages для счётчика больше не используется (старые строки
// удаляются миграцией).
// (Схема DDS238 — партиционированная dds238_averages_r, см. avg_r.go.)

// insertDds238AveragedExec пишет одну 5-минутную усреднённую точку DDS238 в
// партиционированную колоночную таблицу dds238_averages_r.
func insertDds238AveragedExec(q pgExecer, ctx context.Context, name string, ts time.Time, deviceSN string, vc valuesContract) error {
	return insertAvgRow(q, ctx, avgSpecByTable("sunreceiver.dds238_averages_r"), name, ts,
		map[string]string{"device_sn": deviceSN}, vc)
}

// Dds238Averages возвращает 5-минутные усреднённые точки DDS238 за [start, end]
// (ip = meterDeviceKey).
func (s *pgStore) Dds238Averages(start, end time.Time) ([]deviceSnapshot, error) {
	out, err := s.selectAvgRows(avgSpecByTable("sunreceiver.dds238_averages_r"), "", start, end)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].IP = meterDeviceKey
	}
	return out, nil
}
