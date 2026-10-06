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
	"log"
	"math"
	"sync"
	"time"
)

// avgStep — дискретность усреднения данных в PostgreSQL: 12 фиксированных
// 5-минутных промежутков в час (0:00, 0:05, ..., 0:55).
const avgStep = 5 * time.Minute

// avgDelay — отсрочка усреднения завершённого промежутка после его границы.
// Усредняем не сразу по завершении периода, а спустя эту задержку, чтобы успеть
// собрать «запаздывающие» снимки медленных инверторов (логгеры шлют кадры с
// паузами/pacing, ответ может прийти чуть позже границы). Меньше avgStep
// (2 мин < 5 мин), поэтому цикл не дрейфует относительно границ.
const avgDelay = 2 * time.Minute

// floorToStep возвращает начало (down to nearest) 5-минутного промежутка для t.
func floorToStep(t time.Time) time.Time {
	return t.Truncate(avgStep)
}

// nextBoundary — ближайшая граница 5-минутного промежутка строго после now.
func nextBoundary(now time.Time) time.Time {
	return floorToStep(now).Add(avgStep)
}

// toFloat извлекает числовое значение из универсального контракта.
func toFloat(raw any) (float64, bool) {
	switch n := raw.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

// accumulatorTags — монотонно возрастающие счётчики (энергия/показания).
// Усреднение по окну занижало бы их значение (в среднем попадаем на начало окна),
// поэтому для них усреднение заменяем на последнее по времени значение окна.
var accumulatorTags = map[string]bool{
	"energy_total": true,
	"energy_today": true,
	"meter_import": true,
	"meter_export": true,
	"meter_total":  true,
}

// accumulatorSkipTags — теги, которые НЕ попадают в усреднённые точки PostgreSQL.
// Это брендовые температуры (temperatureTags): они актуальны только для живого
// снимка (current) и не имеют смысла как 5-минутное среднее (датчик один, значение
// почти постоянно, а история температур в PG не нужна — рынок держит 2 суток в
// Redis, дальше курить незачем).
var accumulatorSkipTags = func() map[string]bool {
	m := map[string]bool{}
	for _, t := range temperatureTags {
		m[t] = true
	}
	return m
}()

// averageValues усредняет все числовые теги набора снимков одного инвертора и
// одного 5-минутного промежутка в одну точку для записи в PostgreSQL.
// Так как values содержит только числовые теги общего контракта, усредняется
// каждый ключ; частотные/напряженские величины усредняются честно, а
// накопительные счётчики (accumulatorTags) — по последнему значению окна.
func averageValues(snaps []deviceSnapshot) valuesContract {
	sums := map[string]float64{}
	counts := map[string]int{}
	// Накопительные счётчики: держим значение снимка с наибольшим timestamp.
	lastTS := map[string]time.Time{}
	lastVal := map[string]float64{}
	for _, sn := range snaps {
		ts, perr := time.Parse(time.RFC3339, sn.Timestamp)
		if perr != nil {
			continue
		}
		for k, raw := range sn.Values {
			f, ok := toFloat(raw)
			if !ok {
				continue
			}
			if accumulatorSkipTags[k] {
				continue
			}
			if accumulatorTags[k] {
				if ts.After(lastTS[k]) {
					lastTS[k] = ts
					lastVal[k] = f
				}
				continue
			}
			sums[k] += f
			counts[k]++
		}
	}
	out := valuesContract{}
	for k, n := range counts {
		if n == 0 {
			continue
		}
		out[k] = math.Round(sums[k]/float64(n)*10) / 10
	}
	for k, v := range lastVal {
		out[k] = math.Round(v*10) / 10
	}
	return out
}

// averageBucket читает из Redis снимки за промежуток [start, end) и записывает
// в PostgreSQL по одной усреднённой точке на инвертор (ts = start).
func averageBucket(store *redisStore, pg *pgStore, start, end time.Time) {
	snaps, err := readBucketWindow(store, start, end)
	if err != nil {
		log.Printf("acc avg %s: %v", start.Format(time.RFC3339), err)
		return
	}
	insertAverageBucket(pg, start, snaps)
}

// readBucketWindow читает из Redis снимки за промежуток [start, end). Правый
// конец исключительный (QuerySeries(start, end−1с)): точка ровно на границе end
// принадлежит СЛЕДУЮЩЕМУ бакету (floorToStep(end) == end) — согласовано с
// группировкой backfillAccumulator и без двойного счёта (QuerySeries сам
// включителен с обоих концов).
func readBucketWindow(store *redisStore, start, end time.Time) ([]deviceSnapshot, error) {
	return store.QuerySeries(start, end.Add(-time.Second))
}

// insertAverageBucket группирует снимки по инвертору (по IP) и для каждого
// записывает одну усреднённую точку в PG (ts = start). Общий для накопителя
// (averageBucket) и бэкенд-долива (backfillAccumulator). Весь набор точек одного
// бакета пишется ОДНОЙ транзакцией с ограниченным retry (см. retryPg): либо все
// точки инверторов промежутка, либо ни одной — кратковременный сбой PG не
// оставляет частично записанный бакет («дыру»).
func insertAverageBucket(pg *pgStore, start time.Time, snaps []deviceSnapshot) {
	if len(snaps) == 0 {
		return
	}
	byIP := map[string][]deviceSnapshot{}
	for _, sn := range snaps {
		byIP[sn.IP] = append(byIP[sn.IP], sn)
	}
	type row struct {
		ip, name, deviceSN string
		vc                 valuesContract
	}
	rows := make([]row, 0, len(byIP))
	for ip, group := range byIP {
		vc := averageValues(group)
		if len(vc) == 0 {
			continue
		}
		rows = append(rows, row{ip: ip, name: group[0].Name, deviceSN: group[0].DeviceSN, vc: vc})
	}
	if len(rows) == 0 {
		return
	}
	if err := retryPg(func() error {
		return pg.withTx(func(q pgExecer) error {
			for _, r := range rows {
				if err := insertAveragedExec(q, pg.ctx, r.ip, r.name, start, r.deviceSN, r.vc); err != nil {
					return err
				}
			}
			return nil
		})
	}, 3); err != nil {
		log.Printf("acc pg %s: %v", start.Format(time.RFC3339), err)
	}
}

// backfillAccumulator конвертирует уже накопленные в Redis данные (за последние
// 2 календарных суток, окно удержания Redis) в 5-минутные усреднённые точки PG.
// Вызывается однократно при старте, чтобы промежутки до текущего аккумулирования
// не потерялись при переходе на новый режим. Читает ОДНИМ QuerySeries за всё
// окно и группирует снимки по (ip, 5-минутный промежуток) в памяти, вместо того
// чтобы ходить в Redis по каждому 5-минутному бакету (~576 раз за 2 суток).
func backfillAccumulator(store *redisStore, pg *pgStore, now time.Time) {
	if pg == nil {
		return
	}
	end := floorToStep(now)
	start := recentCutoff(now)
	if !start.Before(end) {
		return
	}
	snaps, err := store.QuerySeries(start, end)
	if err != nil {
		log.Printf("acc backfill: %v", err)
		return
	}
	groups := groupForBackfill(snaps, start, end)
	var n int
	for k, bucketSnaps := range groups {
		insertAverageBucket(pg, k.bts, bucketSnaps)
		n++
	}
	log.Printf("acc backfill: обработано %d 5-минутных бакетов", n)
}

// accBucketKey — ключ группировки backfill: (устройство, начало 5-минутного бакета).
type accBucketKey struct {
	ip  string
	bts time.Time
}

// groupForBackfill группирует снимки по 5-минутному бакету (floorToStep) в
// строгом окне [start, end): снимок с ts == end (начало текущего незавершённого
// бакета) пропускается — его допишет целиком живой цикл, а не backfill (иначе
// backfill записал бы одно-точечную версию незавершённого бакета). Чистая
// функция (без Redis/PG) — тестируется напрямую.
func groupForBackfill(snaps []deviceSnapshot, start, end time.Time) map[accBucketKey][]deviceSnapshot {
	groups := map[accBucketKey][]deviceSnapshot{}
	for _, sn := range snaps {
		ts, perr := time.Parse(time.RFC3339, sn.Timestamp)
		if perr != nil {
			continue
		}
		if ts.Before(start) || !ts.Before(end) {
			continue
		}
		k := accBucketKey{ip: sn.IP, bts: floorToStep(ts)}
		groups[k] = append(groups[k], sn)
	}
	return groups
}

// runAccumulator — фоновый процесс усреднения и записи в PostgreSQL:
//  1. конвертирует накопленные в Redis данные (первичная загрузка);
//  2. далее по завершении каждого 5-минутного промежутка (12 раз в час) НЕ
//     сразу усредняет его, а ждёт avgDelay (2 мин), чтобы собрать запаздывающие
//     снимки медленных инверторов, и уже потом читает накопленное из Redis и
//     пишет усреднённую точку в PG (см. averageBucket).
//
// Целевая граница (target = nextBoundary) фиксируется ДО ожидания таймера и
// используется как b, а НЕ берётся от time.Now() после задержки: если таймер
// сработал с большой задержкой (сна/подвес машины, GC-пауза), time.Now() уже
// мог пересечь следующую границу и b уехал бы вперёд (усреднился бы ещё не
// завершённый бакет). После задержки усредняются ВСЕ завершённые на данный
// момент границы, начиная с target (в норме ровно одна; после сна/задержки —
// несколько), каждая — [b-avgStep, b).
//
// Сам averageBucket выполняется в отдельной горутине с обработкой stop: медленное
// усреднение (QuerySeries + вставка в PG) не задерживает планировщик и не смещает
// границы следующих итераций. Так как avgDelay (2 мин) < avgStep (5 мин), между
// усреднением промежутка b (в b+2 мин) и следующей границей (b+5 мин) есть запас
// ~3 минуты — цикл не дрейфует. Останавливается по закрытию канала stop.
func runAccumulator(store *redisStore, pg *pgStore, ctx context.Context) {
	if pg == nil {
		return
	}
	log.Printf("avg: старт; период=%s, отсрочка усреднения=%s, 12 промежутков в час", avgStep, avgDelay)
	backfillAccumulator(store, pg, time.Now())

	// bucketWg — запущенные, но ещё не завершённые averageBucket: при stop
	// ждём их завершения, чтобы main закрыл пул PG только после последней записи.
	var bucketWg sync.WaitGroup
	waitBuckets := func() { bucketWg.Wait() }

	for {
		now := time.Now()
		// 1) Целевая граница, на которую ждём (фиксируем ДО таймера).
		target := nextBoundary(now)
		boundary := time.NewTimer(time.Until(target))
		select {
		case <-boundary.C:
			// граница наступила
		case <-ctx.Done():
			stopTimer(boundary)
			waitBuckets()
			return
		}

		// 2) Ждём отсрочку, чтобы собрать запаздывающие снимки.
		delay := time.NewTimer(avgDelay)
		select {
		case <-delay.C:
			// Все ЗАВЕРШЁННЫЕ на данный момент границы, начиная с target
			// (в норме ровно одна; после сна/задержки — несколько).
			for b := target; ; b = b.Add(avgStep) {
				if b.After(floorToStep(time.Now())) {
					break
				}
				start := b.Add(-avgStep)
				bucketWg.Add(1)
				go func(start, end time.Time) {
					defer bucketWg.Done()
					averageBucket(store, pg, start, end)
				}(start, b)
			}
		case <-ctx.Done():
			stopTimer(delay)
			waitBuckets()
			return
		}
	}
}

// stopTimer останавливает таймер и, если сигнал уже в канале, вычитывает его
// (иначе срабатывание таймера могло бы утечь и повлиять на следующий select).
func stopTimer(t *time.Timer) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
}

// runRedisCleanup — фоновый процесс очистки старых данных Redis: удаляет точки
// временного ряда старше последних 2 календарных суток (см. redisStore.PurgeOld).
func runRedisCleanup(store *redisStore, ctx context.Context) {
	clean := func() { store.PurgeOld(time.Now()) }
	clean()
	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			clean()
		case <-ctx.Done():
			return
		}
	}
}
