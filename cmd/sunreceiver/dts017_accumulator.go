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
	"sync"
	"time"
)

// Аккумулятор DTS017M: усредняет мгновенные снимки собственного ряда Redis до
// 1 записи за 5 минут в СОБСТВЕННУЮ таблицу sunreceiver.dts017m_averages_r. Общий
// accumulator.go (и таблица averages) не задействован. Накопительные величины
// (энергии, dts017CumulativeTags) берутся последним значением окна, остальные —
// средним.

// (Схема DTS017M — партиционированная dts017m_averages_r, см. avg_r.go.)

// InsertDts017Average сохраняет одну 5-минутную точку DTS017M в
// партиционированную колоночную таблицу dts017m_averages_r. Идемпотентна по (name, ts).
func (s *pgStore) InsertDts017Average(name string, ts time.Time, vc map[string]float64) error {
	cv := make(valuesContract, len(vc))
	for k, v := range vc {
		cv[k] = v
	}
	return insertAvgRow(s.pool, s.ctx, avgSpecByTable("sunreceiver.dts017m_averages_r"), name, ts, nil, cv)
}

// averageDts017 усредняет снимки одного 5-минутного окна: обычные теги — среднее
// (округление до 1 знака), накопительные энергии — последнее значение окна.
func averageDts017(snaps []dts017Snapshot) map[string]float64 {
	sums := map[string]float64{}
	counts := map[string]int{}
	lastTS := map[string]time.Time{}
	lastVal := map[string]float64{}
	for _, sn := range snaps {
		ts, perr := time.Parse(time.RFC3339, sn.Timestamp)
		if perr != nil {
			continue
		}
		for k, v := range sn.Values {
			if dts017CumulativeTags[k] {
				if last, ok := lastTS[k]; !ok || ts.After(last) {
					lastTS[k] = ts
					lastVal[k] = v
				}
				continue
			}
			sums[k] += v
			counts[k]++
		}
	}
	out := map[string]float64{}
	for k, n := range counts {
		if n == 0 {
			continue
		}
		out[k] = math.Round(sums[k]/float64(n)*10) / 10
	}
	for k, v := range lastVal {
		out[k] = math.Round(v*100) / 100
	}
	return out
}

// runDts017Accumulator — фоновый цикл усреднения истории DTS017M в PG. По
// завершении каждого 5-минутного промежутка (спустя avgDelay) пишет одну точку в
// sunreceiver.dts017m_averages_r. При старте — catch-up по окну Redis.
func runDts017Accumulator(store *redisStore, pg *pgStore, name string, ctx context.Context) {
	if pg == nil {
		return
	}
	log.Printf("dts017m: avg старт; период=%s, отсрочка=%s, 5-минутные промежутки", avgStep, avgDelay)
	backfillDts017Accumulator(store, pg, name, time.Now())

	var wg sync.WaitGroup
	defer wg.Wait()

	process := func(start, end time.Time) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snaps, err := store.QueryDts017Series(start, end.Add(-time.Second))
			if err != nil {
				log.Printf("dts017m: avg %s: %v", start.Format(time.RFC3339), err)
				return
			}
			if len(snaps) == 0 {
				return
			}
			vc := averageDts017(snaps)
			if len(vc) == 0 {
				return
			}
			if err := retryPg(func() error {
				return pg.InsertDts017Average(name, start, vc)
			}, 3); err != nil {
				log.Printf("dts017m: avg pg %s: %v", start.Format(time.RFC3339), err)
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

// backfillDts017Accumulator конвертирует уже накопленные в Redis снимки (окно
// удержания) в 5-минутные точки PG, чтобы промежутки до старта не потерялись.
func backfillDts017Accumulator(store *redisStore, pg *pgStore, name string, now time.Time) {
	if pg == nil {
		return
	}
	end := floorToStep(now)
	start := recentCutoff(now)
	if !start.Before(end) {
		return
	}
	snaps, err := store.QueryDts017Series(start, end)
	if err != nil {
		log.Printf("dts017m acc backfill: %v", err)
		return
	}
	if len(snaps) == 0 {
		return
	}
	groups := map[time.Time][]dts017Snapshot{}
	for _, sn := range snaps {
		ts, perr := time.Parse(time.RFC3339, sn.Timestamp)
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
		vc := averageDts017(bucket)
		if len(vc) == 0 {
			continue
		}
		if err := pg.InsertDts017Average(name, bts, vc); err != nil {
			log.Printf("dts017m acc backfill %s: %v", bts.Format(time.RFC3339), err)
			continue
		}
		n++
	}
	log.Printf("dts017m acc backfill: обработано %d 5-минутных бакетов", n)
}
