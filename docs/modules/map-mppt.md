# Модуль МАП Титанатор и MPPT-контроллеры

ПАК «Малина» (Raspberry Pi, 192.168.0.60) — веб-интерфейс Микроарт для МАП
Титанатор (192.168.0.74, port **502**, unit 1) и до 16 параллельных **MPPT
контроллеров** Микроарт (`(C)mART`). Один контроллер = одна цель/колонка дашборда.

## Источники данных

- **MPPT-контроллеры** (`kindMPPT`) — при `method=rs485`/`malina`: **веб-API ПАК
  «Малина»** `read_json.php?device=mppt` (HTTP Basic-auth; блок `map.malina`
  `sunReceiver.json`); при `method=malina26`: новый API «Малина26»
  (`/api/kes/*`, см. `malina26.go`). Пароль в открытом виде, файл в git не
  выгружается; для разработки SSH — `.kilo/malina-ssh.json`. В `sunReceiver.json` MPPT
  **не регистрируются**: состав определяется динамически по факту подключения
  контроллеров из ответа API. Контроллер появляется на дашборде, когда приходит в
  ответе, и исчезает, когда перестаёт отвечать (ключ удаляется из `current` через
  `PruneMPPT`; чистка — только при УСПЕШНОМ ответе API: при ошибке запроса
  предыдущий состав сохраняется). Имя — `MPPT-<serial>`, ключ — `mppt-<serial>`.
  Напрямую видны параметры панелей, которых нет в Modbus-гейте: `Vc_PV`, `Ic_PV`,
  `P_PV`, `V_Bat`, `I_Ch`, `P_Out` + `timestamp`. Реализация — `mppt_api.go`
  (`mpptSite`, `FetchMPPTs`, `mapMPPTAPI`) и `malina26.go` (`malina26Source`,
  `decodeMalina26KES`) + сбор в `pollAndSaveMap`; коннектор к `read_json.php` — в
  [../read_json.md](../read_json.md).
- **МАП (`kindMAP`)** — данные **батареи и сети** МАП на дашборд. Раздел `map` имеет
  общий `method` (`rs485` | `malina` | `malina26`), способы — равнозначные блоки:
  - `method=rs485` — **Modbus TCP/RS485** (блок `rs485`, `internal/modbusmap/` +
    `mapClientFor`), блоки 0x400/0x580;
  - `method=malina` — legacy **веб-API ПАК «Малина»** `read_json.php?device=map`
    (блок `malina`; `mpptSite.FetchMAP` + `mapMAPAPI`). Путь — `map_path` (иначе
    выводится из `mppt_path` подменой `device`);
  - `method=malina26` — новый **HTTP-API «Малина26»** (блок `malina26`): токен
    (`POST /api/token`) + бинарный контракт `MapData` из `/api/map/raw`. Реализация —
    `malina26.go` (`malina26Source`, `decodeMalina26Map`).

  Ключ устройства (`devKey`) — `"map"`, имя — из блока, поэтому история на дашборде
  преемственна при переключении методов.

  КЭС/MPPT: при `rs485`/`malina` — через блок `malina` (`read_json.php?device=mppt`);
  при `malina26` — из того же API (`/api/kes/raw`, `decodeMalina26KES`).

Верхнеуровневый **`map.disabled`** (обязательное поле раздела `map`) — мастер-выключатель
всего раздела: при `true` НЕ запускаются пулеры МАП и КЭС, блок «Данные МАП» на дашборде
скрывается; на пулеры сетевых инверторов не влияет.
Опрос ANT BMS вынесен в отдельный раздел **`antBms`** `{"url", "disabled"}` (см.
[`bms-listener.md`](bms-listener.md)) и от `map.disabled` не зависит.

## `values` углов МАП/MPPT (теги в `commonContractTags`)

- **MPPT API** (`mapMPPTAPI`): `pv1_voltage/current/power` = `Vc_PV`/`Ic_PV`/`P_PV`
  панелей контроллера; `l1_voltage` = `V_Bat` (напряжение АКБ); `l1_current` =
  `I_Ch` (ток заряда); `ac_active_power` = `P_Out`; `energy_today` = `Pwr_kW` +
  `Pwr_W/1000` (выработка за сутки, кВт·ч). `timestamp` снимка = поле `timestamp`
  ответа API.
- **MAP Modbus** (`mapMAPRegisters`): `l1_voltage`=`battery_voltage`=АКБ (`_UAcc_med`
  0x405/0x406, `(VH*256+VL)/10`); `l1_current`/`ac_active_power` для МАП **НЕ
  выставляются** (это теги AC-инверторов; раньше дублировали ток/мощность АКБ и нигде
  не использовались); `grid_frequency`=0; `battery_power` = `U_АКБ × I_АКБ`
  (ток 0x432/0x433, `(L+H*256)/16`; в заряде MODE==4 ток отрицательный → мощность
  отрицательная, при отдаче положительная). `_PLoad` 0x59E/0x59F НЕ используется — это
  мощность нагрузки, не батареи. Для дашборда: `grid_voltage` (`_UNET` 0x422, 0 → нет
  сети, иначе +100 В), `grid_power` (`_PNET` 0x59A/0x59B, знак `_PNET_Sign_P` 0x587).
- **MAP API** (`mapMAPAPI`, при `method=malina`): тот же контракт из
  `read_json.php?device=map`: `battery_voltage`/`l1_voltage` = `_Uacc`, `grid_frequency`
  = `_TFNET`, `grid_voltage` = `_UNET` (уже в В, без смещения +100), `grid_power` =
  `±|_PNET_calc|` (= `_UNET`×`_INET`, достоверная; сырой `_PNET` у МАП занижен ~в 5 раз
  против счётчика, поэтому не используется — фолбэк на него только при отсутствии
  `_PNET_calc`). Направление берётся из сырой ячейки `_PNET_Sign_P` (0x587),
  читаемой отдельно через `read_memory.php?offset=1415&count=1` (`mpptSite.FetchPNETSign`,
  `mapRaw.PNETSign`): `1` — потребление из сети «+», `0` — отдача «−». `_PNET_calc` при
  этом даёт только модуль: у новой прошивки Малины (`fw=4.3`) его знак не привязан к
  направлению, а `_Inet_flag` (по которому mapd опционально инвертирует знак) — флаг
  динамический и направления не несёт (см. `docs/malina-bug_Inet_flag.md`). Если сырая
  ячейка недоступна — фолбэк на прежнюю эвристику по `_Inet_flag`. Правка только
  для ветки веб-API, Modbus-ветка (`mapMAPRegisters`) не затрагивается.
  `battery_power` = `−_PLoad_calc` (инверсия знака — расчётная батарейная мощность
  `_Uacc×_Iacc` положительна при заряде; контракт требует заряд отрицательным). Время снимка
  — текущее время опроса, а не `timestamp` ответа API (МАП отдаёт его
  устаревшим/некорректным, что увело бы точки на годы в прошлое).
- **MAP/KES malina26** (`malina26.go`, `method=malina26`): МАП из `/api/map/raw`
  (`decodeMalina26Map`) теми же формулами, что rs485-ветка (адреса ячеек совпадают):
  `battery_voltage`/`l1_voltage` = `UAcc_med_VH/VL` (0x405/0x406), `battery_current` =
  `(L+H*256)/16` со знаком заряд− (`MODE_RAM` 0x400 == 4), `battery_power` = `U×I`,
  `grid_voltage` = `UNET`+100, `grid_power` = `PNET_8/8×100` (знак `PNET_Sign_P`),
  `grid_frequency` = `6250/TFNET`, `map_temp_*` = `Grad0/1/2 − 50` (`Temp_off`-биты).
  КЭС из `/api/kes/raw` (`decodeMalina26KES`, `KESR`): `pv1_*`, `l1_*`, `ac_active_power`,
  `energy_today` = `PwrKW + PwrW/10000`. Версии контрактов пиннятся
  (`MapData version=5/402`, `KESR version=1`), при расхождении payload отвергается.

## Ключ устройства и цикл опроса

- **Ключ в Redis/PG = `devKey(t)`**: для kindMPPT — `mppt-<UID>` (напр. `mppt-1097`,
  UID = серийник КЭС: legacy `UID`/`KESE.Serial`), без host; для kindMAP — `"map"`
  (единственная колонка/ряд батареи-сети, не зависит от метода опроса); для
  инверторов — **серийник инвертора (`InverterSN`)**, IP — информационное поле.
  **Смена формата ключа КЭС (`<host>#mppt-*` → `mppt-<serial>`)** выполнена разовыми
  Python-скриптами (`scripts/migrate_map*.py`/`migrate_kes*.py`) — в бинарнике
  миграций нет (правило проекта). Остаточные старые ключи чистит `PruneMPPT`
  (`redis_store.go`).
- **Опрос — 1 раз в секунду** (отдельный цикл `runMapPoll`, не в 10-сек циклах
  инверторов). `PollDevice` (case `kindMAP`) читает блоки `modbusmap`: 0x400
  (0x20 слов = 0x400..0x43F) и 0x580 (`_PNET_Sign_P` 0x587, `_PNET` 0x59A/0x59B,
  `_PLoad` 0x59E/0x59F). Блок 0x420 отдельно не запрашивается — это подмножество
  блока 0x400..0x43F; блок 0x530 (токи MPPT) не читается — mapMAPRegisters его не
  использует (пер-слотовые MPPT берутся из веб-API).
- **Фактический интервал.** Тикер цикла — 1 с, но сам опрос МАП делает **2
  последовательных чтения блоков** Modbus (0x400, 0x580), каждое — до
  таймаута чтения гейта (~3 с). Тики `Ticker` коалесцируются, поэтому при медленном
  гейте реальный период опроса МАП — **~6 с**, а не 1 с (by-design, единый цикл на
  всех МАП; одно устройство не даёт отдачи чаще). В Redis запись идёт **каждым
  снятым показанием** (см. `saveMapSnapshot`).
- Запись — через `saveMapSnapshot` (`map_store.go`, см.
  [storage.md](storage.md)): score = секунда снятия; пишется КАЖДОЕ показание
  (дедуп только в пределах одной секунды на устройство). Аккумуляцией PG
  усредняется как у остальных (1 точка за 5 минут).

## Связанные документы

- [../read_json.md](../read_json.md) — форматы `read_json.php?device=map|mppt|bat`.
- [../malina-web-api.md](../malina-web-api.md) — устройство ПАК «Малина», shm,
  все PHP-эндпоинты.
- `docs/map/` — байт-карты/Modbus-регистры МАП (справочные материалы реверса).
- [Дашборд](dashboard.md) — плашки/графики МАП.
## Ошибки МАП (правило, к реализации)

Ошибки МАП читать **только «сырыми»**, в зависимости от выбранного в конфиге
способа мониторинга (`method`):

- `method=rs485` — по **Modbus** (Modbus TCP/RTU через `mapgateway`);
- `method=malina` — из Малины через **`read_memory.php?offset=<байт>&count=<n>`**
  (RAM МАП, байт-ячейки 0x000..0x7FF, тот же хост/Basic-auth, что у MPPT).
- `method=malina26` — из бинарного контракта `MapData` (`/api/map/raw`), ячейки по
  адресам (см. `.kilo/malina26-contracts.md`).

Никаких агрегирующих эндпоинтов с готовыми текстами (`map_errors.php` и т.п.) не
использовать. Запись — в `device_errors` по устройству «МАП», по правилу истории
(только появление ошибки, без дублей на каждом чтении).

## Реверс логирования ошибок МАП на Малине (2026-10-01)

На Малине (`/settings/html`) ошибки МАП/MPPT обслуживают PHP-скрипты
`map_errors.php`, `mppt_errors.php`, `*_errors_history.php`, `errors_counter.php`;
сами ошибки пишет `mapd` в CSV-логи `/settings/logs/map_errors.log` (только при
СМЕНЕ значений, 2553 строки на 2026-10-01) и `mppt_errors.log`.

Формат строки `map_errors.log` (поля по порядку):
`date, time, _RSErrSis, _RSErrJobM, _RSErrJob, _RSWarning, _I2C_Err, _RSErrDop,
_F_Acc_Over, _F_Net_Over, _TFNET_Limit, _UNET_Limit`.
Тексты битов — в `locale/RU|EN/local.inc` (`$data_map[...]`).

Ячейки-ошибки МАП (RAM, из `protocol_MAP_cells_2026_07_15.doc`):
| Ячейка | Имя | Смысл |
|---|---|---|
| 0x41C | `_F_AccOver` | отключения по перегрузкам АКБ (8 бит) |
| 0x41D | `_F_NETOver` | отключения по перегрузкам сети (биты 0,1,3) |
| 0x42A | `_RSErrSis` | критические системные ошибки |
| 0x42B | `_RSErrJobM` | аварии АКБ/сети/выхода |
| 0x42C | `_RSErrJob` | перегрузки/перегрев/вентилятор |
| 0x42D | `_RSWarning` | рабочие предупреждения |
| 0x447 | `_RSErrDop` | 3-фазные/параллельные/I2C/BMS/MPPT |
| 0x448 | `_I2C_Err` | ошибки I2C (Ack/Sum/Size/Protocol) — адрес **предположительный** |

Наш пулер ошибок (`map_errors.go`) читает эти ячейки **сырыми** отдельным циклом
(≤1/30 с): через Modbus (`method=rs485`, блок 0x41C..0x449), через
`read_memory.php` (`method=malina`, web-API) либо из бинарного `MapData`
(`method=malina26`, ячейки 0x41C..0x449 по id). Пишет появление ошибок в
`device_errors` (device=`map`), телеметрию не трогает.
