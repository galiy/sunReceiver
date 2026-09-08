package main

import (
	"log"
	"math"
	"time"
)

// Добор (backfill) посуточных тарифных границ счётчика DDS238.
//
// Живой захват (meterTariffCapture) фиксирует показание Import/Export только
// если пулер работал в пределах ±meterCaptureTol от границы И счётчик ответил.
// Если пулер был выключен/в рестарте либо не было сети (счётчик молчал) — граница
// пропускается. Тогда после восстановления работы фоновый процесс находит в ряде
// Redis уже зафиксированные показания, ближайшие к пропущенной границе, и пишет
// их в daily_tariffs (добор — best-effort, «ближайшее из зафиксированного»).
const (
	// meterBackfillInterval — периодичность добора границ.
	meterBackfillInterval = 5 * time.Minute
	// meterBackfillMinAge — граница «безопасно в прошлом»: добор не трогает
	// границы, которые ещё не наступили или прошли меньше этого периода, чтобы
	// не зафиксировать заведомо преждевременное (не финальное) показание раньше,
	// чем живой захват имел шанс сработать.
	meterBackfillMinAge = 10 * time.Minute
	// meterBackfillScanT — полуширина окна поиска ближайшей точки вокруг границы.
	meterBackfillScanT = 6 * time.Hour
)

// runMeterBackfill — фоновый добор пропущенных тарифных границ. Сразу при старте
// делает catch-up за всё окно удержания Redis, затем повторяется каждые
// meterBackfillInterval. Останавливается по закрытию канала stop.
func runMeterBackfill(store *redisStore, pg *pgStore, cfg *meterConfig, stop <-chan struct{}) {
	if pg == nil || cfg == nil {
		return
	}
	backfillMeterBoundaries(store, pg, cfg, time.Now())
	ticker := time.NewTicker(meterBackfillInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			backfillMeterBoundaries(store, pg, cfg, time.Now())
		case <-stop:
			return
		}
	}
}

// backfillMeterBoundaries пробегает по всем границам за окно удержания Redis
// (последние 2 календарных суток), которые уже безопасно в прошлом и не были
// захвачены живьём, и дофиксирует их ближайшим из записанных показаний.
func backfillMeterBoundaries(store *redisStore, pg *pgStore, cfg *meterConfig, now time.Time) {
	start := recentCutoff(now) // 00:00 вчера (локально) — нижняя граница Redis
	loc := time.Local
	startDay := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, loc)

	// Итерируем по календарным дням окна.
	for day := startDay; day.Before(now); day = day.AddDate(0, 0, 1) {
		for _, h := range meterBoundaryHours {
			b := time.Date(day.Year(), day.Month(), day.Day(), h, 0, 0, 0, loc)
			if !b.Before(now) {
				continue // граница не наступила
			}
			if now.Sub(b) < meterBackfillMinAge {
				continue // ещё рано, может сработать живой захват
			}
			if b.Before(start) {
				continue // вне окна Redis — данных нет
			}
			if pg.meterBoundaryCaptured(b) {
				continue // уже захвачено (живьём или ранее добором)
			}
			imp, exp, ok := nearestMeterReading(store, cfg, b)
			if !ok {
				continue // нет зафиксированных показаний рядом с границей
			}
			if err := pg.StoreMeterBoundary(b, imp, exp); err != nil {
				log.Printf("meter backfill: граница %s: %v", b.Format(time.RFC3339), err)
			} else {
				log.Printf("meter backfill: граница %s дофиксирована ближайшим показанием (imp=%.3f exp=%.3f)",
					b.Format(time.RFC3339), imp, exp)
			}
		}
	}
}

// meterBoundaryCaptured возвращает true, если показание Import на границе b уже
// записано в daily_tariffs (т.е. граница захвачена живьём или ранее добором).
// Достаточно проверить import-колонку — StoreMeterBoundary всегда пишет пару
// import/export вместе.
func (s *pgStore) meterBoundaryCaptured(b time.Time) bool {
	col := meterBoundaryImportCol(b.Hour())
	if col == "" {
		return false
	}
	loc := time.Local
	y, mo, d := b.In(loc).Date()
	day := time.Date(y, mo, d, 0, 0, 0, 0, loc)
	var v *float64
	err := s.pool.QueryRow(s.ctx,
		`SELECT "`+col+`" FROM sunreceiver.daily_tariffs WHERE day=$1`, day).Scan(&v)
	if err != nil {
		return false
	}
	return v != nil
}

// meterBoundaryImportCol возвращает имя import-колонки в daily_tariffs по часу границы.
func meterBoundaryImportCol(hour int) string {
	switch hour {
	case 0:
		return "import_0000"
	case meterDayStartH:
		return "import_0700"
	case meterDayEndH:
		return "import_2300"
	}
	return ""
}

// nearestMeterReading ищет в ряде Redis (по devKey cfg.IP) снимок счётчика,
// ближайший по времени к границе b в пределах ±meterBackfillScanT, и возвращает
// его показание Import/Export (kWh). Если поблизости данных нет — ok=false.
func nearestMeterReading(store *redisStore, cfg *meterConfig, b time.Time) (imp, exp float64, ok bool) {
	from := b.Add(-meterBackfillScanT)
	to := b.Add(meterBackfillScanT)
	snaps, err := store.QuerySeries(from, to)
	if err != nil {
		return 0, 0, false
	}
	var bestImp, bestExp float64
	var bestDelta = time.Duration(math.MaxInt64)
	for _, sn := range snaps {
		if sn.IP != cfg.IP {
			continue
		}
		ii, okI := snapFloat(sn.Values, "meter_import")
		ee, okE := snapFloat(sn.Values, "meter_export")
		if !okI || !okE {
			continue
		}
		ts, okTS := parseTS(sn.Timestamp)
		if !okTS {
			continue
		}
		d := ts.Sub(b)
		if d < 0 {
			d = -d
		}
		if d < bestDelta {
			bestDelta = d
			bestImp = ii
			bestExp = ee
		}
	}
	if bestDelta == time.Duration(math.MaxInt64) {
		return 0, 0, false
	}
	return bestImp, bestExp, true
}