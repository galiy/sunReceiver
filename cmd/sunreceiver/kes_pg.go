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

// Схема PG модуля КЭС: обособленная таблица kes_averages (5-мин средние).
// Без колонки ip; PK (name, ts). Ключ устройства восстанавливается из name
// (`MPPT-<serial>` → `mppt-<serial>`).
func ensureKesSchema(s *pgStore) error {
	if _, err := s.pool.Exec(s.ctx, `
CREATE TABLE IF NOT EXISTS sunreceiver.kes_averages (
	name      text        NOT NULL,
	ts        timestamptz NOT NULL,
	device_sn text        NOT NULL DEFAULT '',
	values    jsonb       NOT NULL DEFAULT '{}'::jsonb,
	PRIMARY KEY (name, ts)
);
CREATE INDEX IF NOT EXISTS kes_averages_ts_idx ON sunreceiver.kes_averages (ts);`); err != nil {
		return fmt.Errorf("pg kes_averages schema: %w", err)
	}
	return nil
}

// insertKesAveragedExec пишет одну 5-минутную усреднённую точку КЭС.
func insertKesAveragedExec(q pgExecer, ctx context.Context, name string, ts time.Time, deviceSN string, vc valuesContract) error {
	vals, err := json.Marshal(vc)
	if err != nil {
		return fmt.Errorf("pg marshal kes avg: %w", err)
	}
	_, err = q.Exec(ctx, `
INSERT INTO sunreceiver.kes_averages (name, ts, device_sn, values)
VALUES ($1, $2, $3, $4)
ON CONFLICT (name, ts) DO UPDATE SET device_sn = EXCLUDED.device_sn, values = EXCLUDED.values`,
		name, ts.UTC(), deviceSN, vals)
	if err != nil {
		return fmt.Errorf("pg insert kes avg %s: %w", name, err)
	}
	return nil
}

// KesAverages возвращает 5-минутные усреднённые точки КЭС за [start, end]
// (ip восстанавливается в ключ `mppt-<serial>`).
func (s *pgStore) KesAverages(start, end time.Time) ([]deviceSnapshot, error) {
	rows, err := s.pool.Query(s.ctx, `
SELECT name, ts, device_sn, values
FROM sunreceiver.kes_averages
WHERE ts >= $1 AND ts <= $2
ORDER BY ts ASC`, start.UTC(), end.UTC())
	if err != nil {
		return nil, fmt.Errorf("pg query kes averages: %w", err)
	}
	defer rows.Close()
	out := []deviceSnapshot{}
	for rows.Next() {
		var name, deviceSN string
		var ts time.Time
		var vals json.RawMessage
		if err := rows.Scan(&name, &ts, &deviceSN, &vals); err != nil {
			return nil, fmt.Errorf("pg scan kes avg: %w", err)
		}
		var vc valuesContract
		if len(vals) > 0 {
			if err := json.Unmarshal(vals, &vc); err != nil {
				continue
			}
		}
		out = append(out, deviceSnapshot{
			Name: name, IP: kesKeyByName(name), DeviceSN: deviceSN,
			Timestamp: ts.Format(time.RFC3339), Values: vc,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg rows kes avg: %w", err)
	}
	return out, nil
}
