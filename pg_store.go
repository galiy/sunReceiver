package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// pgStore — persistent-хранилище снимков в PostgreSQL. Служит долговременным
// архивированием данных, которые Redis хранит in-memory: при запуске (пустом
// Redis) данные из PG восстанавливаются обратно в Redis.
type pgStore struct {
	pool *pgxpool.Pool
	ctx  context.Context
}

// openPG открывает пул соединений PostgreSQL и применяет схему.
func openPG(dsn string) (*pgStore, error) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("pg pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pg ping: %w", err)
	}
	s := &pgStore{pool: pool, ctx: ctx}
	if err := s.ensureSchema(); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

// ensureSchema создаёт схему и таблицу снимков (идемпотентно).
func (s *pgStore) ensureSchema() error {
	_, err := s.pool.Exec(s.ctx, `
CREATE SCHEMA IF NOT EXISTS sunreceiver;
CREATE TABLE IF NOT EXISTS sunreceiver.snapshots (
	ip        text        NOT NULL,
	name      text        NOT NULL,
	ts        timestamptz NOT NULL,
	device_sn text        NOT NULL DEFAULT '',
	values    jsonb       NOT NULL DEFAULT '{}'::jsonb,
	PRIMARY KEY (ip, ts)
);
CREATE INDEX IF NOT EXISTS snapshots_ts_idx ON sunreceiver.snapshots (ts);
`)
	if err != nil {
		return fmt.Errorf("pg schema: %w", err)
	}
	return nil
}

// Insert сохраняет снимок в PG. Idempotентен по (ip, ts): повторная запись того же
// времени игнорируется (ON CONFLICT DO NOTHING).
func (s *pgStore) Insert(snap deviceSnapshot, ts time.Time) error {
	vals, err := json.Marshal(snap.Values)
	if err != nil {
		return fmt.Errorf("pg marshal values %s: %w", snap.IP, err)
	}
	_, err = s.pool.Exec(s.ctx, `
INSERT INTO sunreceiver.snapshots (ip, name, ts, device_sn, values)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (ip, ts) DO NOTHING`,
		snap.IP, snap.Name, ts.UTC(), snap.DeviceSN, vals)
	if err != nil {
		return fmt.Errorf("pg insert %s: %w", snap.IP, err)
	}
	return nil
}

// Close закрывает пул соединений.
func (s *pgStore) Close() {
	if s != nil && s.pool != nil {
		s.pool.Close()
	}
}

// Snapshot возвращает снимки за период [start, end] включительно, отсортированные
// по времени. Используется для восстановления Redis из persistent-хранилища.
func (s *pgStore) Snapshots(start, end time.Time) ([]deviceSnapshot, error) {
	rows, err := s.pool.Query(s.ctx, `
SELECT ip, name, ts, device_sn, values
FROM sunreceiver.snapshots
WHERE ts >= $1 AND ts <= $2
ORDER BY ts`, start.UTC(), end.UTC())
	if err != nil {
		return nil, fmt.Errorf("pg query snapshots: %w", err)
	}
	defer rows.Close()

	snaps := []deviceSnapshot{}
	for rows.Next() {
		var ip, name, deviceSN string
		var ts time.Time
		var vals json.RawMessage
		if err := rows.Scan(&ip, &name, &ts, &deviceSN, &vals); err != nil {
			return nil, fmt.Errorf("pg scan: %w", err)
		}
		var vc valuesContract
		if len(vals) > 0 {
			if err := json.Unmarshal(vals, &vc); err != nil {
				continue
			}
		}
		snaps = append(snaps, deviceSnapshot{
			Name:      name,
			IP:        ip,
			Timestamp: ts.Format(time.RFC3339),
			DeviceSN:  deviceSN,
			Values:    vc,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg rows: %w", err)
	}
	return snaps, nil
}