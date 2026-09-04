package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// Ключи Redis.
const (
	// redisCurrentKey — HASH текущих (последних) значений: поле=ip инвертора,
	// значение=JSON deviceSnapshot. Один HGETALL отдаёт состояние всех инверторов.
	redisCurrentKey = "sunreceiver:current"
	// redisSeriesPrefix — временной ряд. Ключи вида sunreceiver:series:<YYYY-MM>,
	// каждый — ZSET: score=Unix (сек.), member=JSON deviceSnapshot.
	// Месячная сегментация: чтение произвольного периода — это несколько ZRANGEBYSCORE
	// по затронутым месяцам, отсортированных по времени.
	redisSeriesPrefix = "sunreceiver:series:"
)

// redisStore — тонкая обёртка над клиентом Redis для текущих значений и временного ряда.
type redisStore struct {
	rdb *redis.Client
	ctx context.Context
}

// openRedis создаёт клиент Redis. Retry-логику оставляем библиотеке go-redis.
func openRedis(addr string) (*redis.Client, error) {
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		rdb.Close()
		return nil, fmt.Errorf("redis ping %s: %w", addr, err)
	}
	return rdb, nil
}

// redisSeriesKey возвращает ключ месячного сегмента временного ряда для ts.
func redisSeriesKey(ts time.Time) string {
	return redisSeriesPrefix + ts.Format("2006-01")
}

// SaveSnapshot пишет снимок в Redis одной транзакцией:
//   - обновляет текущее значение (HASH current[ip]);
//   - кладёт точку в месячный ZSET временного ряда (score = Unix-секунды).
func (s *redisStore) SaveSnapshot(snap deviceSnapshot, ts time.Time) error {
	b, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("marshal snapshot %s: %w", snap.IP, err)
	}
	key := redisSeriesKey(ts)
	pipe := s.rdb.TxPipeline()
	pipe.HSet(s.ctx, redisCurrentKey, snap.IP, b)
	pipe.ZAdd(s.ctx, key, redis.Z{Score: float64(ts.Unix()), Member: string(b)})
	// Держим хоть один месяц истории; при смене месяца эта строка оставить ключ живым.
	pipe.Expire(s.ctx, key, 40*24*time.Hour)
	_, err = pipe.Exec(s.ctx)
	if err != nil {
		return fmt.Errorf("save %s: %w", snap.IP, err)
	}
	return nil
}

// eachMonth вызывает fn для каждого года/месяца, покрывающего [start, end] включительно.
// Возвращает false, если fn хочет остановиться.
func eachMonth(start, end time.Time, fn func(y int, m time.Month) bool) {
	y, m := start.Year(), start.Month()
	for {
		t := time.Date(y, m, 1, 0, 0, 0, 0, time.Local)
		if t.After(end) {
			break
		}
		if !fn(y, m) {
			break
		}
		if m == 12 {
			y++
			m = 1
		} else {
			m++
		}
	}
}

// Current возвращает текущие снимки всех инверторов (поля HASH current) из Redis,
// отсортированные по имени для стабильного порядка на дашборде.
func (s *redisStore) Current() ([]deviceSnapshot, error) {
	m, err := s.rdb.HGetAll(s.ctx, redisCurrentKey).Result()
	if err != nil {
		return nil, fmt.Errorf("HGETALL %s: %w", redisCurrentKey, err)
	}
	snaps := make([]deviceSnapshot, 0, len(m))
	for _, v := range m {
		var snap deviceSnapshot
		if e := json.Unmarshal([]byte(v), &snap); e != nil {
			continue
		}
		snaps = append(snaps, snap)
	}
	// Сортируем по логическому имени — стабильный порядок карточек.
	for i := 1; i < len(snaps); i++ {
		for j := i; j > 0 && snaps[j].Name < snaps[j-1].Name; j-- {
			snaps[j], snaps[j-1] = snaps[j-1], snaps[j]
		}
	}
	return snaps, nil
}

// QuerySeries возвращает все снимки за период [start, end] включительно из временного ряда.
// Читает по одному месячному сегменту ZRANGEBYSCORE, объединяя в порядке времени.
func (s *redisStore) QuerySeries(start, end time.Time) ([]deviceSnapshot, error) {
	if start.After(end) {
		return nil, errors.New("start after end")
	}
	min := strconv.FormatInt(start.Unix(), 10)
	max := strconv.FormatInt(end.Unix(), 10)
	var all []deviceSnapshot
	eachMonth(start, end, func(y int, m time.Month) bool {
		key := redisSeriesKey(time.Date(y, m, 1, 0, 0, 0, 0, time.Local))
		vals, err := s.rdb.ZRangeByScore(s.ctx, key, &redis.ZRangeBy{
			Min: min,
			Max: max,
		}).Result()
		if err != nil {
			return false
		}
		for _, v := range vals {
			var snap deviceSnapshot
			if e := json.Unmarshal([]byte(v), &snap); e != nil {
				continue
			}
			all = append(all, snap)
		}
		return true
	})
	return all, nil
}