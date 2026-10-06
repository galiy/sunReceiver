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
	"log"
	"math"
	"time"
)

// Добор (backfill) пропущенных тарифных границ DTS017M. Логика повторяет
// meter_backfill.go, но с СОБСТВЕННЫМ рядом Redis (dts017) и собственной таблицей
// тарифов (dts017_tariff.go). Best-effort «ближайшее из зафиксированного».

// runDts017Backfill — фоновый добор границ: сразу при старте catch-up, затем
// каждые meterBackfillInterval. Останавливается по закрытию ctx.
func runDts017Backfill(store *redisStore, pg *pgStore, cfg *dts017Config, ctx context.Context) {
	if pg == nil || cfg == nil {
		return
	}
	backfillDts017Boundaries(store, pg, cfg, time.Now())
	ticker := time.NewTicker(meterBackfillInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			backfillDts017Boundaries(store, pg, cfg, time.Now())
		case <-ctx.Done():
			return
		}
	}
}

// backfillDts017Boundaries проходит по границам за окно удержания Redis и
// дофиксирует пропущенные ближайшим из записанных показаний.
func backfillDts017Boundaries(store *redisStore, pg *pgStore, cfg *dts017Config, now time.Time) {
	start := recentCutoff(now)
	loc := time.Local
	startDay := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, loc)

	for day := startDay; day.Before(now); day = day.AddDate(0, 0, 1) {
		for _, h := range meterBoundaryHours {
			b := time.Date(day.Year(), day.Month(), day.Day(), h, 0, 0, 0, loc)
			if !b.Before(now) {
				continue
			}
			if now.Sub(b) < meterBackfillMinAge {
				continue
			}
			if b.Before(start) {
				continue
			}
			if pg.dts017BoundaryCaptured(b) {
				continue
			}
			imp, exp, ok := nearestDts017Reading(store, b)
			if !ok {
				continue
			}
			if err := pg.StoreDts017Boundary(b, imp, exp); err != nil {
				log.Printf("dts017m backfill: граница %s: %v", b.Format(time.RFC3339), err)
			} else {
				log.Printf("dts017m backfill: граница %s дофиксирована (imp=%.3f exp=%.3f)",
					b.Format(time.RFC3339), imp, exp)
			}
		}
	}
}

// nearestDts017Reading ищет в собственном ряду Redis снимок, ближайший по времени
// к границе b (±meterBackfillScanT), и возвращает его Import/Export (kWh). Принято
// только показание не дальше meterBackfillMaxDist.
func nearestDts017Reading(store *redisStore, b time.Time) (imp, exp float64, ok bool) {
	snaps, err := store.QueryDts017Series(b.Add(-meterBackfillScanT), b.Add(meterBackfillScanT))
	if err != nil {
		return 0, 0, false
	}
	var bestImp, bestExp float64
	bestDelta := time.Duration(math.MaxInt64)
	for _, sn := range snaps {
		ii, okI := sn.Values[dts017Import]
		ee, okE := sn.Values[dts017Export]
		if !okI || !okE {
			continue
		}
		ts, perr := time.Parse(time.RFC3339, sn.Timestamp)
		if perr != nil {
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
	if bestDelta > meterBackfillMaxDist {
		return 0, 0, false
	}
	return bestImp, bestExp, true
}
