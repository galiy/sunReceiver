# Модуль хранения: Redis + PostgreSQL + аккумулятор

Схема хранения (обновлено 2026-09-30): **Redis — горячие данные за последние
2 календарных суток, КАЖДОЕ снятое показание** (полное разрешение пулера;
инверторы ~10 с, МАП/MPPT/счётчик/ANT BMS — каждый опрос, EnBMS ~1 с, CE308 ~2 с),
**PostgreSQL — вся история, ЕДИНАЯ гранулярность 1 запись / 5 минут** для всех
рядов (инверторы, МАП/MPPT, DDS238, CE308, ANT BMS, EnBMS).

Распределение ролей:

- **Поток данных**: логгер → poller → Redis (каждое показание). Фоновый процесс
  `runAccumulator` (`accumulator.go`, для инверторов/МАП/счётчика) и отдельные
  аккумуляторы (`ce308_accumulator.go`, `bms_accumulator.go`, `bms26_store.go`)
  по завершении каждого 5-минутного промежутка усредняют накопленные в Redis
  снимки и пишут **одну** точку в PG. Прямой записи сырых снимков в PG из poller нет.
- **Redis хранит только последние 2 календарных суток**: окно «сегодня» + «вчера»,
  от 00:00 вчерашнего дня (`recentCutoff(now)`). Старые точки удаляет фоновый
  процесс `runRedisCleanup` → `redisStore.PurgeOld`. HASH `current` не трогается.
- **PostgreSQL хранит всю историю** усреднёнными 5-минутными точками
  (`*_averages` — марочные deye/sofar и обособленные `dds238_averages_r`/`map_averages_r`/`kes_averages_r`/`dts017m_averages_r`/`ce308_averages_r`/`bms26_averages_r`); старше двух суток — только здесь
  и никогда не удаляются.

## Redis (`redis_store.go`)

Запускается **с persistent storage**: `redis-server --port 6379 --dir <data-dir>
--save 900 1 --save 300 10 --appendonly yes` (RDB + AOF), данные в
`<data-dir>` (локально `~/.cache/sunreceiver-redis`) — не исчезают при
перезапусках. В проде Redis и PG — на сервере 253 (адреса из раздела `db`).
Клиент — `github.com/redis/go-redis/v9` (ReadTimeout 3 с / WriteTimeout 2 с —
зависший Redis не замораживает пулеры).

Ключи (сетевые инверторы **обособлены по марке**; ключ устройства — **серийный
номер инвертора** `inverter_sn`, IP — информационное поле «откуда получено»):

- **`sunreceiver:deye:current`** / **`sunreceiver:sofar:current`** — HASH текущих
  (последних) значений: поле = серийник инвертора, значение = JSON `deviceSnapshot`.
  `HGETALL` отдаёт состояние всех инверторов марки — читает дашборд. Чистке не подлежит.
- **`sunreceiver:deye:series:<YYYY-MM>`** / **`sunreceiver:sofar:series:<YYYY-MM>`** —
  временной ряд марки, месячный сегмент = ZSET: score = Unix (сек.),
  member = JSON `deviceSnapshot`. TTL на сегмент ~40 дней (предохранитель; основную
  очистку делает `PurgeOld`).
- Общие **`sunreceiver:current`** / **`sunreceiver:series:*`** **выведены из
  эксплуатации** (все классы устройств обособлены). Старые данные перенесены
  скриптами `scripts/migrate_inv_split.py` + `backfill_inv_ip.py` (плюс
  `scripts/cleanup_common_shared.py`, `scripts/fix_rudiment_sn.py`), общие ключи удалены.

Сохранение снимка одним циклом: `HSet(<марка>:current, serial, snap)` +
`ZAdd(<марка>:series, epoch, snap)` (PIPELINE/TxPipeline). `SaveSnapshot` пишет
**каждое** снятое показание (дедуп только в пределах одной секунды на устройство) и
диспетчерит марку по `deviceSnapshot.Kind` (`deye`/`sofar`).

`recentCutoff(now)` — момент начала вторых из последних двух календарных суток
(00:00 вчера в локальной зоне). Используется и очисткой Redis, и дашбордом для
выбора источника. Стык PG/Redis во всех series-API — общий helper
`seamWindows` (`dashboard.go`): PG `[start, cutoff−1 с]`, Redis `[cutoff, end]`
(без дубля точки на cutoff и без разрыва).

**Восстановление при пустом Redis**: `restoreRedisFromPG` при старте восстанавливает
ряды НЕЗАВИСИМО друг от друга: инверторы по марке (`pg.invAverages` deye/sofar за окно
2 календарных суток), МАП/счётчик/КЭС и прочие обособленные (`averagesAll`),
BMS26, DTS017M, CE308 — каждый в свой ряд `sunreceiver:*:series:<YYYY-MM>`. Сбой
одного запроса не прерывает остальные. Если хотя бы один ряд восстановить не удалось
(или пришёл сигнал остановки), ставится маркер `sunreceiver:restore:incomplete`, и
реставрация повторяется при следующем старте даже при непустом Redis; маркер снимается
после полностью успешной реставрации.
**Нюанс**: штатный Redis-ряд BMS — сырые показания (`samples=1`), а восстановленный
из PG участок представлен 5-минутными средними (`samples>1`) — до первых новых
опросов ряд смешанный (маркер — поле `samples`).

**Объём Redis**: с 2026-09-30 ряды хранят КАЖДОЕ показание (окно 2 суток): инверторы
~10 с, МАП/MPPT/счётчик/ANT BMS — каждый опрос, EnBMS ~1 с, CE308 ~2 с. Для BMS это
до ~86 400 точек/сутки на устройство (JSON с массивами ячеек). При росте памяти
следить за `INFO memory`/размером ключей `sunreceiver:*series*`.

## PostgreSQL (`pg_store.go`, `avg_r.go`)

Все 5-минутные средние хранятся в **партиционированных колоночных** таблицах
`<orig>_averages_r` (суффикс `R`): `PARTITION BY RANGE (ts)` помесячно, `PK (ключ, ts)`,
значения — типизированные колонки `double precision` (вместо `values jsonb`),
информационные метаданные — `text`. **FOREIGN KEY нет — только индексы**;
согласованность связанной группы строк (BMS) — общей транзакцией. Миграция истории
выполнена (тул `--migrate-avg`), старые jsonb-таблицы удалены.

- Инверторы: **`deye_averages_r`/`sofar_averages_r`** (`serial`, `ts`, `name`,
  `device_sn`, `ip`, 17 метрик), ключ — серийник. `insertAveraged`-ы и читатели — в
  `avg_r.go` (`insertAvgRow`/`selectAvgRows`).
- DDS238/МАП/КЭС/CE308/DTS017M/BMS26: свои `*_averages_r` (`avgSpecs`), BMS —
  parent + дети `bms26_averages_r_cells/_temps/_alarm(name, ts, idx, v)`.
- **Старые jsonb-таблицы** (`deye_averages_r`, `sofar_averages_r`, `dds238_averages_r`,
  `map_averages_r`, `kes_averages_r`, `ce308_averages_r`, `dts017m_averages_r`,
  `bms26_averages_r`) — **история до этапа миграции**; в коде не создаются/не пишутся,
  удаляются на этапе 6.
- Партиции: функция `sunreceiver.ensure_month_partition`, горизонт −1…+2 месяца
  создаётся при старте и каждые 12 ч (`runAvgPartitionMaintenance`).
- Тарифы счётчика: `DailyTariffsRange`, `StoreMeterBoundary` — см.
  [dds238-meter.md](../dds238-meter.md).
- Счётчик DTS017M: **обособленные** ключи/ряд Redis (`sunreceiver:dts017m:current`,
  `sunreceiver:dts017m:series:*`) и таблицы PG (`dts017m_averages_r`,
  `dts017m_daily_tariffs`) — [dts017m-meter.md](../dts017m-meter.md).
- Счётчик DDS238: **обособленные** ключи/ряд Redis
  (`sunreceiver:dds238:current`, `sunreceiver:dds238:series:*`) и таблицы PG
  (`dds238_averages_r`, `dds238_daily_tariffs`); логический ключ — `dds238`
  (`meterDeviceKey`). Перенос старого ключа — разовыми скриптами; в бинарнике нет.

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

- **BMS (ANT + EnBMS + malina26)** — единое хранилище bms26: текущее в
  `sunreceiver:bms26:current`, КАЖДОЕ показание в ряд `sunreceiver:bms26:series:<YYYY-MM>`,
  5-минутные средние в PG `sunreceiver.bms26_averages_r` (name = serial). Пулер
  malina26 пишет напрямую; старые пулеры ANT (`bms_poller.go`) и EnBMS
  (`enBms_poller.go`) кормят те же ряды через шимы (`bms26_legacy.go`,
  `enBms_store.go`). Подробнее — [bms26.md](bms26.md), [enbms.md](enbms.md),
  [../antbms.md](../antbms.md).
- **CE308** (`ce308_accumulator.go`): сырой ряд Redis (~2 с), PG
  `sunreceiver.ce308_averages_r` — усреднение **5 минут** (тот же `avgStep`), см.
  [ce308.md](ce308.md).

Повторная запись того же (устройство, промежуток) в PG **заменяется**
(`InsertCe308Average` и bms26-аккумулятор — ON CONFLICT DO UPDATE), а в Redis-ряду
дедуп — по (устройство, секунда).

## Связанные документы

- [Дашборд](dashboard.md) — читатель марочных `deye/sofar:current`/`:series` +
  `invAverages` (обособленные — своими читалками).
- [dds238-meter.md](../dds238-meter.md) — `dds238_daily_tariffs`.
- [../antbms.md](../antbms.md) — ключи `sunreceiver:bms26:*`, `bms26_averages_r`.
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

Источник ошибок МАП — только «сырые» значения: через Modbus (`method=rs485`)
либо из Малины через `read_memory.php` (`method=malina`); агрегирующие
эндпоинты-ошибки не использовать (см. [map-mppt.md](map-mppt.md)).
