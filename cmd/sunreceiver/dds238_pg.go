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
	"encoding/json"
	"fmt"
	"time"
)

// Схема ПГ модуля DDS238: обособленные таблицы dds238_averages (5-мин средние)
// и dds238_daily_tariffs (посуточные тарифные границы). Пишутся/читаются только
// для DDS238; общая averages для счётчика больше не используется (старые строки
// удаляются миграцией).
func ensureDds238Schema(s *pgStore) error {
	if _, err := s.pool.Exec(s.ctx, `
CREATE TABLE IF NOT EXISTS sunreceiver.dds238_averages (
	ip        text        NOT NULL,
	name      text        NOT NULL,
	ts        timestamptz NOT NULL,
	device_sn text        NOT NULL DEFAULT '',
	values    jsonb       NOT NULL DEFAULT '{}'::jsonb,
	PRIMARY KEY (ip, ts)
);
CREATE INDEX IF NOT EXISTS dds238_averages_ts_idx ON sunreceiver.dds238_averages (ts);`); err != nil {
		return fmt.Errorf("pg dds238_averages schema: %w", err)
	}
	return nil
}

// insertDds238AveragedExec пишет одну 5-минутную усреднённую точку DDS238.
func insertDds238AveragedExec(q pgExecer, ctx context.Context, ip, name string, ts time.Time, deviceSN string, vc valuesContract) error {
	vals, err := json.Marshal(vc)
	if err != nil {
		return fmt.Errorf("pg marshal dds238 avg: %w", err)
	}
	_, err = q.Exec(ctx, `
INSERT INTO sunreceiver.dds238_averages (ip, name, ts, device_sn, values)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (ip, ts) DO UPDATE SET name = EXCLUDED.name, device_sn = EXCLUDED.device_sn, values = EXCLUDED.values`,
		ip, name, ts.UTC(), deviceSN, vals)
	if err != nil {
		return fmt.Errorf("pg insert dds238 avg %s: %w", ip, err)
	}
	return nil
}

// Dds238Averages возвращает 5-минутные усреднённые точки DDS238 за [start, end]
// (ip = meterDeviceKey).
func (s *pgStore) Dds238Averages(start, end time.Time) ([]deviceSnapshot, error) {
	rows, err := s.pool.Query(s.ctx, `
SELECT ip, name, ts, device_sn, values
FROM sunreceiver.dds238_averages
WHERE ts >= $1 AND ts <= $2
ORDER BY ts ASC`, start.UTC(), end.UTC())
	if err != nil {
		return nil, fmt.Errorf("pg query dds238 averages: %w", err)
	}
	defer rows.Close()
	out := []deviceSnapshot{}
	for rows.Next() {
		var ip, name, deviceSN string
		var ts time.Time
		var vals json.RawMessage
		if err := rows.Scan(&ip, &name, &ts, &deviceSN, &vals); err != nil {
			return nil, fmt.Errorf("pg scan dds238 avg: %w", err)
		}
		var vc valuesContract
		if len(vals) > 0 {
			if err := json.Unmarshal(vals, &vc); err != nil {
				continue
			}
		}
		out = append(out, deviceSnapshot{
			Name: name, IP: ip, DeviceSN: deviceSN,
			Timestamp: ts.Format(time.RFC3339), Values: vc,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg rows dds238 avg: %w", err)
	}
	return out, nil
}
