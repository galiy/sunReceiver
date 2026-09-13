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
		ts, _ := time.Parse(time.RFC3339, sn.Timestamp)
		for k, raw := range sn.Values {
			f, ok := toFloat(raw)
			if !ok {
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
	snaps, err := store.QuerySeries(start, end)
	if err != nil {
		log.Printf("acc avg %s: %v", start.Format(time.RFC3339), err)
		return
	}
	insertAverageBucket(pg, start, snaps)
}

// insertAverageBucket группирует снимки по инвертору (по IP) и для каждого
// записывает одну усреднённую точку в PG (ts = start). Общий для накопителя
// (averageBucket) и бэкенд-долива (backfillAccumulator).
func insertAverageBucket(pg *pgStore, start time.Time, snaps []deviceSnapshot) {
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
	// Группируем по 5-минутному бакету (начало бакета = ключ); снимок с ts
	// строго в [бакет, бакет+5м). Внутри бакета усредняем все снимки всех
	// инверторов по IP (см. insertAverageBucket).
	type bucketKey struct {
		ip  string
		bts time.Time
	}
	groups := map[bucketKey][]deviceSnapshot{}
	for _, sn := range snaps {
		ts, perr := time.Parse(time.RFC3339, sn.Timestamp)
		if perr != nil {
			continue
		}
		if !ts.After(end) || ts.Before(start) {
			continue
		}
		groups[bucketKey{ip: sn.IP, bts: floorToStep(ts)}] = append(groups[bucketKey{ip: sn.IP, bts: floorToStep(ts)}], sn)
	}
	var n int
	for k, bucketSnaps := range groups {
		insertAverageBucket(pg, k.bts, bucketSnaps)
		n++
	}
	log.Printf("acc backfill: обработано %d 5-минутных бакетов", n)
}

// runAccumulator — фоновый процесс усреднения и записи в PostgreSQL:
//   1) конвертирует накопленные в Redis данные (первичная загрузка);
//   2) далее по завершении каждого 5-минутного промежутка (12 раз в час) НЕ
//      сразу усредняет его, а ждёт avgDelay (2 мин), чтобы собрать запаздывающие
//      снимки медленных инверторов, и уже потом читает накопленное из Redis и
//      пишет усреднённую точку в PG (см. averageBucket).
//
// Граница b фиксируется в момент срабатывания таймера (а НЕ берётся от текущего
// времени после задержки), поэтому при любых задержках в цикле усредняется именно
// завершённый промежуток [b-avgStep, b), а не смежный/незавершённый.
//
// Сам averageBucket выполняется в отдельной горутине с обработкой stop: медленное
// усреднение (QuerySeries + вставка в PG) не задерживает планировщик и не смещает
// границы следующих итераций. Так как avgDelay (2 мин) < avgStep (5 мин), между
// усреднением промежутка b (в b+2 мин) и следующей границей (b+5 мин) есть запас
// ~3 минуты — цикл не дрейфует. Останавливается по закрытию канала stop.
func runAccumulator(store *redisStore, pg *pgStore, stop <-chan struct{}) {
	if pg == nil {
		return
	}
	log.Printf("avg: старт; период=%s, отсрочка усреднения=%s, 12 промежутков в час", avgStep, avgDelay)
	backfillAccumulator(store, pg, time.Now())

	for {
		now := time.Now()
		// 1) Ждём ближайшую границу 5-минутного промежутка и фиксируем её.
		boundary := time.NewTimer(time.Until(nextBoundary(now)))
		var b time.Time
		select {
		case <-boundary.C:
			b = floorToStep(time.Now())
		case <-stop:
			stopTimer(boundary)
			return
		}

		// 2) Ждём отсрочку, чтобы собрать запаздывающие снимки промежутка
		//    [b-avgStep, b), затем усредняем в отдельной горутине.
		delay := time.NewTimer(avgDelay)
		select {
		case <-delay.C:
			start, end := b.Add(-avgStep), b
			go func() {
				averageBucket(store, pg, start, end)
			}()
		case <-stop:
			stopTimer(delay)
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