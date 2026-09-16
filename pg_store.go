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
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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

// pgStatementTimeout — серверный лимит на одно SQL-выражение для всех
// соединений пула: «повисший» PG (черная дыра) не должен копить зависшие
// горутины/соединения (каждый averageBucket — своя горутина).
const pgStatementTimeout = "15s"

// openPG открывает пул соединений PostgreSQL и применяет схему + миграцию.
func openPG(dsn string) (*pgStore, error) {
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("pg config: %w", err)
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET statement_timeout = '"+pgStatementTimeout+"'")
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("pg pool: %w", err)
	}
	// Ограничение по времени на Ping: мёртвый/фильтруемый хост не должен
	// блокировать старт на время TCP-ретрансмитов (~2 мин).
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err = pool.Ping(pingCtx)
	cancel()
	if err != nil {
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
CREATE INDEX IF NOT EXISTS averages_ts_ip_idx ON sunreceiver.averages (ts, ip);
`)
	if err != nil {
		return fmt.Errorf("pg schema: %w", err)
	}
	// Таблица посуточных тарифных значений электросчётчика DDS238.
	if err := ensureMeterTariffSchema(s); err != nil {
		return fmt.Errorf("pg meter tariff schema: %w", err)
	}
	// Таблица 5-минутных усреднённых точек ANT BMS (name = deviceName).
	// PK (name, ts) — эффективная выборка «конкретная BMS за диапазон времени»
	// (узкий индексный range-scan по первичному ключу).
	if _, err := s.pool.Exec(s.ctx, `
CREATE TABLE IF NOT EXISTS sunreceiver.bms_averages (
	name   text        NOT NULL,
	ts     timestamptz NOT NULL,
	values jsonb       NOT NULL DEFAULT '{}'::jsonb,
	PRIMARY KEY (name, ts)
);`); err != nil {
		return fmt.Errorf("pg bms_averages schema: %w", err)
	}
	return nil
}

// pgExecer — общее для *pgxpool.Pool и pgx.Tx: Exec с CommandTag. Нужно, чтобы
// метод записи одной точки можно было вызвать и напрямую на пуле, и внутри
// транзакции (см. withTx, insertAverageBucket).
type pgExecer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// pgRetryBackoff — паузы между повторными попытками транзакционной записи бакета
// (см. retryPg): кратковременный сбой PG при 3 попытках (100/500/1000 мс) не
// оставляет «дыру» в ряде.
var pgRetryBackoff = [...]time.Duration{100 * time.Millisecond, 500 * time.Millisecond, time.Second}

// retryPg выполняет fn с повторными попытками (attempts раз), между ними — пауза
// из pgRetryBackoff. Применяется к транзакционной записи 5-минутного бакета:
// одноразовая горутина бакета не переписывается позже, поэтому одиночный сбой PG
// должен быть пережит повторной попыткой.
func retryPg(fn func() error, attempts int) error {
	var err error
	for i := 0; i < attempts; i++ {
		if err = fn(); err == nil {
			return nil
		}
		if i == attempts-1 {
			break
		}
		d := pgRetryBackoff[i]
		if i >= len(pgRetryBackoff) {
			d = pgRetryBackoff[len(pgRetryBackoff)-1]
		}
		time.Sleep(d)
	}
	return err
}

// withTx выполняет fn на одной транзакции пула и коммитит её: весь набор точек
// одного бакета (или одного вызова для BMS) записывается атомарно — либо все,
// либо ничего. При ошибке транзакция откатывается.
func (s *pgStore) withTx(fn func(q pgExecer) error) error {
	tx, err := s.pool.Begin(s.ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(s.ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(s.ctx)
}

// InsertAveraged сохраняет одну усреднённую за 5 минут точку (ts — начало
// промежутка). Идемпотентна по (ip, ts), но повторная запись ОБНОВЛЯЕТ строку:
// поздняя запись того же промежутка побеждает (last-write-wins), как в
// InsertBMSAveraged. Нужно как страховка при перезапуске/пересчёте: backfill
// при старте не трогает незавершённый бакет (строго [start, end)), так что
// нормального перекрытия backfill↔живой цикл нет, но если такое перекрытие
// возникнет — более полная (поздняя) запись переопределит раннюю частичную.
func (s *pgStore) InsertAveraged(ip, name string, ts time.Time, deviceSN string, vc valuesContract) error {
	return insertAveragedExec(s.pool, s.ctx, ip, name, ts, deviceSN, vc)
}

// insertAveragedExec — реализация вставки одной усреднённой точки в
// sunreceiver.averages; вызывается и напрямую на пуле (InsertAveraged), и внутри
// транзакции (insertAverageBucket). Идемпотентна по (ip, ts).
func insertAveragedExec(q pgExecer, ctx context.Context, ip, name string, ts time.Time, deviceSN string, vc valuesContract) error {
	vals, err := json.Marshal(vc)
	if err != nil {
		return fmt.Errorf("pg marshal values %s: %w", ip, err)
	}
	_, err = q.Exec(ctx, `
INSERT INTO sunreceiver.averages (ip, name, ts, device_sn, values)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (ip, ts) DO UPDATE
  SET name = EXCLUDED.name,
      device_sn = EXCLUDED.device_sn,
      values = EXCLUDED.values`,
		ip, name, ts.UTC(), deviceSN, vals)
	if err != nil {
		return fmt.Errorf("pg insert avg %s: %w", ip, err)
	}
	return nil
}

// Averages возвращает усреднённые точки за период [start, end] включительно.
// Необязательный фильтр ips ограничивает выборку конкретными устройствами
// (nil или пустой — все устройства). Порядок не гарантируется — потребители
// (дашборд, реставрация Redis) сортируют точки сами.
func (s *pgStore) Averages(start, end time.Time, ips ...string) ([]deviceSnapshot, error) {
	q := `
SELECT ip, name, ts, device_sn, values
FROM sunreceiver.averages
WHERE ts >= $1 AND ts <= $2`
	args := []interface{}{start.UTC(), end.UTC()}
	if len(ips) > 0 {
		q += ` AND ip = ANY($3)`
		args = append(args, ips)
	}
	rows, err := s.pool.Query(s.ctx, q, args...)
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

// InsertBMSAveraged сохраняет одну усреднённую за 5 минут точку BMS
// (ts — начало промежутка). Идемпотентна по (name, ts).
//
// Sample-count guard: повторная запись того же (name, ts) НЕ перетирает более
// полную строку. Effect при рестарте: при остановке посреди промежутка drain
// (partial=true) пишет неполный [start,stop] ТОЛЬКО в Redis и НЕ в PG (см.
// saveBMSClosedBuckets), поэтому в PG уходят лишь полные 5-минутные бакеты. Если
// всё же встречается конкурентное дописывание живого бакета (поздняя запись того
// же (name,ts)), выигрывает запись с БОЛЬШИМ число сэмплов (samples), а не
// «последняя» — так более полное усреднение не заменяется частичным и снимки до
// рестарта не выпадают из итога (у BMS нет backfill, история только живой опрос).
func (s *pgStore) InsertBMSAveraged(name string, ts time.Time, avg bmsAveraged) error {
	return insertBMSAveragedExec(s.pool, s.ctx, name, ts, avg)
}

// insertBMSAveragedExec — реализация вставки одной усреднённой точки BMS в
// sunreceiver.bms_averages; вызывается и напрямую на пуле (InsertBMSAveraged), и
// внутри транзакции (saveBMSClosedBuckets). Where-условие — sample-count guard,
// см. InsertBMSAveraged.
func insertBMSAveragedExec(q pgExecer, ctx context.Context, name string, ts time.Time, avg bmsAveraged) error {
	vals, err := json.Marshal(avg)
	if err != nil {
		return fmt.Errorf("pg marshal bms avg %s: %w", name, err)
	}
	_, err = q.Exec(ctx, `
INSERT INTO sunreceiver.bms_averages (name, ts, values)
VALUES ($1, $2, $3)
ON CONFLICT (name, ts) DO UPDATE SET values = EXCLUDED.values
  WHERE sunreceiver.bms_averages.values->>'samples' IS NULL
     OR (EXCLUDED.values->>'samples')::int > (sunreceiver.bms_averages.values->>'samples')::int`,
		name, ts.UTC(), vals)
	if err != nil {
		return fmt.Errorf("pg insert bms avg %s: %w", name, err)
	}
	return nil
}

// BMSAverages возвращает 5-минутные усреднённые точки одной BMS (по ключу
// bmsKey — name, см. bms_poller.go)
// за период [start, end] включительно, по возрастанию ts. Выборка идёт по
// PK (name, ts) — узкий индексный range-scan по конкретной BMS (эффективно
// для построения графиков за произвольный диапазон времени).
func (s *pgStore) BMSAverages(name string, start, end time.Time) ([]bmsSeriesPoint, error) {
	rows, err := s.pool.Query(s.ctx, `
SELECT ts, values
FROM sunreceiver.bms_averages
WHERE name = $1 AND ts >= $2 AND ts <= $3
ORDER BY ts ASC`, name, start.UTC(), end.UTC())
	if err != nil {
		return nil, fmt.Errorf("pg query bms averages: %w", err)
	}
	defer rows.Close()

	pts := []bmsSeriesPoint{}
	for rows.Next() {
		var ts time.Time
		var vals json.RawMessage
		if err := rows.Scan(&ts, &vals); err != nil {
			return nil, fmt.Errorf("pg scan bms avg: %w", err)
		}
		p := bmsSeriesPoint{Name: name, Ts: ts.Format(time.RFC3339)}
		if len(vals) > 0 {
			if err := json.Unmarshal(vals, &p.bmsAveraged); err != nil {
				continue
			}
		}
		pts = append(pts, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg bms averages rows: %w", err)
	}
	return pts, nil
}

// BMSAveragesAll возвращает 5-минутные усреднённые точки BMS ВСЕХ устройств
// за период [start, end] включительно, по возрастанию ts. Используется для
// реставрации Redis-ряда sunreceiver:bms:series из PG при полностью пустом
// Redis (см. restoreRedisFromPG).
func (s *pgStore) BMSAveragesAll(start, end time.Time) ([]bmsSeriesPoint, error) {
	rows, err := s.pool.Query(s.ctx, `
SELECT name, ts, values
FROM sunreceiver.bms_averages
WHERE ts >= $1 AND ts <= $2
ORDER BY ts ASC`, start.UTC(), end.UTC())
	if err != nil {
		return nil, fmt.Errorf("pg query bms averages all: %w", err)
	}
	defer rows.Close()

	pts := []bmsSeriesPoint{}
	for rows.Next() {
		var name string
		var ts time.Time
		var vals json.RawMessage
		if err := rows.Scan(&name, &ts, &vals); err != nil {
			return nil, fmt.Errorf("pg scan bms avg all: %w", err)
		}
		p := bmsSeriesPoint{Name: name, Ts: ts.Format(time.RFC3339)}
		if len(vals) > 0 {
			if err := json.Unmarshal(vals, &p.bmsAveraged); err != nil {
				continue
			}
		}
		pts = append(pts, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg bms averages all rows: %w", err)
	}
	return pts, nil
}

// DailyTariffsRange возвращает финализированные посуточные тарифы счётчика
// (день/ночь × потребление/отдача, kWh) за период [start, end), отсортированные по
// дню возрастанию. Дни без финализации (finalized IS NULL) пропускаются. start/end
// должны передаваться в локальной зоне (границы суток).
func (s *pgStore) DailyTariffsRange(start, end time.Time) ([]meterDayStat, error) {
	rows, err := s.pool.Query(s.ctx, `
SELECT day, import_day, import_night, export_day, export_night
FROM sunreceiver.daily_tariffs
WHERE finalized IS NOT NULL
  AND import_day IS NOT NULL AND import_night IS NOT NULL
  AND export_day IS NOT NULL AND export_night IS NOT NULL
  AND day >= $1 AND day < $2
ORDER BY day ASC`, start, end)
	if err != nil {
		return nil, fmt.Errorf("pg daily_tariffs range: %w", err)
	}
	defer rows.Close()

	got := []meterDayStat{}
	for rows.Next() {
		var day time.Time
		var st meterDayStat
		if err := rows.Scan(&day, &st.ImportDay, &st.ImportNight, &st.ExportDay, &st.ExportNight); err != nil {
			return nil, fmt.Errorf("pg daily_tariffs range scan: %w", err)
		}
		st.Day = day.Format("2006-01-02")
		got = append(got, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg daily_tariffs range rows: %w", err)
	}
	return got, nil
}

// meterBoundaryRow — фиксированные граничные показания Import/Export (kWh) одного
// календарного дня из daily_tariffs. Поля могут быть nil, если граница ещё не
// захвачена (показание NULL в БД).
type meterBoundaryRow struct {
	Import0000 *float64 // на 00:00 дня
	Import0700 *float64 // на 07:00 дня
	Import2300 *float64 // на 23:00 дня
	ImportNext *float64 // на 00:00 следующего дня (закрытие ночного тарифа дня)
	Export0000 *float64
	Export0700 *float64
	Export2300 *float64
	ExportNext *float64
}

// MeterBoundaryValues возвращает фиксированные граничные показания счётчика за
// календарный день day (локальная зона). Используется дашбордом для расчёта
// незавершённых тарифных величин текущих суток: день ещё не финализирован, но
// уже есть захваченные на 00:00/07:00/23:00 показания. day передаётся в локальной
// зоне (начало суток).
func (s *pgStore) MeterBoundaryValues(day time.Time) (*meterBoundaryRow, error) {
	var row meterBoundaryRow
	err := s.pool.QueryRow(s.ctx, `
SELECT import_0000, import_0700, import_2300, import_next,
       export_0000, export_0700, export_2300, export_next
FROM sunreceiver.daily_tariffs
WHERE day = $1`, day).Scan(
		&row.Import0000, &row.Import0700, &row.Import2300, &row.ImportNext,
		&row.Export0000, &row.Export0700, &row.Export2300, &row.ExportNext)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return &meterBoundaryRow{}, nil
		}
		return nil, fmt.Errorf("pg meter boundary values: %w", err)
	}
	return &row, nil
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
		bts, ok := parseTS(first.Timestamp)
		if !ok {
			log.Printf("pg legacy: пропускаю снимок с нераспознанным timestamp %q", first.Timestamp)
			continue
		}
		bucket := floorToStep(bts)
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

// parseTS разбирает timestamp снимка (RFC3339) в time.Time. При ошибке
// парсинга — ok=false (вызывающий должен пропустить снимок).
func parseTS(s string) (time.Time, bool) {
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return ts, true
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
