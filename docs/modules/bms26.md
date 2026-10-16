# BMS через malina26 (пулер `bms26`)

ANT BMS и EnBMS переехали в ПАК «Малина26» в **единый тип BMS**: телеметрия снимается
одним пулером через HTTP-API, хранится одинаково. ANT и EnBMS больше не различаются в
хранении — тип BMS нужен только для **раскладки тревог/ошибок** (отдельный этап).

## Источник данных (API «Малина26»)

Хост/порт/пароль — тот же ПАК «Малина26», что и у метода `map=malina26` (см.
[`map-mppt.md`](map-mppt.md)). Авторизация — как у остальных ручек «Малины26»:
`POST /api/token {password}` → `Bearer` (переавторизация на 401).

- `GET /api/bms/devices` → `[{type:"bms", id, gen, serial, base, bms_type, source,
  framing, last_seen}]`. `bms_type` — `AntBMS-DSP` | `EnBMS-RS485` (нужен для трактовки
  `alarm`); `serial` — наш ID (`BASE-NNNNN`).
- `GET /api/bms/raw?id=<id>&gen=<gen>` → бинарный контракт **107 Б** (per slot);
  несовпадение `id+gen`/пустой слот → **404**.

Клиент и пулер — `cmd/sunreceiver/bms26.go`.

## Бинарный контракт (копия)

Контракт BMS **скопирован** в sunReceiver (не импортируется по сети/через replace),
чтобы сборка была самодостаточной:

- копия: **`internal/bmscontract/bmscontract.go`** (пакет
  `github.com/galiy/sunReceiver/internal/bmscontract`);
- **оригинал (bring-up)**:
  `https://github.com/galiy/malina-26/blob/main/daemons/lib/bmscontract/bmscontract.go`
  (модуль `github.com/galiy/malina-26/daemons`, локально —
  `../cleaning.malina/daemons/lib/bmscontract/bmscontract.go`);
- в шапке копии указаны origin и commit снимка.

Контракт развивается (**V2/V3**): при изменении оригинала **обновить копию**
(перенести оффсеты/поля и шапку), отдельного авто-подхвата нет. Декодер
`decodeBms26Contract` (`bms26.go`) вызывает `bmscontract.Decode` — правки оффсетов
делаются в пакете-копии, не в декодере. Тест `bms26_test.go` строит payload через
`bmscontract.Encode`, поэтому самодостаточен к версии.

Документация контракта (оригинал): `cleaning.malina/docs/bms-reader.md` §1 (раскладка
107 Б, big-endian), архитектура — `cleaning.malina/docs/bms-reader-arch.md` (§12),
типы и раскладка `alarm` — `cleaning.malina/docs/bms/{antbms-dsp,enbms-rs485}.md`.

Поля контракта (кратко): `cell_count`, `temp_count`, `flags` (charge/discharge MOS,
balancer), `cells_mv[16]`, `temps_dc[8]`, `current_da` (разряд «+», заряд «−»), `power_w`,
`soc_dp`, `soh_dp`, `cap_mah`, `remaining_mah`, `rated_mah`, `cycles`, `total_cv`,
`port_cv`, `balance_mask`, `mode` (канонический код в младшем байте), `alarm[19]`.

## Конфигурация

Отдельного раздела у BMS **нет** — используется уже существующий блок
**`map.malina26`** (`host`, `port`, `password`): тот же ПАК «Малина26», общий
HTTP-клиент и токен (см. [`map-mppt.md`](map-mppt.md)). Пулер BMS запускается
автоматически при `map.method = "malina26"`; при другом методе (`rs485`/`malina`)
источник «Малина26» не сконфигурирован и опрос BMS не идёт.

```json
"map": { "disabled": false, "method": "malina26",
         "malina26": { "host": "192.168.13.60", "port": 80,
                       "password": "CHANGE_ME_PASSWORD", "serial": "51510" } }
```

Старые разделы `antBms` (read_bms.php) и `enBms` (BLE/RS485 напрямую) сохранены в
коде, но на проде **отключены** (`disabled=true`).

## Хранение

Обособлено от прежних ANT (`sunreceiver:bms`) и EnBMS (`sunreceiver:enbms:*`):

- **Redis current**: HASH `sunreceiver:bms26:current`, поле = `serial`, значение = JSON
  `bms26Snapshot`;
- **Redis series**: месячный ZSET `sunreceiver:bms26:series:<YYYY-MM>` (score = Unix-сек,
  member = JSON снимка; каждое снятое показание);
- **PG**: таблица `sunreceiver.bms26_averages (name text, ts timestamptz, values jsonb,
  PK(name,ts))` — 5-минутные средние (аккумулятор в памяти процесса, `bms26_store.go`).

Агрегация в 5-минутном баке (спека `bms-reader.md` §1): `flags`/`balance_mask`/`alarm` —
побитовое ИЛИ; `cells_*`/`temps_*`/`current`/`power`/`soc`/`soh`/`remaining`/`total`/
`port` — среднее; счётчики/ёмкости/`mode` — последнее; `cell_count`/`temp_count` —
последнее. Производные max/min/avg по ячейкам вычисляются, в контракте не хранятся.

Ключ устройства — `serial` (напр. `AntBMS-DSP-160-00001`, `EMU110110S-…`).

## Этапы

1. **Пулер и хранение** (сделано): сбор телеметрии в Redis + PG, чтобы статистика
   собиралась.
2. **UI** (сделано): страница BMS (`/bms/<serial>`), батарейки на главной (`/api/bms`)
   и графики деталей (`/api/bms/<serial>/series`) переведены на новый ключ `bms26` и
   единый тип. Снимок `bms26Snapshot` адаптируется в прежнюю форму `bmsDevice`
   (`bmsDeviceFromBms26`, `kind="bms26"`), поэтому страница/JS переиспользованы; при
   `method=malina26` legacy ANT/EnBMS-ключи не подмешиваются (их пулеры отключены).
2.5. **Тревоги/ошибки** (сделано): `alarm` (19 Б) трактуется по `bms_type`
   (`bms26_alarm.go`); человекочитаемый список кладётся в `bms26Snapshot.alarms`
   (Redis/PG, страница BMS) и группируется на странице по категориям.
   - **EnBMS**: ячейки (4 бита/ячейку: HVwarn/OVprot/LVwarn/UVprot), датчики (4 бита),
     одиночные warn (биты 96..99), `Ext_Bit` Byte0..5 (Warn/Protect). По-ячеечные и
     по-датчиковые маски (`cell_*_mask`/`temp_*_mask`) подсвечивают проблемные ячейки
     (красная рамка — защита, оранжевая — предупреждение) и датчики.
   - **AntBMS**: коды MOS (charge/discharge/balancer) + `system log`; незнакомые коды
     не теряются (`code N`). По-ячеечных бит в alarm нет.
   - Показ на странице BMS (`#bmsAlarms`, `renderAlarms` — раскладка по категориям),
     плюс режим (`mode`), номинальная ёмкость (`rated_mah`), напряжение пакета из
     контракта (`total_cv`) и напряжение порта (`port_cv`) плашками. Появление тревоги
     пишется в `device_errors` (`kind="bms"`, `device=serial`, только появление) — видно
     и в истории на странице BMS, и на общей странице «Ошибки» (`/errors`).
   - Тексты тревог переведены (ru/en/zh): i18n `i18n.js` переводит и сами фразы
     (`высокое напряжение…`, `защита от…`, одиночные warn, Ext_Bit), и подписи групп.
3. **Миграция истории** (сделано ранее): разовыми Python-скриптами перенесены ряды
   из
   `sunreceiver:bms:series:*`/`enbms:series:*` и PG `bms_averages`/`enbms_averages` в
   `sunreceiver:bms26:*`/`bms26_averages` (JSON приводится к форме `bms26Snapshot`;
   дедуп). Связка id: ANT — по ёмкости (`AntBms <cap> A/h` → `AntBMS-DSP-<cap>-NNNNN`),
   EnBMS — единственный MAC → единственный EnBMS-serial. Также перенесена **история
   ошибок** `device_errors`: `kind='antbms'`/`'enbms'` → `kind='bms'`, `device` → serial.
   `current` не мигрируется (перезаписывается пулером). Старые данные удалены
   (старые ключи/таблицы и строки `device_errors`). В бинарнике миграций нет
   (правило проекта: миграции — только скриптами).

## Связанные документы

- [`docs/bms-reader.md` (malina-26)](../../cleaning.malina/docs/bms-reader.md) — контракт и общие циклы;
- [`docs/bms-reader-arch.md` (malina-26)](../../cleaning.malina/docs/bms-reader-arch.md) — архитектура;
- [`map-mppt.md`](map-mppt.md) — клиент «Малина26» (token/Bearer), общий с BMS;
- Старые модули: [`../antbms.md`](../antbms.md), [`enbms.md`](enbms.md).
