package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
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
	// redisBMSKey — HASH текущего состояния ANT BMS (bmslistener → read_bms.php
	// ПАК «Малина»): поле = deviceName (напр. "AntBms 320 A/h"), значение = JSON
	// устройства bmsDevice. Отдельный ключ — BMS не входит в общий снимок
	// sunreceiver:current (нет универсального контракта значений). Пишется
	// BMS-пулером 1 раз в секунду (bms_poller.go), чистится при исчезновении
	// устройства из коллекции.
	redisBMSKey = "sunreceiver:bms"
	// redisBMSSeriesPrefix — временной ряд 5-минутных усреднённых точек ANT BMS
	// (bms_accumulator.go). Ключи вида sunreceiver:bms:series:<YYYY-MM>, каждый —
	// ZSET: score=Unix (сек., начало 5-минутного промежутка), member=JSON
	// bmsSeriesPoint (name + ts + значения). Хранятся последние 2 календарных
	// суток (чистка PurgeOld), как и ряд инверторов.
	redisBMSSeriesPrefix = "sunreceiver:bms:series:"
)

// redisStore — тонкая обёртка над клиентом Redis для текущих значений и временного ряда.
type redisStore struct {
	rdb *redis.Client
	ctx context.Context

	// mapWin помнит последнюю точку временного ряда каждого МАП-инвертора и её
	// 10-секундное окно, чтобы в пределах одного окна заменять её (см. SaveSnapshotWindow),
	// а не плодить дубли: в Redis остаётся ровно одна строка МАП за каждые 10 секунд.
	mapMu  sync.Mutex
	mapWin map[string]mapWinMember
}

type mapWinMember struct {
	window int64  // Unix-начало 10-секундного окна
	member string // JSON-снапшот (member ZSET)
}

// openRedis создаёт клиент Redis. Retry-логику оставляем библиотеке go-redis.
// Read/Write-таймауты ограничивают зависшую операцию (повисший Redis без
// OOM-свопа/сбоев TCP не должен замораживать всех пулеров и дашборд);
// go-redis сам переподнимает зависшее соединение.
func openRedis(addr string) (*redis.Client, error) {
	rdb := redis.NewClient(&redis.Options{
		Addr:         addr,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 2 * time.Second,
	})
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

// bmsSeriesKey возвращает ключ месячного сегмента ряда 5-минутных усреднённых
// точек BMS для ts.
func bmsSeriesKey(ts time.Time) string {
	return redisBMSSeriesPrefix + ts.Format("2006-01")
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

// mergeMAPSnap дополняет снимок устройства МАП недостающими МАП-тегами
// (grid_voltage, grid_power, battery_voltage, battery_power) значениями из
// предыдущего снимка prevMember этого же устройства. Нужно из-за того, что гейт
// МАП нестабильно отдаёт блоки ячеек: тег может отсутствовать в части кадров.
// Возвращает снимок с полным набором последних известных значений МАП.
func mergeMAPSnap(snap deviceSnapshot, prevMember string) deviceSnapshot {
	if prevMember == "" {
		return snap
	}
	var prev deviceSnapshot
	if err := json.Unmarshal([]byte(prevMember), &prev); err != nil {
		return snap
	}
	out := valuesContract{}
	for k, v := range snap.Values {
		out[k] = v
	}
	for _, tag := range []string{"grid_voltage", "grid_power", "battery_voltage", "battery_power"} {
		if _, ok := out[tag]; !ok {
			if pv, ok2 := prev.Values[tag]; ok2 {
				out[tag] = pv
			}
		}
	}
	snap.Values = out
	return snap
}

// SaveSnapshotWindow — специальная запись для целей kindMAP: снимок пишется с
// score = начало 10-секундного окна (ts.Truncate(10s)), а не с точной секундой.
// В пределах одного окна каждая новая запись ЗАМЕНЯЕТ предыдущую точку этого же
// окна (ZREM старого member + ZADD нового), поэтому в Redis по каждому МАП
// остаётся ровно одна строка за 10 секунд. Текущее значение (HASH current)
// обновляется всегда — на каждый опрос.
func (s *redisStore) SaveSnapshotWindow(snap deviceSnapshot, ts time.Time) error {
	window := ts.Truncate(10 * time.Second)
	winUnix := window.Unix()

	// Для устройства МАП (kindMAP) недостающие МАП-теги (grid_voltage, grid_power,
	// battery_voltage, battery_power) дополняем из предыдущего снимка этого же
	// устройства: гейт МАП нестабильно отдаёт блоки, поэтому последнее известное
	// значение сохраняется, а на плашках/графиках дашборда не бывает прочерка.
	merged := snap
	if isMAPDevice(snap.Values) {
		prevMember := ""
		s.mapMu.Lock()
		if p, had := s.mapWin[snap.IP]; had {
			prevMember = p.member
		}
		s.mapMu.Unlock()
		merged = mergeMAPSnap(snap, prevMember)
	}

	b, err := json.Marshal(merged)
	if err != nil {
		return fmt.Errorf("marshal snapshot %s: %w", snap.IP, err)
	}
	member := string(b)
	key := redisSeriesKey(window)

	var prev mapWinMember
	had := false
	s.mapMu.Lock()
	if s.mapWin == nil {
		s.mapWin = map[string]mapWinMember{}
	}
	prev, had = s.mapWin[snap.IP]
	s.mapWin[snap.IP] = mapWinMember{window: winUnix, member: member}
	s.mapMu.Unlock()

	pipe := s.rdb.TxPipeline()
	if had && prev.window == winUnix {
		// то же 10-секундное окно — стираем предыдущую точку и заменяем на актуальную
		pipe.ZRem(s.ctx, key, prev.member)
	}
	pipe.ZAdd(s.ctx, key, redis.Z{Score: float64(winUnix), Member: member})
	pipe.HSet(s.ctx, redisCurrentKey, snap.IP, b)
	pipe.Expire(s.ctx, key, 40*24*time.Hour)
	_, err = pipe.Exec(s.ctx)
	if err != nil {
		return fmt.Errorf("save window %s: %w", snap.IP, err)
	}
	return nil
}

// PruneMPPT удаляет из HASH current все MPPT-ключи (содержащие "#mppt"), которых
// нет в active (множество актуальных devKey каждого MPPT, присутствующем в ответе
// ПАК «Малина»). Нужно, чтобы исчезнувшие с МАП контроллеры переставали считаться
// «актуальными» на дашборде, но их история во временном ряду сохранялась.
func (s *redisStore) PruneMPPT(active map[string]struct{}) {
	m, err := s.rdb.HGetAll(s.ctx, redisCurrentKey).Result()
	if err != nil {
		log.Printf("redis prune mppt HGETALL: %v", err)
		return
	}
	var keys []string
	for ip := range m {
		if !strings.Contains(ip, "#mppt") {
			continue
		}
		if _, ok := active[ip]; ok {
			continue
		}
		keys = append(keys, ip)
	}
	if len(keys) == 0 {
		return
	}
	if err := s.rdb.HDel(s.ctx, redisCurrentKey, keys...).Err(); err != nil {
		log.Printf("redis prune mppt del %v: %v", keys, err)
	}
	// Чистим в памяти mapWin по удалённым MPPT-ключам (иначе карта растёт при
	// ротации контроллеров).
	s.mapMu.Lock()
	for _, k := range keys {
		delete(s.mapWin, k)
	}
	s.mapMu.Unlock()
}

// SetBMS обновляет коллекцию BMS-устройств в HASH sunreceiver:bms одной
// транзакцией: пишет все активные (поле = deviceName, значение = JSON bmsDevice)
// и удаляет те, которых нет в active (устройство исчезло из коллекции —
// адаптер отключился/замолчал, bmslistener уже забыл его).
func (s *redisStore) SetBMS(active map[string]string) error {
	cur, err := s.rdb.HGetAll(s.ctx, redisBMSKey).Result()
	if err != nil {
		return fmt.Errorf("bms HGETALL: %w", err)
	}
	var stale []string
	for k := range cur {
		if _, ok := active[k]; !ok {
			stale = append(stale, k)
		}
	}
	pipe := s.rdb.TxPipeline()
	for k, v := range active {
		pipe.HSet(s.ctx, redisBMSKey, k, v)
	}
	if len(stale) > 0 {
		pipe.HDel(s.ctx, redisBMSKey, stale...)
	}
	if _, err := pipe.Exec(s.ctx); err != nil {
		return fmt.Errorf("bms set: %w", err)
	}
	return nil
}

// BMSCurrent возвращает текущее состояние всех BMS-устройств (поле = deviceName,
// значение = JSON bmsDevice).
func (s *redisStore) BMSCurrent() (map[string]string, error) {
	m, err := s.rdb.HGetAll(s.ctx, redisBMSKey).Result()
	if err != nil {
		return nil, fmt.Errorf("bms HGETALL: %w", err)
	}
	return m, nil
}

// BMSOne возвращает текущее состояние одного BMS-устройства по deviceName;
// пустая строка, если устройство отсутствует.
func (s *redisStore) BMSOne(name string) (string, error) {
	v, err := s.rdb.HGet(s.ctx, redisBMSKey, name).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	return v, err
}

// SaveBMSSeries кладёт одну 5-минутную усреднённую точку BMS в месячный ZSET
// ряда (score = Unix-секунды начала промежутка). Хранение — последние 2
// календарных суток (чистка PurgeOld), как и ряд инверторов.
//
// Тот же (устройство, промежуток) может записываться дважды: при остановке
// пулера дописывается неполный промежуток (drain), а новый процесс пишет
// продолжение того же промежутка. Голый ZADD оставил бы в ZSET два разных
// member с одинаковым score — две точки в один и тот же момент времени на
// графике («ступенька»). Поэтому перед записью удаляются старые версии того
// же устройства на этом score: в Redis остаётся ровно одна точка на
// (устройство, 5-минутный промежуток), последняя (более полная) запись
// побеждает.
func (s *redisStore) SaveBMSSeries(p bmsSeriesPoint, ts time.Time) error {
	b, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("marshal bms series %s: %w", p.Name, err)
	}
	key := bmsSeriesKey(ts)
	score := strconv.FormatInt(ts.Unix(), 10)
	// Старые версии этого же устройства на том же моменте (score): ищем среди
	// member'ов с точным score, разбираем и собираем для удаления.
	old, err := s.rdb.ZRangeByScore(s.ctx, key, &redis.ZRangeBy{Min: score, Max: score}).Result()
	if err != nil {
		return fmt.Errorf("bms series dedup %s: %w", key, err)
	}
	var stale []any
	for _, m := range old {
		var q bmsSeriesPoint
		if json.Unmarshal([]byte(m), &q) != nil {
			continue
		}
		if q.Name == p.Name {
			stale = append(stale, m)
		}
	}
	pipe := s.rdb.TxPipeline()
	if len(stale) > 0 {
		pipe.ZRem(s.ctx, key, stale...)
	}
	pipe.ZAdd(s.ctx, key, redis.Z{Score: float64(ts.Unix()), Member: string(b)})
	pipe.Expire(s.ctx, key, 40*24*time.Hour)
	_, err = pipe.Exec(s.ctx)
	if err != nil {
		return fmt.Errorf("save bms series %s: %w", p.Name, err)
	}
	return nil
}

// QueryBMSSeries возвращает 5-минутные усреднённые точки одной BMS (по
// deviceName) за период [start, end] включительно из Redis-ряда, по
// возрастанию времени. Читает месячные сегменты ZRANGEBYSCORE (в пределах
// окна удержания 2 календарных суток); отфильтрованные по имени точки —
// точный срез для графиков конкретной BMS.
func (s *redisStore) QueryBMSSeries(name string, start, end time.Time) ([]bmsSeriesPoint, error) {
	if start.After(end) {
		return nil, errors.New("start after end")
	}
	min := strconv.FormatInt(start.Unix(), 10)
	max := strconv.FormatInt(end.Unix(), 10)
	var all []bmsSeriesPoint
	eachMonth(start, end, func(y int, m time.Month) bool {
		key := bmsSeriesKey(time.Date(y, m, 1, 0, 0, 0, 0, time.Local))
		vals, err := s.rdb.ZRangeByScore(s.ctx, key, &redis.ZRangeBy{
			Min: min,
			Max: max,
		}).Result()
		if err != nil {
			return false
		}
		for _, v := range vals {
			var p bmsSeriesPoint
			if e := json.Unmarshal([]byte(v), &p); e != nil {
				continue
			}
			if p.Name != name {
				continue
			}
			all = append(all, p)
		}
		return true
	})
	return all, nil
}

// eachMonth вызывает fn для каждого года/месяца, покрывающего [start, end] включительно.// Возвращает false, если fn хочет остановиться.
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

// scanPrefixKeys возвращает все ключи с данным префиксом через SCAN — не
// блокирует Redis в отличие от KEYS.
func (s *redisStore) scanPrefixKeys(prefix string) ([]string, error) {
	var (
		keys   []string
		cursor uint64
	)
	for {
		batch, next, err := s.rdb.Scan(s.ctx, cursor, prefix, 200).Result()
		if err != nil {
			return nil, fmt.Errorf("scan keys %s: %w", prefix, err)
		}
		keys = append(keys, batch...)
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return keys, nil
}

// scanSeriesKeys возвращает все месячные ключи временного ряда инверторов
// (prefix*) — для проверки пустоты (IsEmpty).
func (s *redisStore) scanSeriesKeys() ([]string, error) {
	return s.scanPrefixKeys(redisSeriesPrefix + "*")
}

// IsEmpty возвращает true, если в Redis нет ни текущего состояния, ни одного
// сегмента временного ряда (т.е. in-memory данные потеряны и нужна реставрация
// из persistent-хранилища PostgreSQL).
func (s *redisStore) IsEmpty() (bool, error) {
	n, err := s.rdb.HLen(s.ctx, redisCurrentKey).Result()
	if err != nil {
		return false, err
	}
	if n > 0 {
		return false, nil
	}
	keys, err := s.scanSeriesKeys()
	if err != nil {
		return false, err
	}
	return len(keys) == 0, nil
}

// recentCutoff возвращает момент начала вторых из последних двух календарных
// суток (в локальной зоне). Redis хранит данные за последние 2 календарных
// суток: «сегодня» и «вчера», т.е. начиная с 00:00 вчерашнего дня. Данные
// строго старше cutofа удаляются фоновой очисткой и не читаются из Redis.
func recentCutoff(t time.Time) time.Time {
	y, m, d := t.In(time.Local).Date()
	startOfToday := time.Date(y, m, d, 0, 0, 0, 0, time.Local)
	return startOfToday.AddDate(0, 0, -1)
}

// PurgeOld удаляет из временных рядов Redis (месячные сегменты инверторов и
// 5-минутных усреднённых точек BMS) все точки, timestamp которых строго
// старше окна последних 2 календарных суток (recentCutoff). Пустые сегменты
// удаляются целиком. Текущие HASH (current, sunreceiver:bms) не трогается —
// последнее состояние устройств хранится всегда.
// Вызывается фоновым процессом (см. runRedisCleanup).
func (s *redisStore) PurgeOld(now time.Time) {
	cutoff := recentCutoff(now)
	keys, err := s.scanSeriesKeys()
	if err != nil {
		log.Printf("redis cleanup keys: %v", err)
		return
	}
	// Ряд 5-минутных усреднённых точек BMS чистится тем же окном.
	bmsKeys, err := s.scanPrefixKeys(redisBMSSeriesPrefix + "*")
	if err != nil {
		log.Printf("redis cleanup bms keys: %v", err)
	} else {
		keys = append(keys, bmsKeys...)
	}
	// Операцию выполняем так, чтобы «строго старше cutoff», т.е. ZRemRangeByScore
	// убирает [ -inf ; cutoff-1 ], поэтому ровно cutoff остаётся в ряде.
	remBelow := strconv.FormatInt(cutoff.Unix()-1, 10)
	for _, key := range keys {
		if err := s.rdb.ZRemRangeByScore(s.ctx, key, "-inf", remBelow).Err(); err != nil {
			log.Printf("redis cleanup %s: %v", key, err)
			continue
		}
		n, err := s.rdb.ZCard(s.ctx, key).Result()
		if err != nil {
			continue
		}
		if n == 0 {
			if err := s.rdb.Del(s.ctx, key).Err(); err != nil {
				log.Printf("redis cleanup del %s: %v", key, err)
			}
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

// CurrentOne возвращает один последний снимок из current-HASH по IP
// (для донаса аппаратных серийных номеров в runInverterPoll).
func (s *redisStore) CurrentOne(ip string) (deviceSnapshot, error) {
	v, err := s.rdb.HGet(s.ctx, redisCurrentKey, ip).Result()
	if err != nil {
		return deviceSnapshot{}, err
	}
	var snap deviceSnapshot
	if e := json.Unmarshal([]byte(v), &snap); e != nil {
		return deviceSnapshot{}, e
	}
	return snap, nil
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