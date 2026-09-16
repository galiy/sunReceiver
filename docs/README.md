# sunReceiver

Go-приложение мониторинга домашней солнечной станции. Опрашивает инверторы
(Deye, Sofar) через их WiFi-даталоггеры (Solarman V5, TCP 8899), МАП Титанатор
(батарея/сеть), MPPT-контроллеры «КЭС» (веб-API ПАК «Малина») и электросчётчик
DDS238 (Modbus TCP), нормализует всё в единый контракт значений и хранит в Redis
(горячие данные, последние 2 календарных суток) + PostgreSQL (вся история —
5-минутные усреднённые точки). Встроенный веб-дашборд.

Язык/команды: Go 1.26, `go run .` — запуск, `go vet ./...` — проверки.
Коммиты — по-русски (как в истории репо).

## Архитектура (кратко)

Данные текут: **устройство → poller → Redis (сразу) → аккумулятор → PG (5-мин
усреднённые точки) → дашборд (чтение)**. Опрос ведут независимые горутины
(инверторы — раз в 10 с; МАП/MPPT/счётчик/BMS — раз в 1 с). Дашборд только читает
`current`/`series` (Redis) и `averages` (PG).

## Модули системы

| Модуль | Что делает | Подробное описание |
|---|---|---|
| **solarman/** | Клиент протокола Solarman V5: сборка кадра (Sofar/Deye), разбор ответа, CRC/checksum, чтение регистров, коды ошибок логгера | [modules/solarman-client.md](modules/solarman-client.md) |
| **Poller инверторов** (`main.go`) | Опрос Deye/Sofar раз в 10 с в независимых циклах, маппинг регистров в `values` (Sofar/Deye), серийные номера, чистое завершение | [modules/inverter-poller.md](modules/inverter-poller.md) |
| **Хранение** (`redis_store.go`, `pg_store.go`, `accumulator.go`, `bms_accumulator.go`) | Redis (2 суток, live) + PG (5-мин средние, вечно), фоновые аккумулятор/очистка, реставрация Redis из PG при пустом старте | [modules/storage.md](modules/storage.md) |
| **Веб-дашборд** (`dashboard.go`) | HTML + JSON API (`/`, `/charts`, `/energy`, `/bms/<name>`) поверх Redis/PG, зум/панорама, offline-индикация, mobile-раскладка | [modules/dashboard.md](modules/dashboard.md) |
| **МАП + MPPT** (`mppt_api.go`, `modbusmap/`) | МАП (батарея/сеть) через Modbus TCP или веб-API; MPPT-контроллеры через `read_json.php?device=mppt` (динамический состав) | [modules/map-mppt.md](modules/map-mppt.md) |
| **Счётчик DDS238** (`meter_*.go`) | Мгновенные значения `meter_*` + посуточные тарифы «День/Ночь» (`daily_tariffs`) с добором пропущенных границ | [dds238-meter.md](dds238-meter.md) |
| **ANT BMS** (`bms_poller.go`, `bmslistener/`) | Опрос батарей через `read_bms.php` → shm bmslistener; 5-мин усреднённые точки в Redis+PG | [antbms.md](antbms.md) |
| **Универсальный контракт `values`** | Набор общих тегов с одинаковыми именами/единицами для всех марок (PV, AC, фазы, энергия, МАП) | [universal-contract.md](universal-contract.md) |

## Что опрашивается

| Устройство | Протокол | Период | Данные |
|---|---|---|---|
| Инверторы Deye / Sofar | Solarman V5 (TCP 8899) | 10 с | PV, AC, энергия, частота, фазы, температуры |
| МАП Титанатор | Modbus TCP (502) или веб-API | 1 с | напряжение/мощность сети и батареи |
| MPPT-контроллеры «КЭС» | веб-API read_json.php (HTTP) | 1 с | PV панели, заряд АКБ, выработка за сутки |
| Счётчик DDS238 | Modbus TCP (502) | 1 с | мгновенные значения + посуточные тарифы «День/Ночь» |
| ANT BMS | веб-API read_bms.php (HTTP) | 1 с | SOC, ячейки, ток/мощность, температуры, MOS |

- **Инверторы** — раздел `invertors` конфига, нормализуются в контракт `values`
  (у каждого флаг `disabled`, обязательное поле).
- **МАП** — раздел `map`; источник задаёт обязательное поле `disabled`
  (`false`=Modbus, `true`=веб-API ПАК «Малина», нужен раздел `mppt`).
- **MPPT** — раздел `mppt` (доступ к ПАК «Малина»); состав контроллеров
  **динамический** по ответу веб-API (появляется/исчезает на дашборде).
- **Счётчик** — раздел `meter` (legacy: файл `dds238.json`).
- **BMS** — поле `mppt.bms_path`; состав батарей динамический по ответу `read_bms.php`.

## Хранение данных

- **Redis** — live-хранилище последних 2 календарных суток (полное разрешение ~10 с):
  HASH `sunreceiver:current` (последнее состояние) + месячные ZSET
  `sunreceiver:series:<YYYY-MM>`. Запускается с persistence (RDB+AOF). При полностью
  пустом Redis данные восстанавливаются из PostgreSQL. (Подробнее —
  [modules/storage.md](modules/storage.md).)
- **PostgreSQL** — вся история, только в виде **усреднённых 5-минутных точек**
  (`sunreceiver.averages`), плюс `sunreceiver.bms_averages` и
  `sunreceiver.daily_tariffs`. Фоновый процесс усредняет накопленные в Redis снимки.
- **Тарифы счётчика** — `sunreceiver.daily_tariffs` (посуточно, «День/Ночь» ×
  потребление/отдача).

## Конфигурация

Один файл **`sunReceiver.json`** рядом с бинарником (`os.Executable()`; при
`go run .` — fallback в CWD). Разделы: `invertors`, `map`, `mppt`, `db`, `meter`.
Файл приватный (пароли — в открытом виде, в `.gitignore`); публичный шаблон
структуры — [`sunReceiver.sample.json`](../sunReceiver.sample.json) (обновлять при
любом изменении структуры конфига: IP — случайные из `192.168.0.x`, серийные
номера — случайные, пароли — `CHANGE_ME`).

| Раздел | Поля |
|---|---|
| `invertors[]` | `ip`, `name`, `type` (`deye`/`sofar`), `logger_sn`, `disabled` (обязательное) |
| `map` | `name`, `ip`, `unit` (Modbus, умолч. 1), `disabled` (обязательное: `false`=Modbus, `true`=веб-API ПАК «Малина», нужен раздел `mppt`) |
| `mppt` | `base_url`, `mppt_path`, `login`, `password`, `bms_path` (включает опрос ANT BMS) |
| `db` | `redis` (host:port), `pg` (DSN с паролем) |
| `meter` | `name`, `ip`, `port`, `unit`, `first_reg`, `register_count` |

## Сборка, запуск, деплой

```sh
go build -o sunReceiver .   # сборка
./sunReceiver               # запуск (persistent-процесс)
go run .                     # запуск из исходников (конфиг из CWD)
```

Флаги: `-redis <addr>` (умолч. из конфига), `-pg <dsn>` (пустая — выключить PG),
`-pg-restore-window <dur>` (окно реставрации Redis из PG), `-dashboard <addr>`.

Прод развёрнут на внутреннем сервере (Ubuntu 22.04, amd64) как systemd-сервис
`sunreceiver.service`; PostgreSQL 16 и Redis тоже там. Адрес и деплой — приватные,
см. `AGENTS.md` (или `.kilo/AGENTS-private.md`). Деплой: собрать Linux-бинарник
(`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o sunReceiver-linux .`), scp на
сервер, `systemctl restart sunreceiver.service`.

## Документация

**Описание модулей** — [`modules/`](modules/README.md):

- [`modules/solarman-client.md`](modules/solarman-client.md) — клиент Solarman V5.
- [`modules/inverter-poller.md`](modules/inverter-poller.md) — poller инверторов.
- [`modules/storage.md`](modules/storage.md) — Redis + PG + аккумулятор.
- [`modules/dashboard.md`](modules/dashboard.md) — веб-дашборд.
- [`modules/map-mppt.md`](modules/map-mppt.md) — МАП + MPPT.

**Результаты исследований** — [`research/`](research/README.md):

- [`research/solarman-v5.md`](research/solarman-v5.md) — протокол V5: кадр, ответ,
  datafield Sofar/Deye, коды ошибок, поведение живых логгеров, находки по CRC.
- [`research/sofar-registers.md`](research/sofar-registers.md) — маппинг регистров
  Sofar K-TLX (0x0000–0x0027, 0x0105–0x0114, HW 0x2000–0x200D).
- [`research/deye-registers.md`](research/deye-registers.md) — маппинг регистров
  Deye string.
- [`malina-web-api.md`](malina-web-api.md) — устройство ПАК «Малина», shm, все
  PHP-эндпоинты.
- [`read_json.md`](read_json.md) — форматы `read_json.php?device=map|mppt|bat`.

**Прочие**:

- [`universal-contract.md`](universal-contract.md) — контракт `values` (каждый тег).
- [`dds238-meter.md`](dds238-meter.md) — модуль счётчика (мгновенные значения,
  посуточные тарифы, конфигурация).
- [`antbms.md`](antbms.md), [`antbms-worklog.md`](antbms-worklog.md) — BMS: цепочка
  bmslistener → read_bms.php, протокол, дашборд.
- `docs/map/`, `docs/antbms/` — справочные материалы реверса (байт-карты, кадры,
  команды).