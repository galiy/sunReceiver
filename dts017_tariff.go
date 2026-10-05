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
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Посуточная тарифная статистика DTS017M. Логика полностью повторяет DDS238
// (см. meter_tariff.go), но с СОБСТВЕННОЙ таблицей sunreceiver.dts017m_daily_tariffs
// (общую daily_tariffs DDS238 не трогаем).
//
// Зоны (локальное время time.Local): День 07:00–23:00; Ночь 23:00–24:00 и
// 00:00–07:00. Границы захвата: 00:00, 07:00, 23:00. Историю счётчика не читаем —
// считаем сами по накопленным Import/Export между границами.

// dts017TariffTable — обособленная таблица посуточных тарифов DTS017M.
const dts017TariffTable = "sunreceiver.dts017m_daily_tariffs"

// dts017TariffCapture удерживает наименьшее отклонение |Δ| для каждой границы
// (ключ — RFC3339 границы) в одном цикле опроса.
type dts017TariffCapture struct {
	pg   *pgStore
	best map[string]time.Duration
}

func newDts017TariffCapture(pg *pgStore) *dts017TariffCapture {
	return &dts017TariffCapture{pg: pg, best: map[string]time.Duration{}}
}

// capture обрабатывает ближайшие к now границы (prev/next) в окне ±meterCaptureTol.
func (c *dts017TariffCapture) capture(r dts017Readings, now time.Time) {
	now = now.In(time.Local)
	prev, next := meterBoundaryTimes(now)
	c.atBoundary(prev, r, now)
	c.atBoundary(next, r, now)
}

func (c *dts017TariffCapture) atBoundary(b time.Time, r dts017Readings, now time.Time) {
	if b.IsZero() {
		return
	}
	delta := now.Sub(b)
	if delta < 0 {
		delta = -delta
	}
	if delta > meterCaptureTol {
		return
	}
	key := b.Format(time.RFC3339)
	if prev, ok := c.best[key]; ok && delta >= prev {
		return
	}
	c.best[key] = delta
	if err := c.pg.StoreDts017Boundary(b, r.Import, r.Export); err != nil {
		log.Printf("dts017m tariff: граница %s: %v", key, err)
	}
}

// StoreDts017Boundary сохраняет показание Import/Export на границе b и пытается
// финализировать день. Граница 00:00 принадлежит двум дням; все записи и обе
// финализации — в ОДНОЙ транзакции (атомарность, как у DDS238).
func (s *pgStore) StoreDts017Boundary(b time.Time, imp, exp float64) error {
	loc := time.Local
	y, mo, d := b.In(loc).Date()
	hour := b.In(loc).Hour()
	day := time.Date(y, mo, d, 0, 0, 0, 0, loc)

	tx, err := s.pool.Begin(s.ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(s.ctx) }()
	e := meterExecer(tx)

	switch hour {
	case 0:
		if err := applyDts017Boundary(e, s.ctx, day, "import_0000", "export_0000", imp, exp); err != nil {
			return err
		}
		prevDay := day.AddDate(0, 0, -1)
		if err := applyDts017Boundary(e, s.ctx, prevDay, "import_next", "export_next", imp, exp); err != nil {
			return err
		}
		if err := finalizeDts017Day(e, s.ctx, day); err != nil {
			return err
		}
		if err := finalizeDts017Day(e, s.ctx, prevDay); err != nil {
			return err
		}
	case meterDayStartH:
		if err := applyDts017Boundary(e, s.ctx, day, "import_0700", "export_0700", imp, exp); err != nil {
			return err
		}
		if err := finalizeDts017Day(e, s.ctx, day); err != nil {
			return err
		}
	case meterDayEndH:
		if err := applyDts017Boundary(e, s.ctx, day, "import_2300", "export_2300", imp, exp); err != nil {
			return err
		}
		if err := finalizeDts017Day(e, s.ctx, day); err != nil {
			return err
		}
	}
	return tx.Commit(s.ctx)
}

// applyDts017Boundary пишет одно граничное показание (UPSERT) в строку дня.
func applyDts017Boundary(e meterExecer, ctx context.Context, day time.Time, colImp, colExp string, imp, exp float64) error {
	q := fmt.Sprintf(`
INSERT INTO %s (day, "%s", "%s")
VALUES ($1, $2, $3)
ON CONFLICT (day) DO UPDATE SET "%s"=EXCLUDED."%s", "%s"=EXCLUDED."%s"`,
		dts017TariffTable, colImp, colExp, colImp, colImp, colExp, colExp)
	_, err := e.Exec(ctx, q, day, round3(imp), round3(exp))
	return err
}

// finalizeDts017Day, если собраны все 4 границы и разности неотрицательны,
// вычисляет import/export_day/night и отмечает день финализированным.
func finalizeDts017Day(e meterExecer, ctx context.Context, day time.Time) error {
	var import0000, import0700, import2300, importNext *float64
	var export0000, export0700, export2300, exportNext *float64
	q := fmt.Sprintf(`
SELECT import_0000, import_0700, import_2300, import_next,
       export_0000, export_0700, export_2300, export_next
FROM %s WHERE day = $1`, dts017TariffTable)
	err := e.QueryRow(ctx, q, day).Scan(
		&import0000, &import0700, &import2300, &importNext,
		&export0000, &export0700, &export2300, &exportNext)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("dts017m tariff: чтение границ дня %s: %w", day.Format("2006-01-02"), err)
	}

	var missing []string
	check := func(name string, v *float64) {
		if v == nil {
			missing = append(missing, name)
		}
	}
	check("import_0000", import0000)
	check("import_0700", import0700)
	check("import_2300", import2300)
	check("import_next", importNext)
	check("export_0000", export0000)
	check("export_0700", export0700)
	check("export_2300", export2300)
	check("export_next", exportNext)
	if len(missing) > 0 {
		log.Printf("dts017m tariff: день %s: границы неполные (%s) — тариф не финализирован",
			day.Format("2006-01-02"), strings.Join(missing, ","))
		return nil
	}

	impDay := *import2300 - *import0700
	impNight := (*import0700 - *import0000) + (*importNext - *import2300)
	expDay := *export2300 - *export0700
	expNight := (*export0700 - *export0000) + (*exportNext - *export2300)

	if (*import0700-*import0000 < 0 || *importNext-*import2300 < 0 ||
		*export0700-*export0000 < 0 || *exportNext-*export2300 < 0) && impNight >= 0 && expNight >= 0 {
		log.Printf("dts017m tariff: день %s: ночная разность собрана из отрицательных частей (замена счётчика?)",
			day.Format("2006-01-02"))
	}
	if impDay < 0 || impNight < 0 || expDay < 0 || expNight < 0 {
		return nil
	}

	q = fmt.Sprintf(`
UPDATE %s
SET import_day=$2, import_night=$3, export_day=$4, export_night=$5, finalized=now()
WHERE day=$1`, dts017TariffTable)
	_, err = e.Exec(ctx, q, day, round3(impDay), round3(impNight), round3(expDay), round3(expNight))
	return err
}

// ensureDts017TariffSchema создаёт таблицу посуточных тарифов DTS017M.
func ensureDts017TariffSchema(s *pgStore) error {
	_, err := s.pool.Exec(s.ctx, fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s (
	day          date PRIMARY KEY,
	import_0000  double precision,
	import_0700 double precision,
	import_2300 double precision,
	import_next double precision,
	export_0000 double precision,
	export_0700 double precision,
	export_2300 double precision,
	export_next double precision,
	import_day    double precision,
	import_night  double precision,
	export_day    double precision,
	export_night  double precision,
	finalized timestamptz
);
`, dts017TariffTable))
	return err
}

// dts017BoundaryCaptured — true, если граница b уже зафиксирована в таблице тарифов.
func (s *pgStore) dts017BoundaryCaptured(b time.Time) bool {
	col := meterBoundaryImportCol(b.Hour())
	if col == "" {
		return false
	}
	loc := time.Local
	y, mo, d := b.In(loc).Date()
	day := time.Date(y, mo, d, 0, 0, 0, 0, loc)
	var v *float64
	q := fmt.Sprintf(`SELECT "%s" FROM %s WHERE day=$1`, col, dts017TariffTable)
	err := s.pool.QueryRow(s.ctx, q, day).Scan(&v)
	if err != nil || v == nil {
		return false
	}
	if b.In(loc).Hour() == 0 {
		prevDay := day.AddDate(0, 0, -1)
		var next *float64
		q := fmt.Sprintf(`SELECT "import_next" FROM %s WHERE day=$1`, dts017TariffTable)
		err := s.pool.QueryRow(s.ctx, q, prevDay).Scan(&next)
		return err == nil && next != nil
	}
	return true
}

// Dts017BoundaryValues возвращает фиксированные граничные показания счётчика за
// календарный день day (для будущего дашборда; сейчас API не используется).
func (s *pgStore) Dts017BoundaryValues(day time.Time) (*meterBoundaryRow, error) {
	var row meterBoundaryRow
	q := fmt.Sprintf(`
SELECT import_0000, import_0700, import_2300, import_next,
       export_0000, export_0700, export_2300, export_next
FROM %s WHERE day = $1`, dts017TariffTable)
	err := s.pool.QueryRow(s.ctx, q, day).Scan(
		&row.Import0000, &row.Import0700, &row.Import2300, &row.ImportNext,
		&row.Export0000, &row.Export0700, &row.Export2300, &row.ExportNext)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return &meterBoundaryRow{}, nil
		}
		return nil, fmt.Errorf("pg dts017m boundary values: %w", err)
	}
	return &row, nil
}

// DailyDts017TariffsRange возвращает финализированные посуточные тарифы DTS017M за
// период [start, end) (для будущих потребителей; сейчас API не используется).
func (s *pgStore) DailyDts017TariffsRange(start, end time.Time) ([]meterDayStat, error) {
	q := fmt.Sprintf(`
SELECT day, import_day, import_night, export_day, export_night
FROM %s
WHERE finalized IS NOT NULL
  AND import_day IS NOT NULL AND import_night IS NOT NULL
  AND export_day IS NOT NULL AND export_night IS NOT NULL
  AND day >= $1 AND day < $2
ORDER BY day ASC`, dts017TariffTable)
	rows, err := s.pool.Query(s.ctx, q, start, end)
	if err != nil {
		return nil, fmt.Errorf("pg dts017m daily_tariffs range: %w", err)
	}
	defer rows.Close()
	got := []meterDayStat{}
	for rows.Next() {
		var day time.Time
		var st meterDayStat
		if err := rows.Scan(&day, &st.ImportDay, &st.ImportNight, &st.ExportDay, &st.ExportNight); err != nil {
			return nil, fmt.Errorf("pg dts017m daily_tariffs scan: %w", err)
		}
		st.Day = day.Format("2006-01-02")
		got = append(got, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg dts017m daily_tariffs rows: %w", err)
	}
	return got, nil
}
