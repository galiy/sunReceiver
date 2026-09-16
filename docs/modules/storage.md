# Модуль хранения: Redis + PostgreSQL + аккумулятор

Схема хранения (решено 2026-09-07): **Redis — горячие данные за последние
2 календарных суток** (полное разрешение ~10 с), **PostgreSQL — вся история, но
только в виде усреднённых за 5 минут точек** (12 фиксированных промежутков в час).

Распределение ролей:

- **Поток данных**: логгер → poller → Redis (сразу, каждые 10 с). Фоновый процесс
  `runAccumulator` (`accumulator.go`) по завершении каждого 5-минутного промежутка
  ждёт отсрочку `avgDelay` (2 мин), читает из Redis накопленные снимки, усредняет
  (`averageValues`) и пишет **одну** точку в PG (`InsertAveraged`). Прямой записи в
  PG из poller нет.
- **Redis хранит только последние 2 календарных суток**: окно «сегодня» + «вчера»,
  от 00:00 вчерашнего дня (`recentCutoff(now)`). Старые точки удаляет фоновый
  процесс `runRedisCleanup` → `redisStore.PurgeOld`. HASH `current` не трогается.
- **PostgreSQL хранит всю историю** усреднёнными 5-минутными точками в
  `sunreceiver.averages` (PK `(ip, ts)`); старше двух суток — только здесь и
  никогда не удаляются.

## Redis (`redis_store.go`)

Запускается **с persistent storage**: `redis-server --port 6379 --dir <data-dir>
--save 900 1 --save 300 10 --appendonly yes` (RDB + AOF), данные в
`<data-dir>` (локально `~/.cache/sunreceiver-redis`) — не исчезают при
перезапусках. В проде Redis и PG — на сервере 253 (адреса из раздела `db`).
Клиент — `github.com/redis/go-redis/v9` (ReadTimeout 3 с / WriteTimeout 2 с —
зависший Redis не замораживает пулеры).

Ключи:

- **`sunreceiver:current`** — HASH текущих (последних) значений: поле = IP
  инвертора, значение = JSON `deviceSnapshot`. `HGETALL` отдаёт состояние всех
  инверторов — его читает дашборд для `/api/current`. Чистке старого не подлежит.
- **`sunreceiver:series:<YYYY-MM>`** — временной ряд, месячный сегмент = ZSET:
  score = Unix (сек.), member = JSON `deviceSnapshot`. Чтение периода
  (`QuerySeries`) = `ZRANGEBYSCORE` по затронутым месяцам. TTL на сегмент ~40 дней
  (запасной предохранитель; основную очистку делает `PurgeOld`).

Сохранение снимка одним циклом: `HSet(current, ip, snap)` + `ZAdd(series, epoch,
snap)` (PIPELINE/TxPipeline). `SaveSnapshotWindow` — для 10-секундных окон
(МАП/MPPT/счётчик): в пределах окна каждая новая запись **заменяет** предыдущую
(ZREM+ZADD), поэтому ровно одна строка за каждые 10 с.

`recentCutoff(now)` — момент начала вторых из последних двух календарных суток
(00:00 вчера в локальной зоне). Используется и очисткой Redis, и дашбордом для
выбора источника.

**Восстановление при пустом Redis**: `restoreRedisFromPG` при старте восстанавливает
ряды инверторов/МАП/счётчика (`pg.Averages` за окно 2 календарных суток) **и ряд
5-минутных усреднённых точек BMS** (`pg.BMSAveragesAll` →
`sunreceiver:bms:series:<YYYY-MM>`), после чего persist их сохраняет.

## PostgreSQL (`pg_store.go`)

- Таблица `sunreceiver.averages(ip, name, ts, device_sn, values jsonb)`, PK
  `(ip, ts)`, индекс по `ts`. Вставка идемпотентна (`ON CONFLICT DO NOTHING`).
- `InsertAveraged(ip, name, ts, deviceSN, vc)` — одна усреднённая точка (ts = начало
  5-минутного промежутка).
- `Averages(start, end, ips ...string)` — чтение усреднённых точек за период (для
  дашборда и реставрации Redis); необязательный `ips` ограничивает выборку.
- `MigrateLegacy()` — однократная конвертация старой таблицы сырых снимков
  `sunreceiver.snapshots` (существовала до 2026-09-07) в 5-минутные усреднённые
  точки `averages`, затем удаление `snapshots`. Идемпотентна (если таблицы нет —
  бездействует).
- `ensureSchema`/`ensureMeterTariffSchema` — создание `averages` и `daily_tariffs`.
- Тарифы счётчика: `DailyTariffsRange`, `StoreMeterBoundary` — см.
  [dds238-meter.md](../dds238-meter.md).

## Фоновые процессы (`accumulator.go`)

- `runAccumulator(store, pg, stop)` — усреднение: (1) `backfillAccumulator` разово
  конвертирует накопленные в Redis за 2 суток снимки в PG; (2) далее по завершении
  5-минутного промежутка (граница `nextBoundary`/`floorToStep`, 12 раз в час) ждёт
  `avgDelay` (2 мин, меньше `avgStep` 5 мин — цикл не дрейфует), затем
  `averageBucket` читает снимки промежутка из Redis, усредняет, пишет в PG. Граница
  фиксируется при срабатывании таймера, а сам `averageBucket` — в отдельной
  горутине; при stop дожидается завершения запущенных.
- `averageValues(snaps)` — усредняет числовые теги снимков одного инвертора и одного
  5-минутного промежутка (накопительные `energy_*` — как «репрезентативное»
  значение).
- `runRedisCleanup(store, stop)` — каждые 15 мин `PurgeOld` (удаление точек старше
  окна 2 календарных суток).

## BMS-аккумулятор (`bms_accumulator.go`)

5-минутные усреднённые точки BMS: 1-секундные снимки накапливаются **в памяти**
(BMS не имеет ряда в Redis до усреднения), по завершении промежутка пишутся в Redis
(ряд `sunreceiver:bms:series:<YYYY-MM>`, окно 2 суток) и в PG
(`sunreceiver.bms_averages`, PK `(name, ts)`, вечно). При остановке неполный
промежуток дописывается; повторная запись того же (устройство, промежуток)
**заменяет** старую — `SaveBMSSeries` (ZREM+ZADD) и `InsertBMSAveraged`
(ON CONFLICT DO UPDATE). Подробнее — [../antbms.md](../antbms.md).

## Связанные документы

- [Дашборд](dashboard.md) — читатель `current`/`series` + `Averages`.
- [dds238-meter.md](../dds238-meter.md) — `daily_tariffs`.
- [../antbms.md](../antbms.md) — ключи `sunreceiver:bms*`, `bms_averages`.