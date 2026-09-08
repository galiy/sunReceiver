package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// pgStore — persistent-хранилище исторических данных в PostgreSQL.
//
// Хранит ТОЛЬКО усреднённые 5-минутные точки (avgStep): сырые 10-секундные
// снимки живут в Redis (за последние 2 календарных суток), а в PG пишутся
// накопленные за каждые 5 минут средние (см. accumulator.go). Данные старше
// двух календарных суток хранятся в PG вечно и читаются дашбордом, когда
// запрошенный период выходит за окно удержания Redis.
type pgStore struct {
	pool *pgxpool.Pool
	ctx  context.Context
}

// openPG открывает пул соединений PostgreSQL и применяет схему + миграцию.
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

// ensureSchema создаёт схему и таблицу усреднённых точек (идемпотентно).
func (s *pgStore) ensureSchema() error {
	_, err := s.pool.Exec(s.ctx, `
CREATE SCHEMA IF NOT EXISTS sunreceiver;
CREATE TABLE IF NOT EXISTS sunreceiver.averages (
	ip        text        NOT NULL,
	name      text        NOT NULL,
	ts        timestamptz NOT NULL,
	device_sn text        NOT NULL DEFAULT '',
	values    jsonb       NOT NULL DEFAULT '{}'::jsonb,
	PRIMARY KEY (ip, ts)
);
CREATE INDEX IF NOT EXISTS averages_ts_idx ON sunreceiver.averages (ts);
`)
	if err != nil {
		return fmt.Errorf("pg schema: %w", err)
	}
	// Таблица посуточных тарифных значений электросчётчика DDS238.
	if err := ensureMeterTariffSchema(s); err != nil {
		return fmt.Errorf("pg meter tariff schema: %w", err)
	}
	return nil
}

// InsertAveraged сохраняет одну усреднённую за 5 минут точку (ts — начало
// промежутка). Idempотентна по (ip, ts): повторная запись игнорируется.
func (s *pgStore) InsertAveraged(ip, name string, ts time.Time, deviceSN string, vc valuesContract) error {
	vals, err := json.Marshal(vc)
	if err != nil {
		return fmt.Errorf("pg marshal values %s: %w", ip, err)
	}
	_, err = s.pool.Exec(s.ctx, `
INSERT INTO sunreceiver.averages (ip, name, ts, device_sn, values)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (ip, ts) DO NOTHING`,
		ip, name, ts.UTC(), deviceSN, vals)
	if err != nil {
		return fmt.Errorf("pg insert avg %s: %w", ip, err)
	}
	return nil
}

// Averages возвращает усреднённые точки за период [start, end] включительно,
// отсортированные по времени. ts точек — начало соответствующего 5-минутного
// промежутка.
func (s *pgStore) Averages(start, end time.Time) ([]deviceSnapshot, error) {
	rows, err := s.pool.Query(s.ctx, `
SELECT ip, name, ts, device_sn, values
FROM sunreceiver.averages
WHERE ts >= $1 AND ts <= $2
ORDER BY ts`, start.UTC(), end.UTC())
	if err != nil {
		return nil, fmt.Errorf("pg query averages: %w", err)
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

// MigrateLegacy конвертирует старую таблицу сырых снимков snapshots в
// 5-минутные усреднённые точки таблицы averages, после чего удаляет snapshots.
// Идемпотентна: вторая попытка находит, что таблицы уже нет, и бездействует.
func (s *pgStore) MigrateLegacy() error {
	var exists bool
	if err := s.pool.QueryRow(s.ctx, `
SELECT EXISTS (
	SELECT 1 FROM information_schema.tables
	WHERE table_schema = 'sunreceiver' AND table_name = 'snapshots'
)`).Scan(&exists); err != nil {
		return fmt.Errorf("pg legacy exists: %w", err)
	}
	if !exists {
		log.Println("pg legacy: таблица sunreceiver.snapshots не найдена — миграция не требуется")
		return nil
	}

	var n int
	if err := s.pool.QueryRow(s.ctx, `SELECT count(*) FROM sunreceiver.snapshots`).Scan(&n); err != nil {
		return fmt.Errorf("pg legacy count: %w", err)
	}
	if n == 0 {
		return s.dropLegacy()
	}
	log.Printf("pg legacy: конвертирую %d сырых снимков snapshots в 5-минутные усреднённые точки", n)

	rows, err := s.pool.Query(s.ctx, `
SELECT ip, name, ts, device_sn, values
FROM sunreceiver.snapshots
ORDER BY ts`)
	if err != nil {
		return fmt.Errorf("pg legacy query: %w", err)
	}
	defer rows.Close()

	// Группируем сырые снимки по (ip, 5-минутный промежуток); ключ = "ip|unixBucket".
	groups := map[string][]deviceSnapshot{}
	for rows.Next() {
		var ip, name, deviceSN string
		var ts time.Time
		var vals json.RawMessage
		if err := rows.Scan(&ip, &name, &ts, &deviceSN, &vals); err != nil {
			return fmt.Errorf("pg legacy scan: %w", err)
		}
		var vc valuesContract
		if len(vals) > 0 {
			if err := json.Unmarshal(vals, &vc); err != nil {
				continue
			}
		}
		bucket := floorToStep(ts)
		key := fmt.Sprintf("%s|%d", ip, bucket.Unix())
		groups[key] = append(groups[key], deviceSnapshot{
			Name:      name,
			IP:        ip,
			Timestamp: ts.Format(time.RFC3339),
			DeviceSN:  deviceSN,
			Values:    vc,
		})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("pg legacy rows: %w", err)
	}

	var inserted int
	for key, group := range groups {
		ip := strings.SplitN(key, "|", 2)[0]
		first := group[0]
		bucket := floorToStep(parseTS(group[0].Timestamp))
		vc := averageValues(group)
		if len(vc) == 0 {
			continue
		}
		if err := s.InsertAveraged(ip, first.Name, bucket, first.DeviceSN, vc); err != nil {
			log.Printf("pg legacy insert %s: %v", ip, err)
			continue
		}
		inserted++
	}
	log.Printf("pg legacy: усреднённых точек записано: %d", inserted)

	return s.dropLegacy()
}

// parseTS разбирает timestamp снимка (RFC3339) в time.Time.
func parseTS(s string) time.Time {
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Now()
	}
	return ts
}

// dropLegacy удаляет старую сырую таблицу snapshots.
func (s *pgStore) dropLegacy() error {
	if _, err := s.pool.Exec(s.ctx, `DROP TABLE IF EXISTS sunreceiver.snapshots`); err != nil {
		return fmt.Errorf("pg drop snapshots: %w", err)
	}
	log.Println("pg legacy: таблица sunreceiver.snapshots удалена")
	return nil
}

// Close закрывает пул соединений.
func (s *pgStore) Close() {
	if s != nil && s.pool != nil {
		s.pool.Close()
	}
}