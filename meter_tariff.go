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
	"fmt"
	"log"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Тарифные границы счётчика DDS238 — константа (ТЗ):
//   - День: 07:00–23:00;
//   - Ночь: 23:00–24:00 и 00:00–07:00.
//
// Для посуточной статистики фиксируем показания Import/Export на границах
// 00:00, 07:00 и 23:00 каждого календарного дня (локальное время, time.Local).
const (
	meterDayStartH  = 7               // начало дневного тарифа (07:00)
	meterDayEndH    = 23              // конец дневного тарифа (23:00)
	meterCaptureTol = 5 * time.Second // окно захвата границы (±5 с от границы)
)

// meterBoundaryHours — часы границ, на которых захватываются показания.
var meterBoundaryHours = []int{0, meterDayStartH, meterDayEndH}

// meterBoundaryTimes возвращает границу, ближайшую к now слева (prev) и справа (next)
// среди всех «часов границ» вокрут настоящего момента (вчера/сегодня/завтра).
func meterBoundaryTimes(now time.Time) (prev, next time.Time) {
	loc := time.Local
	y, m, d := now.In(loc).Date()
	// Собираем границы за вчера, сегодня и завтра.
	var list []time.Time
	for day := -1; day <= 1; day++ {
		base := time.Date(y, m, d, 0, 0, 0, 0, loc).AddDate(0, 0, day)
		for _, h := range meterBoundaryHours {
			list = append(list, time.Date(base.Year(), base.Month(), base.Day(), h, 0, 0, 0, loc))
		}
	}
	prev, next = list[0], list[len(list)-1]
	for _, b := range list {
		if !b.After(now) && b.After(prev) {
			prev = b
		}
		if b.After(now) && b.Before(next) {
			next = b
		}
	}
	return prev, next
}

// meterTariffCapture удерживает в памяти наименьшее отклонение |Δ| для каждой
// границы (ключ — RFC3339 границы). Создаётся один экземпляр на всё время работы
// цикла опроса (runMeterPoll), вызывается каждую секунду, чтобы в пределах окна
// захвата оставить показание, ближайшее к границе: новый отсчёт заменяет только
// если он ближе к границе, чем уже захваченный.
type meterTariffCapture struct {
	pg   *pgStore
	best map[string]time.Duration // RFC3339 границы → минимальное |Δ|
}

func newMeterTariffCapture(pg *pgStore) *meterTariffCapture {
	return &meterTariffCapture{pg: pg, best: map[string]time.Duration{}}
}

// capture обрабатывает ближайшие к now (prev и next) границы: если now в пределах
// окна захвата и отклонение минимально — перезаписывает граничное показание в PG.
func (c *meterTariffCapture) capture(r meterReadings, now time.Time) {
	now = now.In(time.Local)
	prev, next := meterBoundaryTimes(now)
	c.atBoundary(prev, r, now)
	c.atBoundary(next, r, now)
}

// atBoundary обрабатывает одну границу b.
func (c *meterTariffCapture) atBoundary(b time.Time, r meterReadings, now time.Time) {
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
	// Первый отсчёт в окне всегда фиксируется; последующие — только если ближе.
	if prev, ok := c.best[key]; ok && delta >= prev {
		return
	}
	c.best[key] = delta
	if err := c.pg.StoreMeterBoundary(b, r.Import, r.Export); err != nil {
		log.Printf("meter tariff: граница %s: %v", key, err)
	}
}

// meterExecer — минимальный интерфейс для выполнения SQL: позволяет
// StoreMeterBoundary работать как с транзакцией (pgx.Tx), так и с пулом
// (*pgxpool.Pool). В настоящий момент используется только транзакционная
// ветка (pgx.Tx); пул удовлетворяет интерфейс по сигнатурам, проверено
// компиляцией ниже.
type meterExecer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

var (
	_ meterExecer = (pgx.Tx)(nil)
	_ meterExecer = (*pgxpool.Pool)(nil)
)

// StoreMeterBoundary сохраняет показание Import/Export на границе b в таблице
// daily_tariffs (идемпотентно) и пытается финализировать день. Граница 00:00
// принадлежит двум дням: как открывающее показание дня b и как показание на
// 00:00 следующего дня для расчёта ночного тарифа дня (b-1).
//
// Все записи (включая двойную для границы 00:00 и финализацию обоих дней)
// выполняются в ОДНОЙ транзакции: сбой PG между отдельными upsert'ами мог бы
// навсегда оставить день незафинализированным (первый upsert закоммичен,
// второй потерян; живой захват не повторит, добор сочтёт границу захваченной).
func (s *pgStore) StoreMeterBoundary(b time.Time, imp, exp float64) error {
	loc := time.Local
	y, mo, d := b.In(loc).Date()
	hour := b.In(loc).Hour()
	day := time.Date(y, mo, d, 0, 0, 0, 0, loc)

	tx, err := s.pool.Begin(s.ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(s.ctx) }() // после Commit — no-op
	e := meterExecer(tx)

	switch hour {
	case 0:
		if err := applyMeterBoundary(e, s.ctx, day, "import_0000", "export_0000", imp, exp); err != nil {
			return err
		}
		prevDay := day.AddDate(0, 0, -1)
		if err := applyMeterBoundary(e, s.ctx, prevDay, "import_next", "export_next", imp, exp); err != nil {
			return err
		}
		if err := finalizeMeterDay(e, s.ctx, day); err != nil {
			return err
		}
		if err := finalizeMeterDay(e, s.ctx, prevDay); err != nil {
			return err
		}
	case meterDayStartH:
		if err := applyMeterBoundary(e, s.ctx, day, "import_0700", "export_0700", imp, exp); err != nil {
			return err
		}
		if err := finalizeMeterDay(e, s.ctx, day); err != nil {
			return err
		}
	case meterDayEndH:
		if err := applyMeterBoundary(e, s.ctx, day, "import_2300", "export_2300", imp, exp); err != nil {
			return err
		}
		if err := finalizeMeterDay(e, s.ctx, day); err != nil {
			return err
		}
	}
	return tx.Commit(s.ctx)
}

// applyMeterBoundary пишет одно граничное показание в строку дня (UPSERT).
func applyMeterBoundary(e meterExecer, ctx context.Context, day time.Time, colImp, colExp string, imp, exp float64) error {
	q := fmt.Sprintf(`
INSERT INTO sunreceiver.daily_tariffs (day, "%s", "%s")
VALUES ($1, $2, $3)
ON CONFLICT (day) DO UPDATE SET "%s"=EXCLUDED."%s", "%s"=EXCLUDED."%s"`,
		colImp, colExp, colImp, colImp, colExp, colExp)
	_, err := e.Exec(ctx, q, day,
		round3(imp), round3(exp))
	return err
}

// finalizeMeterDay, если у дня есть все 4 граничные показания и разности
// неотрицательны (счётчик не обнулялся), вычисляет и записывает 4 тарифные
// величины: потребление/отдачу по тарифу «День» и «Ночь».
func finalizeMeterDay(e meterExecer, ctx context.Context, day time.Time) error {
	var import0000, import0700, import2300, importNext float64
	var export0000, export0700, export2300, exportNext float64
	err := e.QueryRow(ctx, `
SELECT import_0000, import_0700, import_2300, import_next,
       export_0000, export_0700, export_2300, export_next
FROM sunreceiver.daily_tariffs
WHERE day = $1`, day).Scan(
		&import0000, &import0700, &import2300, &importNext,
		&export0000, &export0700, &export2300, &exportNext)
	if err != nil {
		return nil // строки ещё нет или не все значения — пропускаем
	}

	// День: 07:00→23:00. Ночь: [00:00→07:00] + [23:00→00:00 следующего дня].
	impDay := import2300 - import0700
	impNight := (import0700 - import0000) + (importNext - import2300)
	expDay := export2300 - export0700
	expNight := (export0700 - export0000) + (exportNext - export2300)

	// Счётчик 32-бит не обнулится (сотни тыс. kWh), но на всякий случай
	// отрицательные разности (сброс/замена счётчика) финализацию пропускаем.
	// Если сумма ночи неотрицательна, но одна из двух ночных частей
	// отрицательна (замена счётчика ночью с большим базовым показанием) —
	// день финализируем, но предупреждаем: «ночь» собрана из отрицательных
	// частей и может быть завышена.
	if (import0700-import0000 < 0 || importNext-import2300 < 0 ||
		export0700-export0000 < 0 || exportNext-export2300 < 0) && impNight >= 0 && expNight >= 0 {
		log.Printf("tariff: день %s: ночная разность собрана из отрицательных частей (замена счётчика?)",
			day.Format("2006-01-02"))
	}
	if impDay < 0 || impNight < 0 || expDay < 0 || expNight < 0 {
		return nil
	}

	_, err = e.Exec(ctx, `
UPDATE sunreceiver.daily_tariffs
SET import_day=$2, import_night=$3, export_day=$4, export_night=$5, finalized=now()
WHERE day=$1`, day, round3(impDay), round3(impNight), round3(expDay), round3(expNight))
	return err
}

// ensureMeterTariffSchema создаёт таблицу посуточных тарифных значний DDS238.
func ensureMeterTariffSchema(s *pgStore) error {
	_, err := s.pool.Exec(s.ctx, `
CREATE TABLE IF NOT EXISTS sunreceiver.daily_tariffs (
	day          date PRIMARY KEY,
	-- граничные показания (kWh): на 00:00, 07:00, 23:00 и на 00:00 следующего дня
	import_0000  double precision,
	import_0700 double precision,
	import_2300 double precision,
	import_next double precision,
	export_0000 double precision,
	export_0700 double precision,
	export_2300 double precision,
	export_next double precision,
	-- вычисленные тарифные величины (kWh)
	import_day    double precision,
	import_night  double precision,
	export_day    double precision,
	export_night  double precision,
	finalized timestamptz
);
`)
	return err
}

// round3 округляет вещественное до 3 знаков (счётчики имеют 2 знака после
// запятой; 3 знака оставляют запас от float-шумов). math.Round корректно
// округляет и отрицательные значения.
func round3(v float64) float64 {
	return math.Round(v*1000) / 1000
}
