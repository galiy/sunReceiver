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

// Особенность обработки истории CE308: мгновенные значения пишутся в Redis при
// каждом опросе (~1 точка за 2 с), а в PostgreSQL усредняются до дискретности
// 1 запись за 10 секунд (в отличие от 5-минутных агрегатов инверторов). Поэтому
// для CE308 — собственный аккумулятор (ce308_accumulator.go) с шагом ce308AvgStep
// и собственной таблицей PG (sunreceiver.ce308_averages).

// ce308AvgStep — дискретность усреднения истории CE308 в PostgreSQL: 10 секунд.
const ce308AvgStep = 10 * time.Second

// ce308AvgDelay — отсрочка усреднения завершённого 10-секундного промежутка
// после его границы, чтобы собрать «запаздывающие» снимки (опрос ~2 с + время
// чтения BLE ~4-5 с). Меньше ce308AvgStep, поэтому цикл не дрейфует.
const ce308AvgDelay = 10 * time.Second

// averageCe308 усредняет мгновенные снимки CE308 одного 10-секундного
// промежутка в одну точку: каждое значение — среднее по снимкам (округление
// до 1 знака). Промежуток обычно содержит несколько точек (~2 с), поэтому
// честное усреднение, а не «последнее значение».
func averageCe308(snaps []ce308Snapshot) map[string]float64 {
	sums := map[string]float64{}
	counts := map[string]float64{}
	for _, sn := range snaps {
		for k, v := range sn.Values {
			sums[k] += v
			counts[k]++
		}
	}
	out := map[string]float64{}
	for k, n := range counts {
		if n == 0 {
			continue
		}
		out[k] = ce308Round1(sums[k] / n)
	}
	return out
}

// runCe308Accumulator — фоновый процесс усреднения истории CE308 в PostgreSQL.
// По завершении каждого 10-секундного промежутка (не сразу, а спустя
// ce308AvgDelay) читает снимки промежутка из Redis-ряда и пишет одну усреднённую
// точку в sunreceiver.ce308_averages (ts = начало промежутка).
func runCe308Accumulator(store *redisStore, pg *pgStore, name string, ctx context.Context) {
	if pg == nil {
		return
	}
	log.Printf("ce308: avg старт; период=%s, отсрочка=%s, 10-секундные промежутки", ce308AvgStep, ce308AvgDelay)
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		now := time.Now()
		target := now.Truncate(ce308AvgStep).Add(ce308AvgStep)
		boundary := time.NewTimer(time.Until(target))
		select {
		case <-boundary.C:
		case <-ctx.Done():
			stopTimer(boundary)
			return
		}
		delay := time.NewTimer(ce308AvgDelay)
		select {
		case <-delay.C:
		case <-ctx.Done():
			stopTimer(delay)
			return
		}
		start := target.Add(-ce308AvgStep)
		end := target
		wg.Add(1)
		go func() {
			defer wg.Done()
			snaps, err := store.QueryCE308Series(start, end.Add(-time.Second))
			if err != nil {
				log.Printf("ce308: avg %s: %v", start.Format(time.RFC3339), err)
				return
			}
			if len(snaps) == 0 {
				return
			}
			vc := averageCe308(snaps)
			if len(vc) == 0 {
				return
			}
			if err := retryPg(func() error {
				return pg.InsertCe308Average(name, start, vc)
			}, 3); err != nil {
				log.Printf("ce308: avg pg %s: %v", start.Format(time.RFC3339), err)
			}
		}()
	}
}
