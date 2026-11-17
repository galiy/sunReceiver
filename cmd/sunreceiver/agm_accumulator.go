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

// Аккумулятор AGM: усредняет мгновенные снимки собственного ряда Redis до одной
// записи за 5 минут в СОБСТВЕННУЮ таблицу sunreceiver.agm_averages_r.
// Правила (решение оператора):
//   - обычные теги — среднее по окну;
//   - накопительная энергия PZEM и сырые флаги BK300 — последнее значение окна;
//   - pzem_alarm_high/low — 1, если в окне была хотя бы одна 1, иначе 0.

const agmAvgTable = "sunreceiver.agm_averages_r"

// InsertAGMAverage сохраняет одну 5-минутную точку AGM в партиционированную
// таблицу. Идемпотентна по (name, ts).
func (s *pgStore) InsertAGMAverage(ts time.Time, vc map[string]float64) error {
	conv := make(valuesContract, len(vc))
	for k, v := range vc {
		conv[k] = v
	}
	return insertAvgRow(s.pool, s.ctx, avgSpecByTable(agmAvgTable), agmDeviceKey, ts, nil, conv)
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

// runAGMAccumulator — фоновый цикл усреднения истории AGM в PG. По завершении
// каждого 5-минутного промежутка (спустя avgDelay) пишет одну точку в
// sunreceiver.agm_averages_r. При старте — catch-up по окну Redis.
func runAGMAccumulator(store *redisStore, pg *pgStore, ctx context.Context) {
	if pg == nil {
		return
	}
	log.Printf("agm: avg старт; период=%s, отсрочка=%s, 5-минутные промежутки", avgStep, avgDelay)
	backfillAGMAccumulator(store, pg, time.Now())

	var wg sync.WaitGroup
	defer wg.Wait()

	process := func(start, end time.Time) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snaps, err := store.QueryAGMSeries(start, end.Add(-time.Millisecond))
			if err != nil {
				log.Printf("agm: avg %s: %v", start.Format(time.RFC3339), err)
				return
			}
			if len(snaps) == 0 {
				return
			}
			vc := averageAGM(snaps)
			if len(vc) == 0 {
				return
			}
			if err := retryPg(func() error {
				return pg.InsertAGMAverage(start, vc)
			}, 3); err != nil {
				log.Printf("agm: avg pg %s: %v", start.Format(time.RFC3339), err)
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

// backfillAGMAccumulator конвертирует уже накопленные в Redis снимки (окно
// удержания) в 5-минутные точки PG, чтобы промежутки до старта не потерялись.
func backfillAGMAccumulator(store *redisStore, pg *pgStore, now time.Time) {
	if pg == nil {
		return
	}
	end := floorToStep(now)
	start := recentCutoff(now)
	if !start.Before(end) {
		return
	}
	snaps, err := store.QueryAGMSeries(start, end)
	if err != nil {
		log.Printf("agm acc backfill: %v", err)
		return
	}
	if len(snaps) == 0 {
		return
	}
	groups := map[time.Time][]agmSnapshot{}
	for _, sn := range snaps {
		ts, perr := time.Parse(time.RFC3339Nano, sn.Timestamp)
		if perr != nil {
			continue
		}
		if ts.Before(start) || !ts.Before(end) {
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
		if err := pg.InsertAGMAverage(bts, vc); err != nil {
			log.Printf("agm acc backfill %s: %v", bts.Format(time.RFC3339), err)
			continue
		}
		n++
	}
	log.Printf("agm acc backfill: обработано %d 5-минутных бакетов", n)
}
