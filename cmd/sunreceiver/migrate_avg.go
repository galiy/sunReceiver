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
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"
)

// argPresent — есть ли флаг в os.Args (простые режимы без полноценного flag-парсера).
func argPresent(name string) bool {
	for _, a := range os.Args[1:] {
		if a == name {
			return true
		}
	}
	return false
}

// Этап 5: миграция истории 5-минутных средних из старых jsonb-таблиц в
// партиционированные колоночные `*_averages_r`. Запуск С ЭТОГО (dev) ПК:
//
//	go run ./cmd/sunreceiver --migrate-avg --config=./sunReceiver.json
//
// Идемпотентно: вставка ON CONFLICT DO NOTHING — уже записанные новые точки
// (после деплоя) не затираются. Для каждого устройства-месяца предсоздаются
// партиции. BMS раскладывается по тому же бинарному контракту (bms26Snapshot),
// что и аккумулятор.

type avgMigMap struct {
	old  string // старая jsonb-таблица (без схемы)
	spec string // новая партиционированная таблица
}

var avgMigMaps = []avgMigMap{
	{"deye_averages", "sunreceiver.deye_averages_r"},
	{"sofar_averages", "sunreceiver.sofar_averages_r"},
	{"dds238_averages", "sunreceiver.dds238_averages_r"},
	{"map_averages", "sunreceiver.map_averages_r"},
	{"kes_averages", "sunreceiver.kes_averages_r"},
	{"dts017m_averages", "sunreceiver.dts017m_averages_r"},
}

// runMigrateAvg выполняет миграцию истории средних (этап 5).
func runMigrateAvg(pg *pgStore) error {
	if err := pg.ensureAvgSchema(); err != nil {
		return err
	}
	// Длинные стримы SELECT упираются в statement_timeout сервера (57014) — снимаем.
	// TZ=UTC: границы месячных партиций и окон совпадают (иначе 23514 на границе).
	for i := 0; i < 8; i++ {
		_, _ = pg.pool.Exec(pg.ctx, "SET statement_timeout = 0")
		_, _ = pg.pool.Exec(pg.ctx, "SET TIME ZONE 'UTC'")
	}
	for _, m := range avgMigMaps {
		if err := migrateAvgScalar(pg, m); err != nil {
			return err
		}
	}
	if err := migrateAvgBms26(pg); err != nil {
		return err
	}
	return nil
}

// monthWindows отдаёт [from,to) по календарным месяцам (UTC) в диапазоне [min,max].
func monthWindows(minTS, maxTS time.Time) [][2]time.Time {
	var ws [][2]time.Time
	t := time.Date(minTS.UTC().Year(), minTS.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	end := maxTS.UTC().AddDate(0, 1, 0)
	for t.Before(end) {
		ws = append(ws, [2]time.Time{t, t.AddDate(0, 1, 0)})
		t = t.AddDate(0, 1, 0)
	}
	return ws
}

// oldRange возвращает min/max ts старой таблицы.
func oldRange(pg *pgStore, old string) (*time.Time, *time.Time, error) {
	var mn, mx *time.Time
	err := pg.pool.QueryRow(pg.ctx, fmt.Sprintf("SELECT min(ts), max(ts) FROM sunreceiver.%s", old)).Scan(&mn, &mx)
	return mn, mx, err
}

// tableExists проверяет наличие таблицы (старая могла быть уже удалена).
func tableExists(pg *pgStore, table string) (bool, error) {
	var ex bool
	err := pg.pool.QueryRow(pg.ctx, `
SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema='sunreceiver' AND table_name=$1)`,
		table).Scan(&ex)
	return ex, err
}

// isNoPartitionErr — ошибка «нет партиции для строки» (23514).
func isNoPartitionErr(err error) bool {
	s := err.Error()
	return strings.Contains(s, "23514") || strings.Contains(s, "no partition of relation") ||
		strings.Contains(s, "не найдена секция")
}

// ensurePartitionForTs создаёт партицию под конкретный ts (страховка на границе).
func (pg *pgStore) ensurePartitionForTs(table string, ts time.Time) {
	_, _ = pg.pool.Exec(pg.ctx, "SELECT sunreceiver.ensure_month_partition($1::regclass, $2)", table, ts)
}

// oldColumns возвращает множество колонок старой таблицы.
func oldColumns(pg *pgStore, table string) (map[string]bool, error) {
	rows, err := pg.pool.Query(pg.ctx, `
SELECT column_name FROM information_schema.columns WHERE table_schema='sunreceiver' AND table_name=$1`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	set := map[string]bool{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		set[c] = true
	}
	return set, rows.Err()
}

func migrateAvgScalar(pg *pgStore, m avgMigMap) error {
	ex, err := tableExists(pg, m.old)
	if err != nil {
		return err
	}
	if !ex {
		log.Printf("migrate-avg: %s отсутствует — пропуск", m.old)
		return nil
	}
	sp := avgSpecByTable(m.spec)
	oldCols, err := oldColumns(pg, m.old)
	if err != nil {
		return err
	}
	sel := []string{sp.key, "ts"}
	present := []string{}
	for _, mn := range sp.meta {
		if oldCols[mn] {
			sel = append(sel, mn)
			present = append(present, mn)
		}
	}
	sel = append(sel, "values")
	mn, mx, err := oldRange(pg, m.old)
	if err != nil {
		return err
	}
	if mn == nil || mx == nil {
		log.Printf("migrate-avg: %s → %s: пусто", m.old, sp.table)
		return nil
	}
	base := fmt.Sprintf("SELECT %s FROM sunreceiver.%s WHERE ts >= $1 AND ts < $2",
		strings.Join(sel, ", "), m.old)
	var n, skipped int
	for _, w := range monthWindows(*mn, *mx) {
		if _, err := pg.pool.Exec(pg.ctx,
			"SELECT sunreceiver.ensure_month_partition($1::regclass, $2)", sp.table, w[0]); err != nil {
			return fmt.Errorf("partition %s %s: %w", sp.table, w[0].Format("2006-01"), err)
		}
		rows, err := pg.pool.Query(pg.ctx, base, w[0], w[1])
		if err != nil {
			return fmt.Errorf("migrate %s %s: %w", m.old, w[0].Format("2006-01"), err)
		}
		for rows.Next() {
			var key string
			var ts time.Time
			metaVals := make([]string, len(present))
			var raw []byte
			ptrs := []any{&key, &ts}
			for i := range metaVals {
				ptrs = append(ptrs, &metaVals[i])
			}
			ptrs = append(ptrs, &raw)
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				return err
			}
			if key == "" {
				skipped++
				continue
			}
			meta := map[string]string{}
			for i, mn := range present {
				meta[mn] = metaVals[i]
			}
			var vc valuesContract
			if len(raw) > 0 {
				if err := json.Unmarshal(raw, &vc); err != nil {
					skipped++
					continue
				}
			}
			if err := insertAvgRowNoClobber(pg.pool, pg.ctx, sp, key, ts, meta, vc); err != nil {
				if !isNoPartitionErr(err) {
					rows.Close()
					return err
				}
				pg.ensurePartitionForTs(sp.table, ts)
				if err2 := insertAvgRowNoClobber(pg.pool, pg.ctx, sp, key, ts, meta, vc); err2 != nil {
					rows.Close()
					return err2
				}
			}
			n++
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	log.Printf("migrate-avg: %s → %s: перенесено %d, пропущено %d", m.old, sp.table, n, skipped)
	return nil
}

func migrateAvgBms26(pg *pgStore) error {
	ex, err := tableExists(pg, "bms26_averages")
	if err != nil {
		return err
	}
	if !ex {
		log.Printf("migrate-avg: bms26_averages отсутствует — пропуск")
		return nil
	}
	mn, mx, err := oldRange(pg, "bms26_averages")
	if err != nil {
		return err
	}
	if mn == nil || mx == nil {
		log.Printf("migrate-avg: bms26_averages → %s: пусто", bms26AvgTable)
		return nil
	}
	var n, skipped int
	for _, w := range monthWindows(*mn, *mx) {
		if _, err := pg.pool.Exec(pg.ctx,
			"SELECT sunreceiver.ensure_month_partition($1::regclass, $2)", bms26AvgTable, w[0]); err != nil {
			return fmt.Errorf("partition %s %s: %w", bms26AvgTable, w[0].Format("2006-01"), err)
		}
		rows, err := pg.pool.Query(pg.ctx,
			"SELECT name, ts, values FROM sunreceiver.bms26_averages WHERE ts >= $1 AND ts < $2", w[0], w[1])
		if err != nil {
			return err
		}
		for rows.Next() {
			var name string
			var ts time.Time
			var raw []byte
			if err := rows.Scan(&name, &ts, &raw); err != nil {
				rows.Close()
				return err
			}
			var sn bms26Snapshot
			if len(raw) > 0 {
				if err := json.Unmarshal(raw, &sn); err != nil {
					skipped++
					continue
				}
			}
			sn.Serial = name
			if sn.Timestamp == "" {
				sn.Timestamp = ts.Format(time.RFC3339)
			}
			if err := insertBms26AveragedExecNoClobber(pg.pool, pg.ctx, name, ts, sn); err != nil {
				if !isNoPartitionErr(err) {
					rows.Close()
					return err
				}
				pg.ensurePartitionForTs(bms26AvgTable, ts)
				if err2 := insertBms26AveragedExecNoClobber(pg.pool, pg.ctx, name, ts, sn); err2 != nil {
					rows.Close()
					return err2
				}
			}
			n++
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	log.Printf("migrate-avg: bms26_averages → %s: перенесено %d, пропущено %d", bms26AvgTable, n, skipped)
	return nil
}
