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
	"sync"
	"time"
)

// Аккумуляторы AGM: два независимых 5-минутных ряда — PZEM и BK300 — в свои
// таблицы sunreceiver.agm_pzem_averages_r / agm_bk300_averages_r.
// Правила (решение оператора):
//   - обычные теги — среднее по окну;
//   - накопительная энергия PZEM и сырые флаги BK300 — последнее значение окна;
//   - pzem_alarm_high/low — 1, если в окне была хотя бы одна 1, иначе 0.

// insertAGMAverage сохраняет одну 5-минутную точку источника AGM.
func (s *pgStore) insertAGMAverage(table, device string, ts time.Time, vc map[string]float64) error {
	conv := make(valuesContract, len(vc))
	for k, v := range vc {
		conv[k] = v
	}
	return insertAvgRow(s.pool, s.ctx, avgSpecByTable(table), device, ts, nil, conv)
}

func (s *pgStore) InsertAGMPZEMAverage(ts time.Time, vc map[string]float64) error {
	return s.insertAGMAverage(agmPZEMAvgTable, agmPZEMDeviceKey, ts, vc)
}
func (s *pgStore) InsertAGMBKAverage(ts time.Time, vc map[string]float64) error {
	return s.insertAGMAverage(agmBKAvgTable, agmBKDeviceKey, ts, vc)
}

// averageAGM усредняет снимки одного 5-минутного окна.
func averageAGM(snaps []agmSnapshot) map[string]float64 {
	sums := map[string]float64{}
	counts := map[string]int{}
	lastTS := map[string]time.Time{}
	lastVal := map[string]float64{}
	// Сколько раз тег вообще встретился (для флагов/аварий с нулевыми значениями).
	seen := map[string]bool{}
	alarmHit := map[string]bool{}

	for _, sn := range snaps {
		ts, perr := time.Parse(time.RFC3339Nano, sn.Timestamp)
		if perr != nil {
			continue
		}
		for k, v := range sn.Values {
			seen[k] = true
			switch {
			case agmCumulativeTags[k] || agmLastValueTags[k]:
				// Метки времени снимков — с миллисекундами (RFC3339Nano), так
				// что точки в одну секунду различимы; при полном равенстве
				// оставляем значение, идущее позже в упорядоченном ряду.
				if last, ok := lastTS[k]; !ok || !ts.Before(last) {
					lastTS[k] = ts
					lastVal[k] = v
				}
			case agmAlarmTags[k]:
				if v > 0.5 {
					alarmHit[k] = true
				}
			default:
				sums[k] += v
				counts[k]++
			}
		}
	}

	out := map[string]float64{}
	for k, n := range counts {
		if n == 0 {
			continue
		}
		out[k] = agmRoundTag(k, sums[k]/float64(n))
	}
	for k, v := range lastVal {
		out[k] = agmRoundTag(k, v)
	}
	for k := range agmAlarmTags {
		if !seen[k] {
			continue
		}
		if alarmHit[k] {
			out[k] = 1
		} else {
			out[k] = 0
		}
	}
	return out
}

// runAGMAccumulator запускает два независимых аккумулятора: PZEM и BK300.
func runAGMAccumulator(store *redisStore, pg *pgStore, ctx context.Context) {
	if pg == nil {
		return
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); runAGMSourceAccumulator("pzem", store, pg, ctx) }()
	go func() { defer wg.Done(); runAGMSourceAccumulator("bk300", store, pg, ctx) }()
	wg.Wait()
}

func agmSourceQuery(store *redisStore, source string) func(time.Time, time.Time) ([]agmSnapshot, error) {
	if source == "pzem" {
		return func(a, b time.Time) ([]agmSnapshot, error) { return store.QueryAGMPZEMSeries(a, b) }
	}
	return func(a, b time.Time) ([]agmSnapshot, error) { return store.QueryAGMBKSeries(a, b) }
}

func agmSourceInsert(pg *pgStore, source string, ts time.Time, vc map[string]float64) error {
	if source == "pzem" {
		return pg.InsertAGMPZEMAverage(ts, vc)
	}
	return pg.InsertAGMBKAverage(ts, vc)
}

// runAGMSourceAccumulator — цикл усреднения одного источника AGM в его таблицу.
func runAGMSourceAccumulator(source string, store *redisStore, pg *pgStore, ctx context.Context) {
	query := agmSourceQuery(store, source)
	log.Printf("agm %s: avg старт; период=%s, отсрочка=%s", source, avgStep, avgDelay)
	backfillAGMSourceAccumulator(source, store, pg, query, time.Now())

	var wg sync.WaitGroup
	defer wg.Wait()

	process := func(start, end time.Time) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snaps, err := query(start, end.Add(-time.Millisecond))
			if err != nil {
				log.Printf("agm %s: avg %s: %v", source, start.Format(time.RFC3339), err)
				return
			}
			if len(snaps) == 0 {
				return
			}
			vc := averageAGM(snaps)
			if len(vc) == 0 {
				return
			}
			if err := retryPg(func() error { return agmSourceInsert(pg, source, start, vc) }, 3); err != nil {
				log.Printf("agm %s: avg pg %s: %v", source, start.Format(time.RFC3339), err)
			}
		}()
	}

	end := time.Now().Truncate(avgStep).Add(avgStep)
	for {
		if d := time.Until(end.Add(avgDelay)); d > 0 {
			t := time.NewTimer(d)
			select {
			case <-t.C:
			case <-ctx.Done():
				stopTimer(t)
				return
			}
		}
		now := time.Now()
		for !end.Add(avgDelay).After(now) {
			process(end.Add(-avgStep), end)
			end = end.Add(avgStep)
		}
	}
}

// backfillAGMSourceAccumulator конвертирует накопленное окно Redis в PG.
func backfillAGMSourceAccumulator(source string, store *redisStore, pg *pgStore, query func(time.Time, time.Time) ([]agmSnapshot, error), now time.Time) {
	if pg == nil {
		return
	}
	end := floorToStep(now)
	start := recentCutoff(now)
	if !start.Before(end) {
		return
	}
	snaps, err := query(start, end)
	if err != nil {
		log.Printf("agm %s acc backfill: %v", source, err)
		return
	}
	groups := map[time.Time][]agmSnapshot{}
	for _, sn := range snaps {
		ts, perr := time.Parse(time.RFC3339Nano, sn.Timestamp)
		if perr != nil || ts.Before(start) || !ts.Before(end) {
			continue
		}
		bts := floorToStep(ts)
		groups[bts] = append(groups[bts], sn)
	}
	var n int
	for bts, bucket := range groups {
		vc := averageAGM(bucket)
		if len(vc) == 0 {
			continue
		}
		if err := agmSourceInsert(pg, source, bts, vc); err != nil {
			log.Printf("agm %s acc backfill %s: %v", source, bts.Format(time.RFC3339), err)
			continue
		}
		n++
	}
	log.Printf("agm %s acc backfill: обработано %d 5-минутных бакетов", source, n)
}
