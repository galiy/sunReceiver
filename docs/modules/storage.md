# Модуль хранения: Redis + PostgreSQL + аккумулятор

Схема хранения (обновлено 2026-09-30): **Redis — горячие данные за последние
2 календарных суток, КАЖДОЕ снятое показание** (полное разрешение пулера;
инверторы ~10 с, МАП/MPPT/счётчик/ANT BMS — каждый опрос, EnBMS ~1 с, CE308 ~2 с),
**PostgreSQL — вся история, ЕДИНАЯ гранулярность 1 запись / 5 минут** для всех
рядов (инверторы, МАП/MPPT, DDS238, CE308, ANT BMS, EnBMS).

Распределение ролей:

- **Поток данных**: логгер → poller → Redis (каждое показание). Фоновый процесс
  `runAccumulator` (`accumulator.go`, для инверторов/МАП/счётчика) и отдельные
  аккумуляторы (`ce308_accumulator.go`, `bms_accumulator.go`, `enBms_accumulator.go`)
  по завершении каждого 5-минутного промежутка усредняют накопленные в Redis
  снимки и пишут **одну** точку в PG. Прямой записи сырых снимков в PG из poller нет.
- **Redis хранит только последние 2 календарных суток**: окно «сегодня» + «вчера»,
  от 00:00 вчерашнего дня (`recentCutoff(now)`). Старые точки удаляет фоновый
  процесс `runRedisCleanup` → `redisStore.PurgeOld`. HASH `current` не трогается.
- **PostgreSQL хранит всю историю** усреднёнными 5-минутными точками
  (`sunreceiver.averages` и родственные таблицы); старше двух суток — только здесь
  и никогда не удаляются.

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
snap)` (PIPELINE/TxPipeline). `SaveSnapshot` пишет **каждое** снятое показание
(дедуп только в пределах одной секунды на устройство); `SaveSnapshotMAP` — то же
для МАП/MPPT, дополнительно дополняя недостающие МАП-теги из предыдущего снимка
(гейт МАП нестабилен). 10-секундного окна/схлопывания больше нет.

`recentCutoff(now)` — момент начала вторых из последних двух календарных суток
(00:00 вчера в локальной зоне). Используется и очисткой Redis, и дашбордом для
выбора источника. Стык PG/Redis во всех series-API — общий helper
`seamWindows` (`dashboard.go`): PG `[start, cutoff−1 с]`, Redis `[cutoff, end]`
(без дубля точки на cutoff и без разрыва).

**Восстановление при пустом Redis**: `restoreRedisFromPG` при старте восстанавливает
ряды инверторов/МАП/счётчика (`pg.Averages` за окно 2 календарных суток), ряд ANT BMS
(`pg.BMSAveragesAll` → `sunreceiver:bms:series:<YYYY-MM>`) и ряд EnBMS
(`pg.EnBmsAveragesAll` → `sunreceiver:enbms:series:<YYYY-MM>`), после чего persist их
сохраняет. Ряд CE308 из PG не восстанавливается (только живой опрос).
**Нюанс**: штатный Redis-ряд BMS — сырые показания (`samples=1`), а восстановленный
из PG участок представлен 5-минутными средними (`samples>1`) — до первых новых
опросов ряд смешанный (маркер — поле `samples`).

**Объём Redis**: с 2026-09-30 ряды хранят КАЖДОЕ показание (окно 2 суток): инверторы
~10 с, МАП/MPPT/счётчик/ANT BMS — каждый опрос, EnBMS ~1 с, CE308 ~2 с. Для BMS это
до ~86 400 точек/сутки на устройство (JSON с массивами ячеек). При росте памяти
следить за `INFO memory`/размером ключей `sunreceiver:*series*`.

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

## BMS/CE308: отдельные аккумуляторы и таблицы

- **ANT BMS** (`bms_accumulator.go`): пулер пишет `current` (HASH `sunreceiver:bms`)
  и КАЖДОЕ показание в ряд `sunreceiver:bms:series:<YYYY-MM>` (`saveBMSReading`,
  окно 2 суток). 1-секундные снимки накапливаются **в памяти** и по завершении
  5-минутного промежутка пишутся в PG `sunreceiver.bms_averages` (PK `(name, ts)`,
  вечно). Неполный промежуток при остановке в PG не пишется (только полные бакеты).
  Подробнее — [../antbms.md](../antbms.md).
- **EnBMS** (`enBms_accumulator.go`): аналогично — `current` + сырой ряд
  `sunreceiver:enbms:series:<YYYY-MM>`, PG `sunreceiver.enbms_averages` (5 мин).
  См. [enbms.md](enbms.md).
- **CE308** (`ce308_accumulator.go`): сырой ряд Redis (~2 с), PG
  `sunreceiver.ce308_averages` — усреднение **5 минут** (тот же `avgStep`), см.
  [ce308.md](ce308.md).

Повторная запись того же (устройство, промежуток) в PG **заменяется**
(`InsertBMSAveraged`/`InsertCe308Average` — ON CONFLICT DO UPDATE), а в Redis-ряду
дедуп — по (устройство, секунда).

## Связанные документы

- [Дашборд](dashboard.md) — читатель `current`/`series` + `Averages`.
- [dds238-meter.md](../dds238-meter.md) — `daily_tariffs`.
- [../antbms.md](../antbms.md) — ключи `sunreceiver:bms*`, `bms_averages`.
## История ошибок устройств (`device_errors`)

Таблица `sunreceiver.device_errors(device, kind, code, msg, ts)` — история
**появления** ошибок/аварий всех типов устройств (инверторы, МАП, MPPT, счётчик,
CE308, ANT BMS, EnBMS).

Общее правило записи: **ошибка заносится только при переходе «не было → есть»**.
Если ошибка считана в текущем чтении, но уже была в предыдущем — повторно НЕ
пишется (сравнение по (устройство, код)). Дубли на каждом чтении не создаются.
Гранулярность — по конкретному устройству (`device`), тип — в `kind`; групповые
ошибки приложения тоже допустимы.

Просмотр: `/errors` (все устройства, фильтры тип/устройство/период) и страница BMS
(история конкретной BMS). Описания кодов локализованы (ru/en/zh), параметры
дописываются в текст.

Источник ошибок МАП — только «сырые» значения: через Modbus (`map.rs485.disabled=false`)
либо из Малины через `read_memory.php` (`map.rs485.disabled=true`); агрегирующие
эндпоинты-ошибки не использовать (см. [map-mppt.md](map-mppt.md)).
