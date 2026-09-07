package main

import (
	"encoding/json"
	"log"
	"math"
	"time"
)

// avgStep — дискретность усреднения данных в PostgreSQL: 12 фиксированных
// 5-минутных промежутков в час (0:00, 0:05, ..., 0:55).
const avgStep = 5 * time.Minute

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

// averageValues усредняет все числовые теги набора снимков одного инвертора и
// одного 5-минутного промежутка в одну точку для записи в PostgreSQL.
// Так как values содержит только числовые теги общего контракта, усредняется
// каждый ключ; частотные/напряженские величины усредняются честно, а накопительные
// счётчики (energy_*) — как «репрезентативное» значение промежутка (для графиков
// мощности и так используется только ac_active_power).
func averageValues(snaps []deviceSnapshot) valuesContract {
	sums := map[string]float64{}
	counts := map[string]int{}
	for _, sn := range snaps {
		for k, raw := range sn.Values {
			f, ok := toFloat(raw)
			if !ok {
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
	return out
}

// averageBucket читает из Redis снимки за промежуток [start, end) и записывает
// в PostgreSQL по одной усреднённой точке на инвертор (ts = start).
func averageBucket(store *redisStore, pg *pgStore, start, end time.Time) {
	snaps, err := store.QuerySeries(start, end)
	if err != nil {
		log.Printf("acc avg %s: %v", start.Format(time.RFC3339), err)
		return
	}
	if len(snaps) == 0 {
		return
	}
	byIP := map[string][]deviceSnapshot{}
	for _, sn := range snaps {
		byIP[sn.IP] = append(byIP[sn.IP], sn)
	}
	for ip, group := range byIP {
		vc := averageValues(group)
		if len(vc) == 0 {
			continue
		}
		if err := pg.InsertAveraged(ip, group[0].Name, start, group[0].DeviceSN, vc); err != nil {
			log.Printf("acc pg %s: %v", ip, err)
		}
	}
}

// backfillAccumulator конвертирует уже накопленные в Redis данные (за последние
// 2 календарных суток, окно удержания Redis) в 5-минутные усреднённые точки PG.
// Вызывается однократно при старте, чтобы промежутки до текущего аккумулирования
// не потерялись при переходе на новый режим.
func backfillAccumulator(store *redisStore, pg *pgStore, now time.Time) {
	if pg == nil {
		return
	}
	end := floorToStep(now)
	start := recentCutoff(now)
	for cur := floorToStep(start); cur.Before(end); cur = cur.Add(avgStep) {
		bucketEnd := cur.Add(avgStep)
		if bucketEnd.After(end) {
			break
		}
		averageBucket(store, pg, cur, bucketEnd)
	}
}

// runAccumulator — фоновый процесс усреднения и записи в PostgreSQL:
//   1) конвертирует накопленные в Redis данные (первичная загрузка);
//   2) далее по завершении каждого 5-минутного промежутка (12 раз в час)
//      читает накопленное из Redis и пишет усреднённую точку в PG.
// Останавливается по закрытию канала stop.
func runAccumulator(store *redisStore, pg *pgStore, stop <-chan struct{}) {
	if pg == nil {
		return
	}
	log.Printf("avg: старт; период=%s, 12 промежутков в час", avgStep)
	backfillAccumulator(store, pg, time.Now())

	for {
		now := time.Now()
		timer := time.NewTimer(time.Until(nextBoundary(now)))
		select {
		case <-timer.C:
			b := floorToStep(time.Now())
			averageBucket(store, pg, b.Add(-avgStep), b)
		case <-stop:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		}
	}
}

// runRedisCleanup — фоновый процесс очистки старых данных Redis: удаляет точки
// временного ряда старше последних 2 календарных суток (см. redisStore.PurgeOld).
func runRedisCleanup(store *redisStore, stop <-chan struct{}) {
	clean := func() { store.PurgeOld(time.Now()) }
	clean()
	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			clean()
		case <-stop:
			return
		}
	}
}