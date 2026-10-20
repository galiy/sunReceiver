# Отказ от ответственности

Это программное обеспечение предоставляется «как есть», без каких-либо гарантий,
явных или подразумеваемых, включая (но не ограничиваясь) гарантии товарной
пригодности, соответствия конкретным целям и отсутствия нарушений. Авторы не
несут ответственности за любой ущерб, потери или последствия, прямо или
косвенно связанные с использованием этого ПО, в том числе за работу подключённого
оборудования (инверторы, батареи, счётчики), достоверность данных, потерю данных
или некорректную работу мониторинга. Используете на свой риск.

---

# sunReceiver

Go-приложение мониторинга домашней солнечной станции. Получает данные сетевых
инверторов (Deye, Sofar) через HTTP-API «Малина26», опрашивает МАП Титанатор
(батарея/сеть), MPPT-контроллеры «КЭС» (веб-API ПАК «Малина» или новый API
«Малина26») и электросчётчик
DDS238 (Modbus TCP или Modbus RTU через прозрачный шлюз), нормализует всё в единый
контракт значений и хранит в Redis
(горячие данные, последние 2 календарных суток) + PostgreSQL (вся история —
5-минутные усреднённые точки). Встроенный веб-дашборд.

Язык/команды: Go 1.26, `go run ./cmd/sunreceiver` — запуск, `go vet ./...` — проверки.
Коммиты — по-русски (как в истории репо).

## Архитектура (кратко)

Данные текут: **устройство → poller → Redis (сразу) → аккумулятор → PG (5-мин
усреднённые точки) → дашборд (чтение)**. Опрос ведут независимые горутины
(сетевые инверторы — раз в 5 с; МАП/КЭС/BMS — раз в 1 с). Дашборд только читает
марочные `current`/`series` (Redis) и `*_averages` (PG).

## Модули системы

| Модуль | Что делает | Подробное описание |
|---|---|---|
| **internal/solarman/** | Клиент Solarman V5; после перехода на API используется только счётчиками (`CRC16Modbus`) и диагностикой `cmd/probe` | [modules/solarman-client.md](modules/solarman-client.md) |
| **Сетевые инверторы** (`grid_poller.go`) | Опрос через API «Малина26» (`/api/grid/status` + `/api/grid/device?serial=`) раз в 5 с, маппинг в `values`, имя/размещение/серийник из API | [modules/inverter-poller.md](modules/inverter-poller.md) |
| **Хранение** (`redis_store.go`, `pg_store.go`, `accumulator.go`, `bms26_store.go`, `bms_accumulator.go`) | Redis (2 суток, live) + PG (5-мин средние, вечно), фоновые аккумуляторы/очистка, реставрация Redis из PG при пустом старте | [modules/storage.md](modules/storage.md) |
| **Веб-дашборд** (`dashboard.go`, `cmd/sunreceiver/web/`, `go:embed`) | HTML + JSON API (`/`, `/charts`, `/energy`, `/bms/<name>`) поверх Redis/PG, зум/панорама, offline-индикация, mobile-раскладка; шаблоны/статика (`cmd/sunreceiver/web/templates`, `cmd/sunreceiver/web/static/js`) встроены в бинарник | [modules/dashboard.md](modules/dashboard.md) |
| **МАП + MPPT** (`malina26.go`) | МАП (батарея/сеть) и MPPT-контроллеры (КЭС) через API «Малина26» (`/api/map/raw`, `/api/kes/raw`; динамический состав); резервный серийник МАП | [modules/map-mppt.md](modules/map-mppt.md) |
| **Счётчик DDS238** (`meter_*.go`) | Мгновенные значения `meter_*` + посуточные тарифы «День/Ночь» (`dds238_daily_tariffs`) с добором пропущенных границ; транспорт — Modbus TCP (`tcp`) или Modbus RTU через прозрачный шлюз (`rtu`) | [dds238-meter.md](dds238-meter.md) |
| **Счётчик DTS017M** (`dts017_*.go`) | Трёхфазный счётчик (Modbus RTU через прозрачный шлюз): телеметрия/энергии → **собственные** ключи/ряд Redis и **собственные** таблицы PG; посуточные тарифы «День/Ночь» считаются сами (историю счётчика не читаем); рамка «Гараж» и графики на странице «Электроэнергия», `/api/dts017m/current|tariffs` | [dts017m-meter.md](dts017m-meter.md) |
| **Счётчик «Меркурий»** (энергосбыт, без мониторинга; прогноз) | Последовательно с DDS238; показания снимаются **вручную** (раздел конфига `mercury`), прогноз на текущий момент по последней ручной точке + статистике DDS238 (`mercury.go` → `/api/current` → рамка «Меркурий — прогноз») | [meter_mercury.md](meter_mercury.md) |
| **Счётчик Энергомера CE308** (`ce308_*.go`) | Опрос по BLE: единственный режим — только итоговые показания (накопленная энергия раз в 30 мин/по кнопке), без телеметрии и агрегатов | [modules/ce308.md](modules/ce308.md) |
| **Проброс Bluetooth (usbip)** (вне кода, ОТКЛЮЧЕНО 2026-09-24) | Историческая схема: проброс BLE-контроллера MediaTek с `.9` на `.253` через usbip; на `.253` теперь физический USB-адаптер | [ce308-bluetooth/README.md](ce308-bluetooth/README.md) |
| **BMS malina26** (`bms26*.go`) | Единый пулер BMS (ANT+EnBMS) через HTTP-API «Малина26»: `/api/bms/devices` + `/api/bms/raw` (бинарный контракт 107 Б, копия `internal/bmscontract`); Redis `sunreceiver:bms26:*` + PG `bms26_averages_r` | [modules/bms26.md](modules/bms26.md) |
| **Уведомления в MAX** (`notify.go`) | Отправка событий мониторинга МАП (недоступен / нет напряжения сети) в мессенджер MAX через Bot API, с гистерезисом и дедупликацией | [modules/notify.md](modules/notify.md) |
| **Сетевое реле SR-201** (`relay_control.go`) | Управление двойным реле по UDP (белая/красная лампы): поддержка состояния (вкл/выкл/мигание 2 Гц) + автоиндикаторы (отдача в сеть, наличие напряжения сети) | [relay_sr-201(2light).md](relay_sr-201(2light).md) |
| **Windows-трей и логирование** (`tray_*.go`, `logfile_*.go`) | Сворачивание в системный трей (`fyne.io/systray`) и запись лога в `sunReceiver.log` рядом с exe; на POSIX `runTray`/`setupLogging` — no-op | [modules/tray-logging.md](modules/tray-logging.md) |
| **Универсальный контракт `values`** | Набор общих тегов с одинаковыми именами/единицами для всех марок (PV, AC, фазы, энергия, МАП) | [universal-contract.md](universal-contract.md) |

## Что опрашивается

| Устройство | Протокол | Период | Данные |
|---|---|---|---|
| Инверторы Deye / Sofar | API «Малина26» (HTTP) | 5 с | PV, AC, энергия, частота, фазы, температуры |
| МАП Титанатор | API «Малина26» (HTTP) | 1 с | напряжение/мощность сети и батареи |
| MPPT-контроллеры «КЭС» | API «Малина26» (HTTP) | 1 с | PV панели, заряд АКБ, выработка за сутки |
| Счётчик DDS238 | Modbus TCP или Modbus RTU через прозрачный шлюз (502) | 1 с | мгновенные значения + посуточные тарифы «День/Ночь» |
| Счётчик DTS017M | Modbus RTU через прозрачный шлюз (502, unit 1, fn 03/04) | 1 с | напряжения/токи/мощности/энергии + посуточные тарифы «День/Ночь» |
| Счётчик Энергомера CE308 | BLE (Энергомера/IEC 61107) | 30 мин / кнопка | итоговая накопленная энергия |

- **Инверторы** — только API «Малина26»: настроек в конфиге нет; имя, размещение,
  тип и серийник приходят из API, нормализуются в контракт `values`.
- **МАП** — раздел `map` (HTTP-API «Малина26»): `host/port/password/serial` и
  необязательный `fallback_serial`. Верхнеуровневый `map.disabled` (обязательное)
  отключает ВСЕ пулеры раздела (МАП, КЭС, BMS26).
- **КЭС/MPPT** — из `/api/kes/*` API «Малина26». Состав контроллеров
  **динамический** по ответу API.
- **Счётчик** — раздел `meter` (legacy: файл `dds238.json`); обязательное поле
  `meter.disabled` (`true` — пулеры отключены, плашки/кнопка «Электроэнергия» скрыты).
  Поле `meter.protocol` задаёт транспорт: `"tcp"` (Modbus TCP, по умолчанию) или
  `"rtu"` (Modbus RTU поверх TCP — прозрачный шлюз, напр. USR-DR164; MBAP нет,
  кадр `unit+PDU+CRC16`). Идентификатор счётчика — единый стабильный ключ **`dds238`**
  во всех хранилищах (Redis current/ряд, PG `dds238_averages_r`), не зависящий от `ip`.
- **Счётчик DTS017M** — раздел `dts017m`; `disabled` обязателен. `protocol` — `"rtu"`
  (по умолчанию, прозрачный шлюз) или `"tcp"`; `poll_interval` (сек, по умолчанию 1).
  **Обособленное хранилище**: собственные ключи/ряд Redis (`sunreceiver:dts017m:*`),
  собственные таблицы PG (`dts017m_averages_r`, `dts017m_daily_tariffs`) и отдельный
  аккумулятор; общие ключи/таблицы DDS238 не используются. Историю счётчика не читаем —
  посуточные тарифы считаем сами. См. [`dts017m-meter.md`](dts017m-meter.md).
- **BMS** — единый BMS26 через API «Малина26» (раздел `map`); отдельного раздела
  конфига нет. Устройства ANT+EnBMS показываются по данным `sunreceiver:bms26:*`.
- **Уведомления** — раздел `notify` (бот MAX): события мониторинга МАП
  (недоступен / напряжение сети ниже порога) + восстановление, с гистерезисом
  `stable_window` и дедупликацией. Работают **только** когда включён опрос МАП.

## Хранение данных

- **Redis** — live-хранилище последних 2 календарных суток, **каждое снятое
  показание** (полное разрешение пулера): обособленные HASH+ZSET по марке
  (`sunreceiver:deye:current`/`deye:series:*`, `sunreceiver:sofar:*`) и отдельные
  ключи BMS/CE308/DDS238/МАП/КЭС/DTS017M. Общие `sunreceiver:current`/`series:*`
  выведены из эксплуатации. Ключ инвертора — серийник (`inverter_sn`). Запускается
  с persistence (RDB+AOF). При полностью пустом Redis данные восстанавливаются из
  PostgreSQL. (Подробнее — [modules/storage.md](modules/storage.md).)
- **PostgreSQL** — вся история, **единая гранулярность 1 запись / 5 минут** по марке
  (`sunreceiver.deye_averages_r`/`sofar_averages_r`, а также
  `bms26_averages_r`, `dts017m_averages_r`, `dds238_averages_r`, `map_averages_r`,
  `kes_averages_r`), плюс таблицы тарифов (`sunreceiver.dds238_daily_tariffs` и
  обособленная `sunreceiver.dts017m_daily_tariffs`). Фоновые аккумуляторы усредняют
  накопленные в Redis снимки.
- **Тарифы счётчика** — `sunreceiver.dds238_daily_tariffs` (посуточно, «День/Ночь» ×
  потребление/отдача).

## Конфигурация

Один файл **`sunReceiver.json`** рядом с бинарником (`os.Executable()`; при
`go run ./cmd/sunreceiver` — fallback в CWD). Разделы: `dashboard_port` (обязательное) и
необязательные `dashboard_user`/`dashboard_password`, `map`,
`db`, `meter`, `dts017m`, `ce308`, `notify`, `relay`, `mercury`.
Файл приватный (пароли — в открытом виде, в `.gitignore`); публичный шаблон
структуры — [`sunReceiver.sample.json`](../sunReceiver.sample.json) (обновлять при
любом изменении структуры конфига: IP — случайные из `192.168.0.x`, серийные
номера — случайные, пароли — `CHANGE_ME`).

| Раздел | Поля |
|---|---|
| `dashboard_port` | порт веб-дашборда (обязательное) |
| `dashboard_user` / `dashboard_password` | HTTP Basic для `/api/*` (необязательные; если не заданы — API без авторизации) |
| `map` | `disabled` (обязательное: `true` — все пулеры МАП/КЭС/BMS26 отключены), `host`, `port` (80/443), `password`, `serial`, `fallback_serial` (необязательный резервный серийник) |
| `db` | `redis` (host:port), `pg` (DSN с паролем), `pg_restore_window` (необязательный duration окна реставрации Redis из PG; пусто/нет — 30 суток) |
| `meter` | `disabled` (обязательное: `true` — пулеры отключены, плашки/кнопка «Электроэнергия» скрыты), `name`, `ip`, `port`, `unit`, `first_reg` (должен быть `0`), `register_count` (`0` → 27; иначе ≥ 18) |
| `dts017m` | `disabled` (обязательное), `name`, `ip`, `port` (умолч. 502), `unit` (умолч. 1), `protocol` (`rtu`/`tcp`, умолч. `rtu`), `poll_interval` (сек, умолч. 1) — см. [dts017m-meter.md](dts017m-meter.md) |
| `ce308` | `disabled` (обязательное: `true` — опрос CE308 отключён), `name`, `mac` (BD_ADDR счётчика), `pin` (BLE-PIN радиоинтерфейса) — только итоговые показания (30 мин/кнопка); см. [modules/ce308.md](modules/ce308.md) |
| `mercury` | Коллекция ручных снятий показаний счётчика энергосбыта «Меркурий»: `taken_at` (RFC3339), `import_day`, `import_night`, `export_day`, `export_night` (kWh) — см. [meter_mercury.md](meter_mercury.md) |
| `notify` | `token` (обязательное — токен бота MAX), `user_id`/`chat_id` (адресат; **можно не задавать** — бот сам регистрирует первого подписчика), `disabled` (необязательное: `true` — без оповещений), `stable_window_sec`, `map_undeclared_sec`, `grid_voltage_low` — см. [modules/notify.md](modules/notify.md) |
| `relay` | `disabled` (обязательное: `true` — модуля нет, лампы не управляются), `ip` (обязательное при `disabled=false`), `udp_port`, `blink_hz`, `keepalive`, `lamps[]` (`name`, `relay`), `meter_stale_sec`, `map_stale_sec`, `meter_power_tag`, `meter_voltage_tag`, `map_grid_tag`, `voltage_present_min` — см. [relay_sr-201(2light).md](relay_sr-201(2light).md) |
| legacy | `dds238.json` — старый файл счётчика (используется, только если нет раздела `meter`) |

## Лицензия

Проект распространяется под **GNU General Public License v3.0 (GPL-3.0)** — см.
[`LICENSE`](../LICENSE). Это copyleft-лицензия: производные работы и модификации
обязаны распространяться под той же лицензией с предоставлением исходного кода.

## Сборка, запуск, деплой

```sh
go build -o sunReceiver .   # сборка
./sunReceiver               # запуск (persistent-процесс)
go run ./cmd/sunreceiver                     # запуск из исходников (конфиг из CWD)
```

Все настройки задаются только в `sunReceiver.json` (флагов командной строки нет,
кроме `--version`/`-version` — печатает `sunReceiver <версия>`): адреса БД
(`db.redis`, `db.pg`, опц. `db.pg_restore_window`), порт дашборда
(`dashboard_port`, обязательное).

### Релизы через Makefile

Корневой **`Makefile`** собирает все релизные артефакты; версия подставляется в имя
файла и в `main.version` через `-ldflags "-X main.version=<версия>"`, по умолчанию
`dev`. `dist/` создаётся автоматически.

```sh
make VERSION=1.2.3 linux-x64     # dist/sunReceiver-linux-amd64-1.2.3
make VERSION=1.2.3 win-x64       # dist/sunReceiver-windows-amd64-1.2.3.exe (-H windowsgui)
make VERSION=1.2.3 all           # linux-x64 + win-x64
make clean                       # rm -rf dist
```

**Windows (portable + tray)** — `win-x64` собирается с `-H windowsgui`
(GUI-подсистема, консоль при запуске из проводника не мигает). Приложение
сворачивается в системный трей (`fyne.io/systray`, `tray_windows.go`/`tray_posix.go`,
иконка `tray.ico` embed) — в меню трея только пункт «Закрыть» (graceful shutdown);
на POSIX `runTray` — no-op (обычные SIGINT/SIGTERM). Из-за `-H windowsgui` вывод
`--version` в stdout из проводника не виден (запускать из cmd / перенаправлять).
Логи на Windows пишутся в **`sunReceiver.log`** рядом с exe (как `sunReceiver.json`,
через `setupLogging()` в `logfile_windows.go`); на POSIX `setupLogging()` — no-op
(Linux — journald/systemd, macOS — консоль).

### Деплой (прод)

Прод развёрнут на внутреннем сервере (Ubuntu 22.04, amd64) как systemd-сервис
`sunreceiver.service`; PostgreSQL 16 и Redis тоже там. Адрес и деплой — приватные,
см. `AGENTS.md` (или `.kilo/AGENTS-private.md`). Деплой: собрать Linux-бинарник
(`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o sunReceiver-linux .`), scp на
сервер, `systemctl restart sunreceiver.service`. При переходе на API malina26
сначала обновить прод-конфиг (убрать `invertors` и `poll.inverter`), затем бинарник. Сервис работает от
непривилегированного пользователя `sunreceiver` (nologin) с hardening-юнитом
(`ProtectSystem=strict`, `NoNewPrivileges`, `PrivateTmp`, пустой
`CapabilityBoundingSet` и др.): демон не пишет в ФС, поэтому root не требуется.

## Документация

**Описание модулей** — [`modules/`](modules/README.md):

- [`modules/solarman-client.md`](modules/solarman-client.md) — клиент Solarman V5 (счётчики/диагностика).
- [`modules/inverter-poller.md`](modules/inverter-poller.md) — сетевые инверторы через API malina26.
- [`modules/storage.md`](modules/storage.md) — Redis + PG + аккумулятор.
- [`modules/dashboard.md`](modules/dashboard.md) — веб-дашборд.
- [`modules/map-mppt.md`](modules/map-mppt.md) — МАП + MPPT.
- [`modules/ce308.md`](modules/ce308.md) — счётчик Энергомера CE308 (BLE).
- [`modules/notify.md`](modules/notify.md) — уведомления в мессенджер MAX.
- [`modules/tray-logging.md`](modules/tray-logging.md) — системный трей и логирование
  (Windows / POSIX).
- [`ce308-bluetooth/README.md`](ce308-bluetooth/README.md) — **историческое**
  описание проброса Bluetooth-адаптера по usbip с `.9` на `.253` (отключено
  2026-09-24; снимки конфигов хостов и порядок восстановления — в
  [`ce308-bluetooth/backup/`](ce308-bluetooth/backup/)).

**Результаты исследований** — [`research/`](research/README.md):

- [`research/solarman-v5.md`](research/solarman-v5.md) — протокол V5: кадр, ответ,
  datafield Sofar/Deye, коды ошибок, поведение живых логгеров, находки по CRC.
- [`research/sofar-registers.md`](research/sofar-registers.md) — маппинг регистров
  Sofar K-TLX (0x0000–0x0027, 0x0105–0x0114, HW 0x2000–0x200D).
- [`research/deye-registers.md`](research/deye-registers.md) — маппинг регистров
  Deye string.

**Прочие**:

- [`universal-contract.md`](universal-contract.md) — контракт `values` (каждый тег).
- [`dds238-meter.md`](dds238-meter.md) — модуль счётчика (мгновенные значения,
  посуточные тарифы, конфигурация).
- [`dts017m-meter.md`](dts017m-meter.md) — модуль счётчика DTS017M (Modbus RTU,
  обособленные ключи/таблицы, посуточные тарифы; рамка «Гараж» и графики дашборда,
  `/api/dts017m/current|tariffs`).
- [`meter_mercury.md`](meter_mercury.md) — счётчик энергосбыта «Меркурий»
  (без мониторинга): ручные точки в конфиге `mercury`, прогноз по статистике DDS238.
- `docs/map/` — справочные материалы реверса МАП (байт-карты; локальный, в git не выгружается).