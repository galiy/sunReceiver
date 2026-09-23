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
(инверторы — раз в 10 с; MPPT/счётчик/BMS — раз в 1 с; МАП — цикл 1 с, но фактически
~9 с из-за 3 последовательных чтений Modbus-блоков, см. [map-mppt.md](modules/map-mppt.md)). Дашборд только читает
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
| **Счётчик Энергомера CE308** (`ce308_*.go`) | Опрос по BLE (2 с): напряжения/токи/мощности по фазам + разовый снимок накопленной энергии по сигналу; история усредняется до 1 записи за 10 с в PG | [modules/ce308.md](modules/ce308.md) |
| **Проброс Bluetooth (usbip)** (вне кода) | Проброс BLE-контроллера MediaTek с `.9` на `.253` через usbip; systemd-юниты + watchdog авто-восстановления | [modules/bluetooth-usbip.md](modules/bluetooth-usbip.md) |
| **ANT BMS** (`bms_poller.go`, `bmslistener/`) | Опрос батарей через `read_bms.php` → shm bmslistener; 5-мин усреднённые точки в Redis+PG | [antbms.md](antbms.md), [modules/bms-listener.md](modules/bms-listener.md) |
| **Уведомления в MAX** (`notify.go`) | Отправка событий мониторинга МАП (недоступен / нет напряжения сети) в мессенджер MAX через Bot API, с гистерезисом и дедупликацией | [modules/notify.md](modules/notify.md) |
| **Сетевое реле SR-201** (`relay_control.go`) | Управление двойным реле по UDP (белая/красная лампы): поддержка состояния (вкл/выкл/мигание 2 Гц) + автоиндикаторы (отдача в сеть, наличие напряжения сети) | [relay_sr-201(2light).md](relay_sr-201(2light).md) |
| **Универсальный контракт `values`** | Набор общих тегов с одинаковыми именами/единицами для всех марок (PV, AC, фазы, энергия, МАП) | [universal-contract.md](universal-contract.md) |

## Что опрашивается

| Устройство | Протокол | Период | Данные |
|---|---|---|---|
| Инверторы Deye / Sofar | Solarman V5 (TCP 8899) | 10 с | PV, AC, энергия, частота, фазы, температуры |
| МАП Титанатор | Modbus TCP (502) или веб-API | 1 с | напряжение/мощность сети и батареи |
| MPPT-контроллеры «КЭС» | веб-API read_json.php (HTTP) | 1 с | PV панели, заряд АКБ, выработка за сутки |
| Счётчик DDS238 | Modbus TCP (502) | 1 с | мгновенные значения + посуточные тарифы «День/Ночь» |
| Счётчик Энергомера CE308 | BLE (Энергомера/IEC 61107) | 2 с | напряжения/токи/мощности по фазам + разовый снимок энергии |
| ANT BMS | веб-API read_bms.php (HTTP) | 1 с | SOC, ячейки, ток/мощность, температуры, MOS |

- **Инверторы** — раздел `invertors` конфига, нормализуются в контракт `values`
  (у каждого флаг `disabled`, обязательное поле).
- **МАП** — подраздел `map.rs485`; источник задаёт обязательное поле `disabled`
  (`false`=Modbus/RS485, `true`=веб-API ПАК «Малина»). Верхнеуровневый `map.disabled`
  (обязательное) отключает ВСЕ пулеры раздела (МАП, MPPT, BMS); `map.bms_disabled`
  (обязательное) отключает только пулер ANT BMS.
- **MPPT/МАП веб-API** — раздел `map` (доступ к ПАК «Малина»); состав контроллеров
  **динамический** по ответу веб-API (появляется/исчезает на дашборде).
- **Счётчик** — раздел `meter` (legacy: файл `dds238.json`); обязательное поле
  `meter.disabled` (`true` — пулеры отключены, плашки/кнопка «Электроэнергия» скрыты).
- **BMS** — поле `map.bms_path`; состав батарей динамический по ответу `read_bms.php`.
  При `map.bms_disabled=true` пулер отключён, батарейки с дашборда скрыты.
- **Уведомления** — раздел `notify` (бот MAX): события мониторинга МАП
  (недоступен / напряжение сети ниже порога) + восстановление, с гистерезисом
  `stable_window` и дедупликацией. Работают **только** когда включён опрос МАП.

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
`go run .` — fallback в CWD). Разделы: `invertors`, `map` (с подразделом `rs485`), `db`, `meter`, `notify`, `relay`.
Файл приватный (пароли — в открытом виде, в `.gitignore`); публичный шаблон
структуры — [`sunReceiver.sample.json`](../sunReceiver.sample.json) (обновлять при
любом изменении структуры конфига: IP — случайные из `192.168.0.x`, серийные
номера — случайные, пароли — `CHANGE_ME`).

| Раздел | Поля |
|---|---|
| `invertors[]` | `ip`, `name`, `type` (`deye`/`sofar`), `logger_sn`, `disabled` (обязательное) |
| `map` | `disabled` (обязательное: `true` — все пулеры МАП/MPPT/BMS отключены, плашки МАП скрыты), `bms_disabled` (обязательное: `true` — пулер ANT BMS отключён, батарейки скрыты), `rs485` (подраздел: `name`, `ip`, `unit` (Modbus, умолч. 1), `disabled` (обязательное: `false`=Modbus/RS485, `true`=веб-API ПАК «Малина»)); веб-API: `base_url`, `mppt_path`, `map_path` (необязательное — путь к read_json.php?device=map, при отсутствии выводится из mppt_path), `login`, `password`, `bms_path` (включает опрос ANT BMS) |
| `db` | `redis` (host:port), `pg` (DSN с паролем) |
| `meter` | `disabled` (обязательное: `true` — пулеры отключены, плашки/кнопка «Электроэнергия» скрыты), `name`, `ip`, `port`, `unit`, `first_reg`, `register_count` |
| `notify` | `token` (обязательное, токен бота MAX), `user_id`/`chat_id` (адресат, хотя бы одно), `disabled`, `stable_window_sec`, `map_undeclared_sec`, `grid_voltage_low` — см. [modules/notify.md](modules/notify.md) |
| `relay` | `disabled` (обязательное: `true` — модуля нет), `ip` (обязательное при `disabled=false`), `udp_port`, `blink_hz`, `keepalive`, `lamps[]` (`name`, `relay`), `meter_stale_sec`, `map_stale_sec`, `meter_power_tag`, `meter_voltage_tag`, `map_grid_tag`, `voltage_present_min` — см. [relay_sr-201(2light).md](relay_sr-201(2light).md) |

## Лицензия

Проект распространяется под **GNU General Public License v3.0 (GPL-3.0)** — см.
[`LICENSE`](../LICENSE). Это copyleft-лицензия: производные работы и модификации
обязаны распространяться под той же лицензией с предоставлением исходного кода.

## Сборка, запуск, деплой

```sh
go build -o sunReceiver .   # сборка
./sunReceiver               # запуск (persistent-процесс)
go run .                     # запуск из исходников (конфиг из CWD)
```

Все настройки задаются только в `sunReceiver.json` (флагов командной строки нет,
кроме `--version`/`-version` — печатает `sunReceiver <версия>`): адреса БД
(`db.redis`, `db.pg`, опц. `db.pg_restore_window`), порт дашборда
(`dashboard_port`, обязательное).

### Релизы через Makefile

Корневой **`Makefile`** собирает все релизные артефакты; версия подставляется в имя
файла и в `main.version` через `-ldflags "-X main.version=<версия>"`, по умолчанию
`dev`. `dist/` создаётся автоматически. Для цели `bmslistener` требуется
установленный `zig`.

```sh
make VERSION=1.2.3 linux-x64     # dist/sunReceiver-linux-amd64-1.2.3
make VERSION=1.2.3 win-x64       # dist/sunReceiver-windows-amd64-1.2.3.exe (-H windowsgui)
make VERSION=1.2.3 bmslistener   # dist/bmslistener-armv7l-1.2.3 (zig)
make VERSION=1.2.3 all           # все три
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

**bmslistener (Малина)** — кросс-сборка через `zig cc -target
arm-linux-musleabihf -static` (статичный elf32 ARM), поэтому не зависит от libc
платы и запускается на старом Raspbian jessie. Артефакт кладётся в `dist/`, **на
плату не переносится** (только артефакт/кросс-сборка).

### Деплой (прод)

Прод развёрнут на внутреннем сервере (Ubuntu 22.04, amd64) как systemd-сервис
`sunreceiver.service`; PostgreSQL 16 и Redis тоже там. Адрес и деплой — приватные,
см. `AGENTS.md` (или `.kilo/AGENTS-private.md`). Деплой: собрать Linux-бинарник
(`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o sunReceiver-linux .`), scp на
сервер, `systemctl restart sunreceiver.service`. Сервис работает от
непривилегированного пользователя `sunreceiver` (nologin) с hardening-юнитом
(`ProtectSystem=strict`, `NoNewPrivileges`, `PrivateTmp`, пустой
`CapabilityBoundingSet` и др.): демон не пишет в ФС, поэтому root не требуется.

## Документация

**Описание модулей** — [`modules/`](modules/README.md):

- [`modules/solarman-client.md`](modules/solarman-client.md) — клиент Solarman V5.
- [`modules/inverter-poller.md`](modules/inverter-poller.md) — poller инверторов.
- [`modules/storage.md`](modules/storage.md) — Redis + PG + аккумулятор.
- [`modules/dashboard.md`](modules/dashboard.md) — веб-дашборд.
- [`modules/map-mppt.md`](modules/map-mppt.md) — МАП + MPPT.
- [`modules/ce308.md`](modules/ce308.md) — счётчик Энергомера CE308 (BLE).
- [`modules/bluetooth-usbip.md`](modules/bluetooth-usbip.md) — проброс Bluetooth-адаптера
  по usbip с `.9` на `.253` (systemd, watchdog восстановления).

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
- [`modules/bms-listener.md`](modules/bms-listener.md) — демон bmslistener: описание
  и **установка** на ПАК «Малина» (сборка C + `make install` + systemd).
- `docs/map/`, `docs/antbms/` — справочные материалы реверса (байт-карты, кадры,
  команды).