# sunReceiver

Go-приложение, которое опрашивает solar-инверторы через их WiFi-даталоггеры
(Solarman LSW-3/LSE, порт 8899, TCP) и сохраняет распарсенные данные в Redis
(с persistence RDB+AOF). При полностью пустом Redis данные восстанавливаются из
PostgreSQL. Включает веб-дашборд текущих параметров.

Язык/команды: Go 1.26, `go run .` — запуск, `go vet ./...` — проверки. Коммиты
писать по-русски, как в истории репо.

## Документация

Полная документация — в [`docs/`](docs/). Краткий обзор системы (все функции/модули
+ ссылки) — [`docs/README.md`](docs/README.md). Подробные описания модулей —
[`docs/modules/`](docs/modules/). Результаты исследований (реверс протоколов,
маппинги регистров) — [`docs/research/`](docs/research/).

| Раздел | Где |
|---|---|
| Краткое описание всех модулей + навигация | [`docs/README.md`](docs/README.md) |
| Клиент Solarman V5 (`solarman/`) | [`docs/modules/solarman-client.md`](docs/modules/solarman-client.md) |
| Poller инверторов (`main.go`) | [`docs/modules/inverter-poller.md`](docs/modules/inverter-poller.md) |
| Хранение (Redis/PG/аккумулятор) | [`docs/modules/storage.md`](docs/modules/storage.md) |
| Веб-дашборд (`dashboard.go`) | [`docs/modules/dashboard.md`](docs/modules/dashboard.md) |
| МАП + MPPT | [`docs/modules/map-mppt.md`](docs/modules/map-mppt.md) |
| Счётчик DDS238 | [`docs/dds238-meter.md`](docs/dds238-meter.md) |
| ANT BMS | [`docs/antbms.md`](docs/antbms.md) |
| Уведомления в MAX | [`docs/modules/notify.md`](docs/modules/notify.md) |
| Протокол Solarman V5 (реверс) | [`docs/research/solarman-v5.md`](docs/research/solarman-v5.md) |
| Регистры Sofar K-TLX | [`docs/research/sofar-registers.md`](docs/research/sofar-registers.md) |
| Регистры Deye string | [`docs/research/deye-registers.md`](docs/research/deye-registers.md) |
| API ПАК «Малина» | [`docs/read_json.md`](docs/read_json.md), [`docs/malina-web-api.md`](docs/malina-web-api.md) |
| Универсальный контракт `values` | [`docs/universal-contract.md`](docs/universal-contract.md) |

## Устройство: даталоггеры, адреса, модели

Реальные IP/серийные номера даталоггеров и топология — **приватные**, см.
`.kilo/AGENTS-private.md` (локальный агент подхватывает его через `.kilo/kilo.json` →
`instructions`; файл в `.gitignore`). Публично — только протоколы/маппинги в
[`docs/research/`](docs/research/).

Список опрашиваемых инверторов задаётся в **`sunReceiver.json` рядом с исполняемым
файлом** (`os.Executable()`; при `go run .` — fallback в CWD). Структура: раздел
**`invertors`** — список инверторов `[{"ip", "name", "type": "deye"|"sofar",
"logger_sn": uint32, "disabled": bool}]`; `name` — логическое имя (обязательно);
**`disabled` — ОБЯЗАТЕЛЬНОЕ поле** (`false` = опрашивается, `true` = временно
отключён — устройство в конфиге, но не опрашивается, при старте логируется; отсутствие
поля = ошибка загрузки конфига). МАП Титанатор — **вложенный подраздел `rs485` раздела
`map`** `{"name", "ip", "unit", "disabled"}` (не входит в `invertors`); **`disabled` —
ОБЯЗАТЕЛЬНОЕ поле** (`false` = МАП опрашивается через Modbus TCP/RS485, unit по умолч. 1;
`true` = пулер по Modbus НЕ запускается, а параметры батареи/сети берутся из веб-API
ПАК «Малина» `read_json.php?device=map`). MPPT-контроллеры и доступ к ПАК «Малина» —
**раздел `map`** (те же поля, что и у `rs485`, плюс веб-API) `{"rs485", "base_url",
"mppt_path", "map_path", "login", "password", "bms_path"}` (пароль в открытом виде, файл
в git не выгружается): `mppt_path` — путь к `read_json.php?device=mppt`; `map_path`
(необязательное, напр. `/read_json.php?device=map`) — явный путь к данным МАП, при
отсутствии выводится из `mppt_path` подменой параметра `device`; `bms_path`
(необязательное, напр. `/read_bms.php`) включает опрос ANT BMS — тот же хост/Basic-auth,
что и MPPT. Расположение баз — раздел **`db`**
`{"redis": "host:port", "pg": "DSN", "pg_restore_window": "720h"}` (адреса напрямую из
конфига; `pg_restore_window` — duration-строка окна реставрации Redis из PG, пусто/нет =
дефолт 30 суток; пароль PG — в DSN, конфиг приватный). Порт веб-дашборда — корневое поле
**`dashboard_port`** — **ОБЯЗАТЕЛЬНОЕ** (отсутствие = ошибка загрузки конфига; в
`sunReceiver.sample.json` указан 80, на проде 8080). Счётчик DDS238 — раздел **`meter`**
`{"name", "ip", "port", "unit", "first_reg", "register_count"}` (имеет приоритет над
legacy-файлом `dds238.json`). Уведомления в мессенджер MAX — раздел **`notify`**
`{"token", "user_id", "chat_id", "disabled", "stable_window_sec", "map_undeclared_sec", "grid_voltage_low"}`
(токен бота MAX обязателен; адресат `user_id`/`chat_id` — **необязателен**: если
пуст, бот регистрирует первого подписчика по `bot_started`/`bot_added`/`message_created`
и дописывает адресат в конфиг, последующие отписки/отказы — см.
[`docs/modules/notify.md`](docs/modules/notify.md)). Опрос Deye/Sofar — раз в 10 секунд; МАП и MPPT — 1 раз
в секунду (с сохранением 1 точки за 10 с); BMS — 1 раз в секунду.

**Шаблон `sunReceiver.sample.json`** (в git) — публичный пример структуры конфига.
**Всегда** обновлять его при любом изменении структуры/содержимого `sunReceiver.json`
(новые разделы, поля, типы устройств): IP-адреса заменить на случайные из
`192.168.0.x`, серийные номера — на случайные, пароли — замазать (`CHANGE_ME` /
`CHANGE_ME_PASSWORD`). Сам `sunReceiver.json` приватный (в `.gitignore`), sample —
публичный.

## Модули и поток данных

Данные текут: **устройство → poller → Redis (сразу) → аккумулятор → PG (5-мин
усреднённые точки) → дашборд (чтение)**. Опрос — независимые горутины. Дашборд только
читает `current`/`series` (Redis) и `averages` (PG).

- **`solarman/`** — клиент Solarman V5. Полное описание —
  [`docs/modules/solarman-client.md`](docs/modules/solarman-client.md).
- **`main.go`** — poller инверторов (Deye/Sofar) в независимых циклах `runInverterPoll`
  (раз в 10 с), маппинг регистров в `values`, серийные номера, чистое завершение.
  Полное описание — [`docs/modules/inverter-poller.md`](docs/modules/inverter-poller.md).
- **Хранение** — `redis_store.go`, `pg_store.go`, `accumulator.go`, `bms_accumulator.go`,
  фоновые аккумулятор/очистка, реставрация Redis из PG (`restoreRedisFromPG`).
  Полное описание — [`docs/modules/storage.md`](docs/modules/storage.md).
- **Дашборд** — `dashboard.go`, HTTP + JSON API. Полное описание —
  [`docs/modules/dashboard.md`](docs/modules/dashboard.md).
- **МАП + MPPT** — `mppt_api.go`, `modbusmap/`, `runMapPoll`. Полное описание —
  [`docs/modules/map-mppt.md`](docs/modules/map-mppt.md).
- **Счётчик DDS238** — `meter_*.go`: мгновенные значения `meter_*` + посуточные
  тарифы (`daily_tariffs`), добор пропущенных границ. Полное описание —
  [`docs/dds238-meter.md`](docs/dds238-meter.md).
- **ANT BMS** — `bms_poller.go`, `bms_accumulator.go`, `bmslistener/`. Полное описание —
  [`docs/antbms.md`](docs/antbms.md). Демон bmslistener (установка на ПАК «Малина») —
  [`docs/modules/bms-listener.md`](docs/modules/bms-listener.md).
- **Уведомления в MAX** — `notify.go` (`maxClient` + трекер состояния МАП
  `mapTrack` + `runNotifyMonitor`): события мониторинга МАП (недоступен / нет
  напряжения сети) и восстановление, отправка через Bot API MAX с гистерезисом
  и дедупликацией. Работают только когда включён опрос МАП. Полное описание —
  [`docs/modules/notify.md`](docs/modules/notify.md).

### Универсальный контракт `values`

`values` хранит **только общие теги** (`commonContractTags` в `main.go`) с одинаковыми
именами и единицами; единица зашита в суффикс имени. Бренд-специфичные поля в файл не
пишутся. Теги `*voltage`/`*current`/`*power`/`energy*`/`temperature*` округляются до
1 знака (`round1`). Полное описание каждого тега — [`docs/universal-contract.md`](docs/universal-contract.md).

## Запуск и эксплуатация

- **Прод** развёрнут на внутреннем сервере (Ubuntu 22.04, x86_64/amd64) как
  systemd-сервис `sunreceiver.service` (`/opt/sunreceiver/sunReceiver` +
  `/opt/sunreceiver/sunReceiver.json`; PostgreSQL 16 и Redis — там же; дашборд `:8080`).
  Сервис работает от непривилегированного системного пользователя `sunreceiver`
  (nologin, без home) с hardening-юнитом: демон не пишет в ФС (только исходящие
  TCP/HTTP + слушает `:8080` >1024), поэтому root не требуется. Адрес сервера,
  SSH-доступ, команды деплоя и топология — **приватные**, см.
  `.kilo/AGENTS-private.md`.
- Локальная разработка/тест — `go run .` (fallback конфигов в CWD). Локальный
  пулер работает от конфига **`sunReceiver.json`** рядом с бинарником (все разделы:
  invertors, map, mppt, db, meter — в одном файле; файл приватный, в git не попадает).
- **НЕ запускать инстансы sunReceiver на машине разработки без
  явного разрешения пользователя**: локальный конфиг `sunReceiver.json` указывает
  `db.redis`/`db.pg` на прод-сервер, поэтому `go run .` на ней опрашивает реальное
  железо и пишет в прод-Redis/PG — это второй писатель, и данные «оживают» даже при
  отключённых на проде пулерах. Прод-эксплуатация — только через systemd на прод-сервере;
  локальный запуск — с изолированным хранилищем и после согласования с пользователем.

### Деплой (обновление прода)

Продукт собирается на Mac, бинарник передаётся на прод-сервер, сервис перезапускается.
Точные адрес/команды **sshpass/scp/ssh** (с флагами `PreferredAuthentications=password`)
и порядок сборки под Linux — в `.kilo/AGENTS-private.md`.

**При перезапуске после правок кода**: собрать linux-бинарник, scp поверх
`/opt/sunreceiver/sunReceiver`, `systemctl restart sunreceiver.service`.

**Всегда сразу после любых правок кода — commit (по-русски) + push + деплой**
(без ожидания отдельной команды пользователя).

## Сборка релизов

Релизы собираются через корневой **`Makefile`** (версия подставляется в имя файла и
в `main.version` через `-ldflags "-X main.version=<версия>"`; по умолч. `dev`).
`VERSION ?= dev`, `dist/` создаётся автоматически. Требуется установленный `zig`
(только для цели `bmslistener`).

```sh
make VERSION=1.2.3 linux-x64     # dist/sunReceiver-linux-amd64-1.2.3
make VERSION=1.2.3 win-x64       # dist/sunReceiver-windows-amd64-1.2.3.exe (-H windowsgui)
make VERSION=1.2.3 bmslistener   # dist/bmslistener-armv7l-1.2.3 (zig, кросс-сборка под Малину)
make VERSION=1.2.3 all           # все три
make clean                       # rm -rf dist
```

Подробности и Windows-специфика — [`docs/README.md`](docs/README.md).

**Windows (portable + tray)** — сборка с `-H windowsgui` (GUI-подсистема, консоль при
запуске из проводника не мигает). Приложение сворачивается в системный трей
(`fyne.io/systray`, файлы `tray_windows.go`/`tray_posix.go`; иконка `tray.ico`,
embed; меню — только «Закрыть» → graceful shutdown; на POSIX `runTray` — no-op,
завершение по SIGINT/SIGTERM). Логи на Windows — в `sunReceiver.log` рядом с exe
(`setupLogging()` в `logfile_windows.go`; на POSIX — no-op: Linux journald/systemd,
macOS консоль). Флаг `--version`/`-version` печатает `sunReceiver <версия>`, но из-за
`-H windowsgui` в stdout при запуске из проводника не виден (запускать из cmd /
перенаправлять).

**bmslistener (Малина)** — кросс-сборка через `zig cc -target
arm-linux-musleabihf -static` (статичный elf32 ARM); не зависит от libc платы,
запускается и на старом Raspbian jessie. Артефакт кладётся в `dist/`, **на плату не
переносится** (только артефакт/кросс-сборка).

## Окружение

- Репо: github.com/galiy/sunReceiver (remote git@github.com:galiy/sunReceiver.git,
  branch main).
- macOS, Go 1.26.5. `nc` доступен для быстрых проверок TCP. `timeout` в zsh нет —
  запускать через background_process или `&`.
- Эталонная библиотека (не в vendor, только для справки):
  `~/go/pkg/mod/github.com/snowirbis/solarman@v1.0.4/` (frame.go — формат кадра,
  read.go — payload).