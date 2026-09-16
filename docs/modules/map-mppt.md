# Модуль МАП Титанатор и MPPT-контроллеры

ПАК «Малина» (Raspberry Pi, 192.168.13.60) — веб-интерфейс Микроарт для МАП
Титанатор (192.168.13.74, port **502**, unit 1) и до 16 параллельных **MPPT
контроллеров** Микроарт (`(C)mART`). Один контроллер = одна цель/колонка дашборда.

## Источники данных

- **MPPT-контроллеры** (`kindMPPT`) — **веб-API ПАК «Малина»**
  `read_json.php?device=mppt` (HTTP Basic-auth; конфиг — раздел **`map`**
  `sunReceiver.json`, поля веб-API; пароль в открытом виде, файл в git не выгружается;
  для разработки SSH — `.kilo/malina-ssh.json`). В `sunReceiver.json` MPPT **не
  регистрируются**: состав определяется динамически по факту подключения
  контроллеров из ответа API. Контроллер появляется на дашборде, когда приходит в
  ответе, и исчезает, когда перестаёт отвечать (ключ удаляется из `current` через
  `PruneMPPT`; чистка — только при УСПЕШНОМ ответе API: при ошибке запроса
  предыдущий состав сохраняется). Имя — `MPPT-<slot+1>`. Напрямую видны параметры
  панелей, которых нет в Modbus-гейте: `Vc_PV`, `Ic_PV`, `P_PV`, `V_Bat`, `I_Ch`,
  `P_Out` + `timestamp`. Реализация — `mppt_api.go` (`mpptSite`, `FetchMPPTs`,
  `mapMPPTAPI`) + сбор в `pollAndSaveMap`; коннектор к `read_json.php` — в
  [../read_json.md](../read_json.md).
- **МАП (`kindMAP`)** — подраздел **`map.rs485`** конфига, оставлен для данных **батареи и
  сети** МАП на дашборд. Цель `MAP (батарея/сеть)` (192.168.13.74, unit 1).
  Пер-слотовый MPPT через Modbus больше не опрашивается. Источник задаёт
  обязательное поле `disabled` подраздела `map.rs485` (отсутствие = ошибка конфига):
  - `false` — **Modbus TCP/RS485** (`modbusmap/` + `mapClientFor`), блоки 0x400/0x530/0x580;
  - `true` — пулер по Modbus НЕ запускается, параметры из **веб-API ПАК «Малина»**
    `read_json.php?device=map` (`mpptSite.FetchMAP` + `mapMAPAPI`); обязателен полный
    раздел `map` (поля веб-API, иначе `log.Fatal` при старте). Ключ устройства (devKey)
    и имя — те же (IP МАП из конфига), поэтому история на дашборде преемственна при
    переключении режимов. Путь к `read_json.php?device=map` — поле `map_path` раздела
    `map` (при отсутствии выводится из `mppt_path` подменой `device`).

Верхнеуровневый **`map.disabled`** (обязательное поле раздела `map`) — мастер-выключатель
всего раздела: при `true` НЕ запускаются пулеры МАП (и Modbus, и веб-API) и MPPT, блок
«Данные МАП» на дашборде скрывается; на пулеры сетевых инверторов не влияет.
**`map.bms_disabled`** (обязательное) отключает только пулер ANT BMS (батарейки на
главной странице скрываются).

## `values` углов МАП/MPPT (теги в `commonContractTags`)

- **MPPT API** (`mapMPPTAPI`): `pv1_voltage/current/power` = `Vc_PV`/`Ic_PV`/`P_PV`
  панелей контроллера; `l1_voltage` = `V_Bat` (напряжение АКБ); `l1_current` =
  `I_Ch` (ток заряда); `ac_active_power` = `P_Out`; `energy_today` = `Pwr_kW` +
  `Pwr_W/1000` (выработка за сутки, кВт·ч). `timestamp` снимка = поле `timestamp`
  ответа API.
- **MAP Modbus** (`mapMAPRegisters`): `l1_voltage`=АКБ, `l1_current`=ток заряда
  (**в режиме заряда MODE 0x400==4 — со знаком «минус»,** согласовано с API-веткой);
  `ac_active_power`=V×I; `grid_frequency`=0; и для дашборда: `grid_voltage` (`_UNET`
  0x422, 0 → нет сети, иначе +100 В), `grid_power` (`_PNET` 0x59A/0x59B, sign
  `_PNET_Sign_P` 0x587), `battery_voltage` (`_UAcc_med` 0x405/0x406,
  `(VH*256+VL)/10`), `battery_power` (`_PLoad` 0x59E/0x59F, `((H*256+L)/8)*100`).
- **MAP API** (`mapMAPAPI`, при `map.rs485.disabled=true`): тот же контракт из
  `read_json.php?device=map`: `battery_voltage`/`l1_voltage` = `_Uacc`, `l1_current`
  = `_Iacc`, `ac_active_power` = `_Uacc×_Iacc`, `grid_frequency` = `_TFNET`,
  `grid_voltage` = `_UNET` (уже в В, без смещения +100), `grid_power` =
  `_PNET_calc` (= `_UNET`×`_INET`, достоверная; сырой `_PNET` у МАП занижен ~в 5 раз
  против счётчика, поэтому не используется — фолбэк на него только при отсутствии
  `_PNET_calc`), `battery_power` = `−_PLoad` (инверсия знака — в API `_PLoad` для
  отдачи отрицательный, контракт Modbus требует отдачу положительной). Время снимка
  — текущее время опроса, а не `timestamp` ответа API (МАП отдаёт его
  устаревшим/некорректным, что увело бы точки на годы в прошлое).

## Ключ устройства и цикл опроса

- **Ключ в Redis/PG = `devKey(t)`**: для kindMPPT и kindMAP с slot —
  `IP#mppt<slot>` (напр. `192.168.13.60#mppt0`), чтобы контроллеры не сливались в
  одну колонку/ряд. `slot<0` у kindMAP — база батареи/сети (ключ = IP).
- **Опрос — 1 раз в секунду** (отдельный цикл `runMapPoll`, не в 10-сек циклах
  инверторов). `PollDevice` (case `kindMAP`) читает блоки `modbusmap`: 0x400
  (0x20 слов = 0x400..0x43F), 0x530 (`_I_Akb_MPPT`), 0x580 (`_PNET_Sign_P` 0x587,
  `_PNET` 0x59A/0x59B, `_PLoad` 0x59E/0x59F). Блок 0x420 отдельно не запрашивается —
  это подмножество блока 0x400..0x43F.
- Запись — через `SaveSnapshotWindow` (`redis_store.go`, см.
  [storage.md](storage.md)): score = начало 10-секундного окна (`ts.Truncate(10s)`;
  для MPPT — от `timestamp` ответа API), в пределах окна новая запись заменяет
  предыдущую — ровно одна строка за каждые 10 с. Аккумуляцией PG усредняется как у
  остальных (~1 точка за 10 с с дискретностью 5 мин).

## Связанные документы

- [../read_json.md](../read_json.md) — форматы `read_json.php?device=map|mppt|bat`.
- [../malina-web-api.md](../malina-web-api.md) — устройство ПАК «Малина», shm,
  все PHP-эндпоинты.
- `docs/map/` — байт-карты/Modbus-регистры МАП (справочные материалы реверса).
- [Дашборд](dashboard.md) — плашки/графики МАП.