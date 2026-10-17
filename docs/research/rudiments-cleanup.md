# Выпиливание рудиментов — сбор требований

> **Статус:** требования собраны, решения по вопросам приняты (2026-10-17).
> Код **НЕ менялся** — реализация не начата.
> **Дата:** 2026-10-17.
> **Цель:** навести порядок и удалить рудиментарные методы пулинга/модули.
> Реализация — отдельными заявками.
> **Приватность:** реальные серийники устройств заменены плейсхолдерами
> (`<PRIMARY_SERIAL>`, `<FALLBACK_SERIAL>` и т.п.); фактические значения — в
> приватной конфигурации/заметке, не в git.

Документ фиксирует заявки, инвентарь, результаты исследования и принятые решения.

---

## 1. МАП: оставить только метод `malina26`

**Заявка.** Из пулинга МАП оставляем единственный способ — HTTP-API «Малина26»
(токен по паролю + бинарные контракты; поле `method` в конфиге убирается).
Остальные методы выпиливаем полностью.

**Оставляем:**
- единственный способ — HTTP-API «Малина26»: МАП (`/api/map/raw`) и КЭС
  (`/api/kes/raw`); плоский блок конфига
  `map.{disabled,host,port,password,serial,fallback_serial}`;
- `malina26.go` — файл сохраняется, но правится (fallback, удаление проверки
  `method`, запуск BMS26);
- хранилище МАП/КЭС (ключи `sunreceiver:map:*`, `sunreceiver:kes:*`, `sunreceiver:ts:map:*`,
  `sunreceiver:ts:kes:*`, PG `map_averages_r`, КЭС-таблицы) — оно общее для `malina26`.
  `map.disabled` остаётся **обязательным**.

**Удаляем:**
- `method=rs485` — опрос МАП по Modbus TCP/RS485: блок конфига `map.rs485`,
  Modbus-клиент МАП (`mapClientFor`), маппинг `mapMAPRegisters`, ветка `kindMAP` в
  `pollDevice`, Ц-демон `daemons/mapgateway/` и цель сборки `mapgateway`;
- `method=malina` — legacy веб-API ПАК «Малина» (`read_json.php?device=map` /
  `?device=mppt`, `read_memory.php`): блок конфига `map.malina`, `mpptSite` и весь
  `cmd/sunreceiver/mppt_api.go`, `mapAPISource`, опрос MPPT через `FetchMPPTs`,
  `pollMAPAPI`, `pollMPPTFromArr`;
- чтение ошибок МАП через Modbus и `read_memory.php` — остаётся только источник
  `malina26.mapErrCells` (`/api/map/raw`);
- тесты удаляемого (`mppt_api_test.go` и т.п.), упоминания в доках/примерах.

**Не трогаем:** пакет `internal/modbusmap` — он переиспользуется счётчиками
DDS238/DTS017M (`meter_client.go`), это не рудимент МАП. EnBMS RS485 использует
собственный PACE ASCII.

**Вопрос по данным:** данных, специфичных именно для `rs485`/`malina`, нет —
все методы писали в одни и те же ключи `sunreceiver:map:*` / `sunreceiver:kes:*`
и PG-таблицы. После удаления методы перестают писать, история остаётся
совместимой. Отдельной миграции не требуется.

---

## 2. ANT BMS: выпилить как отдельный пулер и метод

**Заявка.** Удалить ANT BMS как отдельный пулер (`read_bms.php` ПАК «Малина») и
как отдельный метод/раздел конфига. BMS26 (единый тип ANT+EnBMS через `malina26`)
остаётся.

**Удаляем:**
- раздел конфига `antBms` (`{"disabled","url"}`) и его обязательность;
- пулер `runBmsPoll` / `pollAndSaveBMS` / `saveBMSReading` (`bms_poller.go`),
  клиент `read_bms.php` (`bmsApiClient`/`bmsSite`, `loadBmsSite`);
  **исключение:** `bmsDevice` (тип) и `bmsKey()` используются живым BMS26 и
  дашбордом (`bms26.go`, `dashboard.go`) — их вынести в общий файл, не удалять;
- Ц-демон `daemons/bmslistener/` и цель сборки `bmslistener`;
- ANT-часть «костыля» `bms26_legacy.go` (`antDeviceToBms26`, `antCapFromName`,
  `legacyAntSerial`, `antAlarmBytesFromCodes` и связанные таблицы);
- тесты только для legacy (`resolveBMSKey`, `recomputeMinMaxCells`, `decodeAntAlarms`);
  тест `bmsKey` **перенести** в общий файл (напр. `bms_view_test.go`), остальной
  `bms_poller_test.go` удалить.

**Не трогаем:**
- BMS26 через `malina26` (`bms26.go`, `bms26_store.go`) — единый тип ANT+EnBMS,
  ключи `sunreceiver:bms26:*`, PG `bms26_averages_r`;
- `bmsSeriesPoint`/`bmsAveraged` (`bms_accumulator.go`) — используются живым BMS26
  и дашбордом;
- `finalizeBms26` — нужен живому BMS26;
- EnBMS-часть `bms26_legacy.go` — удаляется вместе с EnBMS (раздел 3).

**Удаляем вместе с legacy-кэшом:** `cacheBms26Snapshot`, `loadBms26SerialCache`,
`bms26Serials` — нужны только legacy-serial’ам. При этом править обращения:
`bms26.go:308` (вызов `cacheBms26Snapshot`) и `main.go:2331`
(`loadBms26SerialCache`).

**Факт по накопленным данным (read-only проверка прод-БД, 2026-10-17):**

- Полный аудит: Redis **DB0…DB15** — ключей `*bms*`/`*enbms*`/`*ant*` вне `bms26`
  нет; `sunreceiver:bms` и `sunreceiver:bms:current` **не существуют**.
  В PG таблиц с `bms`/`ant`/`enbms` вне `bms26_*` нет; `bms_averages_r` отсутствует.
- Есть объединённые ключи BMS26:
  - `sunreceiver:bms26:current` — 3 устройства:
    `<ANT_SERIAL_1>`, `<ANT_SERIAL_2>`,
    `<ENBMS_SERIAL>`; снимок свежий (2026-10-17).
  - `sunreceiver:ts:bms26:*` — 195 серий (65 на устройство), ~8.2 млн сэмплов на
    устройство; доступное окно данных ~2 суток (retention 60 суток, но более
    старых точек нет).
- В PG `bms_averages_r` **не существует**; `bms26_averages_r` — ~22 014 строк,
  2026-09-14 … 2026-10-17 (живые данные).

**Вывод:** два ANT-устройства (160 А·ч и 320 А·ч) сейчас читаются **не** старым
ANT-пулером, а единым BMS26 через `malina26`, и пишутся в общие `bms26`-ключи
вместе с EnBMS. Отдельных «ANT-данных» для удаления **уже нет** (удалены ранее,
до этой задачи); удаление `bms26`-данных сломало бы живой мониторинг.
**Рекомендация — данные не трогать**, удалить только legacy-пулер/демон/раздел
конфига. Настоящей задачей мы ничего не удаляли: проверки были только на чтение.

---

## 3. EnBMS: выпилить как отдельный пулер и метод

**Заявка.** Удалить EnBMS как отдельный пулер (прямой опрос по BLE/RS485) и как
отдельный метод/раздел конфига. BMS26 через `malina26` остаётся единым типом
ANT+EnBMS.

**Удаляем:**
- раздел конфига `enBms` (`disabled`, `poll_interval_ble`, `poll_interval_rs485`,
  `devices[]` с `method`/`rs485`) и его обязательные поля;
- пулер EnBMS: `enBms_client.go` (BLE-транспорт), `enBms_rs485.go` (PACE ASCII),
  `enBms_poller.go`, `enBms_store.go`, протокольные парсеры `enBms.go`;
  **исключение:** `enbmsExtGroups`/`enbmsExtBit` (таблицы Ext_Bit) используются
  живым BMS26 (`bms26_alarm.go`) — вынести в общий файл, не удалять;
  `enbmsModeNames`/`enbmsCellStateBits`/`enbmsTempStateBits`/`enbmsBalanceMask` —
  **правило:** удалить, если `git grep` не находит использований вне удаляемых
  файлов; если находит — вынести в общий файл вместе с Ext_Bit;
- EnBMS-часть «костыля» `bms26_legacy.go` (`enbmsSnapshotToBms26`,
  `legacyEnbmsSerial`, `enbmsAlarmBytesFromStrings`, `legacyModeFromText`);
- тесты `enBms_test.go`, `enBms_state_test.go` — удалить; Ext_Bit покрыт
  `bms26_alarm_test.go`; в `bms26_legacy_test.go` убрать legacy-часть
  (`enbmsPackAlarm`), проверку `finalizeBms26` сохранить/перенести.

**Не трогаем:**
- BMS26 через `malina26` (`bms26.go`, `bms26_store.go`) — единый тип ANT+EnBMS,
  ключи `sunreceiver:bms26:*`, PG `bms26_averages_r`;
- BLE-PIN-агент и BLE-транспорт CE308 (`ce308_agent_*`, `ce308_client.go`) —
  CE308 final-режим от них зависит.

**Факт по накопленным данным (read-only проверка прод-БД, 2026-10-17):**

- Redis-ключей `sunreceiver:enbms*` (и `*enbms*`) **нет**.
- EnBMS-устройство (`<ENBMS_SERIAL>`) присутствует в
  общих `sunreceiver:bms26:current` / `sunreceiver:ts:bms26:*` (65 серий,
  ~8.2 млн сэмплов за доступное окно ~2 суток; retention 60 суток) и живо
  пишется через `malina26`.
- В PG отдельных таблиц EnBMS нет; всё в `bms26_averages_r` (и партициях
  `*_cells/_temps/_alarm`).

**Вывод:** как и у ANT, отдельных «EnBMS-данных» для удаления **уже нет** — это
тот же живой унифицированный поток `bms26`, куда EnBMS-пулер писал через
«костыль». Данные не трогать; удалить только legacy-пулер/раздел конфига. Задачей
ничего не удалялось (проверки только на чтение).

---

## 4. CE308: оставить только режим `final_readings_only`

**Заявка.** Для счётчика CE308 (BLE) оставить единственный режим —
`final_readings_only` (без постоянного BLE-соединения; раз в 30 мин или по кнопке
снимается только накопленная энергия, телеметрия/ряды не пишутся). Обычный режим
(постоянное соединение, телеметрия current/series + 5-мин средние в PG) выпилить.

**Оставляем:**
- режим `final_readings_only` и его хранилище: снимок энергии
  `sunreceiver:ce308-energy:current` (обновляется, панель «показания»);
- поле `mac`, `pin`, BLE-транспорт и агент PIN (`ce308_agent_*`).

**Удаляем:**
- обычный режим: постоянное BLE-соединение и телеметрию (`ce308_active_power` и
  пр.), запись `sunreceiver:ce308:current` HASH, ряд `sunreceiver:ts:ce308:*`,
  5-минутные средние в PG `ce308_averages_r`;
- поле `ce308.final_readings_only` и `poll.ce308` — **убрать** (решение
  2026-10-17; конфиг мигрируется с деплоем);
- ветки дашборда/API, отдающие телеметрию/ряды CE308 (рамка «текущие данные»),
  соответствующие тесты и доки.

**Факт по накопленным данным (read-only проверка прод-БД, 2026-10-17):**

- Redis:
  - `sunreceiver:ce308:current` (HASH, 1 поле `<CE308_NAME>`) — снимок
    телеметрии от **2026-10-05 19:27** (устарел; обычный режим уже не пишется);
  - `sunreceiver:ce308-energy:current` — снимок энергии от **2026-10-17 09:11**
    (свежий; режим `final_readings_only` активен);
  - серий `sunreceiver:ts:ce308:*` **нет** (retention 60 суток, поэтому это не
    истечение — вероятно, ручное удаление/сброс Redis; причину зафиксировать при
    cleanup, удалять префиксом безусловно).
- PG:
  - `ce308_averages_r` — **49 840 строк**, все `ts` уникальны (`count = count
    DISTINCT ts`), одно имя `<CE308_NAME>`, 2026-09-23 … 2026-10-05. Плотность
    объясняется сменой шага: 24–30.09 — исторические **10-секундные** средние
    (старый `ce308AvgStep=10s`, ~6.8–8.1 тыс./сутки), 01–05.10 — **5-минутные**
    (~250–290/сутки). Это не дубли.
  - legacy-таблица `sunreceiver.ce308_averages` — **отсутствует** (проверено);
  - таблиц `ce308_daily_tariffs` / `ce308_energy` нет.

**Вывод:** живые данные `final_readings_only` — только
`sunreceiver:ce308-energy:current`; их удалять нельзя. Артефакты обычного режима
(`ce308:current` HASH и PG `ce308_averages_r`) после удаления режима становятся
осиротевшими.

**Решение (2026-10-17): очистить, ключи/таблицы удалить.**
- Удалить ключ телеметрии `sunreceiver:ce308:current` (HASH, устарел 2026-10-05).
- Удалить ряды `sunreceiver:ts:ce308:*` по префиксу (`SCAN`, не полагаться на
  «сейчас их нет»).
- Удалить таблицу PG `ce308_averages_r` **вместе с партициями** `*_pYYYYMM`
  (49 840 строк) — писатель удаляется вместе с обычным режимом.
- **Оставить** `sunreceiver:ce308-energy:current` — это данные режима
  `final_readings_only` (живые итоговые показания, панель «показания»).
- Выполнять отдельным Python-скриптом с dev-ПК напрямую в БД (правило проекта),
  не в бинарнике.
- **Порядок (уточнено после ревью):** старый бинарник при старте
  (`openPG → ensureAvgSchema`) пересоздаёт родительскую `ce308_averages_r` из
  `avgSpecs`, поэтому:
  1) реализация и деплой кода **без** CE308-спеки (деплой — по отдельной команде);
  2) затем cleanup-скрипт (повторный запуск идемпотентен);
  3) после рестарта — контроль: таблицы нет, ключей `ce308:current`/`ts:ce308:*`
     нет, `ce308-energy:current` на месте.
  «Сразу» означает «отдельным шагом сразу после удаления писателя», а не до него.

---

## 5. Инвентаризация удаляемого (backend + web + инфраструктура)

Ниже — полный инвентарь по каждому пункту. Пометки: **У** — удалить,
**О** — оставить. Все ранее открытые «?» закрыты решениями (раздел 11).

### 5.1. MAP (методы `rs485` и `malina`)

Backend/конфиг:
- **У** `mapRS485Section`, `mapMalinaSection`, поля `map.rs485` / `map.malina` в
  `mapSection`; ветки `rs485` / `malina` в `loadConfig`.
- **У** `mpptSite`/`loadMPPTSite`, `mapAPISource`, `mapAPI`, `mppt` и весь
  `cmd/sunreceiver/mppt_api.go`; `pollMAPAPI`, `pollMPPTFromArr`, `FetchMPPTs`,
  `FetchMAP`, `FetchPNETSign`, `FetchMemory`, `mapMAPAPI`, `mapMPPTAPI`.
- **У** Modbus-ветка МАП: `mapClientFor`, `mapClientsByKey`, `mapMAPRegisters`,
  ветка `kindMAP` в `pollDevice`, Modbus-ветка `readMapErrCells`.
- **У** `method=malina` в проверке конфига и `log.Fatalf` про неполный блок.
- **О** способ `malina26` (без поля `method`), `malina26.go`, `pollMAPMalina26`, `FetchKES`,
  `mapErrCells`; `internal/modbusmap` (счётчики).

Инфраструктура:
- **У** `daemons/mapgateway/`, цель `mapgateway` в `Makefile`.
- **У** `cmd/probe/map/` (tracked, проба МАП по Modbus); `cmd/probe/main.go`
  (Solarman) — **О**.
- **У** legacy-доки: `docs/read_json.md`, `docs/malina-web-api.md`,
  `docs/malina-bug_Inet_flag.md`, `docs/research/map-signs-audit.md`
  (решение 2026-10-17 — удалить). `docs/requirements/` — локальный
  gitignored-каталог, в git-объём не входит.

Web:
- **О** все панели МАП/КЭС на дашборде и страницы графиков — они не привязаны к
  методу и обслуживаются `malina26`. Отдельного web-UI под `rs485`/`malina` нет.

### 5.2. ANT BMS (legacy-пулер `read_bms.php`)

Backend/конфиг:
- **У** `antBmsSection`, поле `configFile.AntBms`, блок загрузки раздела `antBms`,
  `antBmsSec`.
- **У** `bms_poller.go` (`runBmsPoll`, `pollAndSaveBMS`, `saveBMSReading`,
  `bmsApiClient`, `bmsSite`, `loadBmsSite`, `fetch`), `bms_poller_test.go`;
  **исключение:** тип `bmsDevice` и `bmsKey()` используются живым BMS26 и
  дашбордом — вынести в общий файл; тест `bmsKey` перенести туда же.
- **У** ANT-часть `bms26_legacy.go`: `antDeviceToBms26`, `antCapFromName`,
  `capFromRe`, `legacyAntSerial`, `antAlarmBytesFromCodes`, `antNameCap`;
  **У** legacy-кэш `cacheBms26Snapshot`/`loadBms26SerialCache`/`bms26Serials`
  (править вызовы `bms26.go:308`, `main.go:2331`);
  **О** `finalizeBms26` (живой BMS26).
- **У** запуск `runBmsPoll` и ветка `bmsSite` в `main.go`; `ShowBMS` считается
  только по `bms26 != nil` (после удаления EnBMS — только `bms26`).
- **У** раздел `antBms` из `sunReceiver.json` / `sunReceiver.sample.json`.

Инфраструктура/доки:
- **У** `daemons/bmslistener/`, цель `bmslistener` в `Makefile`.
- **У** `docs/antbms.md`, `docs/antbms-worklog.md`, `docs/modules/bms-listener.md`,
  каталог `docs/antbms/`.

Web:
- **О** BMS-страница и панель батареек — это единый BMS26 (ANT+EnBMS), устройства
  остаются, отображаются по `sunreceiver:bms26:*`.
- **У** фильтр типа ошибок `antbms` в `web/templates/errors.html` и
  `web/static/js/errors.js` (`KIND_LABEL`) — решение 2026-10-17: удалить (новых
  записей с таким kind не будет; история доступна без фильтра).
- **О** тексты AGM «ANT×2 + EnBMS» — состав BMS26 не меняется.

### 5.3. EnBMS (прямой пулер BLE/RS485)

Backend/конфиг:
- **У** `enBmsConfigFromSection`, `enBmsSection`/`configFile.EnBms`, блок загрузки
  раздела `enBms`, `enBmsCfg`, `describeEnBmsConfig`.
- **У** `enBms_client.go`, `enBms_rs485.go`, `enBms_poller.go`,
  `enBms_store.go`, протокольные парсеры `enBms.go`, `enBms_state_test.go`,
  `enBms_test.go`; **исключение:** таблицы `enbmsExtGroups`/`enbmsExtBit`
  используются живым BMS26 (`bms26_alarm.go`) — вынести в общий файл.
- **У** запуск `runEnBmsPoll` и проверка лимита BLE-устройств (`enBms + CE308`) в
  `main.go`.
- **У** EnBMS-часть `bms26_legacy.go`: `enbmsSnapshotToBms26`,
  `legacyEnbmsSerial`, `enbmsAlarmBytesFromStrings`, `legacyModeFromText`;
  вспомогательные map-таблицы — по правилу `git grep` (вне удаляемых файлов →
  вынести, иначе удалить).
- **У** `cacheBms26Snapshot`/`loadBms26SerialCache`/`bms26Serials` (legacy-кэш;
  править вызовы `bms26.go:308`, `main.go:2331`); **О** `finalizeBms26`.
- **У** раздел `enBms` из `sunReceiver.json` / sample.

Доки:
- **У** `docs/modules/enbms.md`, tracked-каталог `enBms/`
  (`context.md`, `enBms-monitoring.md`).

Web:
- **О** BMS-страница/панель — единый BMS26.
- **О** CSS `.cell.warn` / `.cell.prot` в `bms.css` — это подсветка ячеек BMS26
  (EnBMS-устройство продолжает отображаться через `malina26`).
- **У** фильтр `enbms` в `errors.html` / `errors.js` — решение 2026-10-17:
  удалить (как и `antbms`).
- **О** BLE-агент `ce308_agent_linux.go` — он CE308-специфичный, EnBMS его не
  использует.

### 5.4. CE308 (обычный режим; остаётся `final_readings_only`)

Backend:
- **У** обычный режим пулера в `ce308_poller.go` (постоянное BLE-соединение,
  телеметрия current/series); **О** `final_readings_only` (снимок энергии раз в
  30 мин / по кнопке).
- **У** `ce308_accumulator.go` и запись 5-мин средних в PG `ce308_averages_r`.
- **У** в `ce308_store.go`: `SaveCE308Current`, `CE308Current`, `SaveCE308History`
  (именно он пишет series; метода `SaveCE308Series` нет), `QueryCE308Series`;
  **О** `SaveCE308Energy` / `CE308Energy`.
- **У** в `dashboard.go`: `apiCE308Current` (`/api/ce308/current`),
  `apiCE308Series` (`/api/ce308/series`), `ce308SeriesRange`, `ce308SeriesPoint`,
  поля `CE308L1/L2/L3Voltage`, `CE308ActivePower` в ответе графиков; downsample —
  только у `CE308L1Voltage`/`CE308ActivePower`; **О** `apiCE308Energy`.
- **У** в `pg_store.go`: `InsertCe308Average`, `QueryCE308Averages`,
  `CE308AveragesAll`; spec `ce308_averages_r` из `avg_r.go`; запись CE308 из
  `avgMigMaps` (`migrate_avg.go`) и `TABLES` (`scripts/step1_averages_partitioned.py`),
  CE308-ветку `scripts/migrate_zset_to_rt.py`; добавить nil-guard в
  `migrateAvgScalar`.
- **У** в `main.go`: восстановление CE308-ряда (`restoreCE308Series`,
  `CE308AveragesAll` → `SaveCE308History`/`SaveCE308Current`); удалить
  `redisCE308DataPresent` целиком и её вызов (`main.go:2289-2300`), обновить
  строку лога `main.go:2299-2300` (убрать CE308); убрать `redisCE308CurrentKey`
  из `IsEmpty` (`redis_store.go:224`). **Поведение:** `IsEmpty` не учитывает
  CE308-телеметрию; `ce308-energy:current` не учитывается (Redis с единственным
  energy-ключом считается пустым — реставрация остальных рядов не блокируется).
- **У** в `main.go`/`dashboard.go` флаг `CE308FinalReadingsOnly`:
  `main.go:2203`, ветка выбора цикла `main.go:2471` (остаётся безусловный
  `runCe308FinalReadings`), `dashboard.go:88-91` и проверки `:1358,1952,3459,
  3528,3616`; `restoreCE308Series` удаляется целиком.
- **У** резерв CE308 для мощности «гаража» в анимации: `freshCE308Power`,
  `ce308Name`, параметр `ce308` в `buildAnimationResponse` / `apiAnimation`
  (основной источник — DTS017M).
- **О** `ce308_client.go`, `ce308_agent_*` (BLE-транспорт и PIN нужны final-режиму),
  `ce308.go` (маппинг энергии), `ce308CaptureEnergy` (`ce308_poller.go`),
  `SaveCE308Energy` (`ce308_store.go`).
- **У** поле конфига `ce308.final_readings_only`: **убрать** (решение 2026-10-17),
  как и `poll.ce308` — конфиг мигрируется с деплоем.

Web:
- **У** рамка «Электросчётчик CE308 — текущие данные» в
  `web/templates/index.html` (блок `ce308Cur`, ~строки 73-95).
- **У** `renderCE308Current` и опрос `/api/ce308/current` в
  `web/static/js/dashboard.js`.
- **У** ряды CE308 в `web/static/js/charts.js` (напряжения/активная мощность,
  `showCE308 && !ce308Final`).
- **О** рамка «Гараж. Электросчётчик CE308 — показания» (энергия), кнопка
  «Обновить», `renderCE308Energy`, `/api/ce308/energy`.
- **У** атрибут `data-ce308-final` (режим теперь всегда final); **О**
  `data-show-ce308` (скрывает блок, если CE308 отключён). CSS: удалить классы
  текущей таблицы (`.ce308-scroll/.ce308-table/.ce-num/.ce-pos/.ce-neg`),
  оставить классы «показаний» (`.ce308-head-right/.ce308-btn/.ce308-sec*/.ce308-grid/...`).

Доки:
- **Обновить** `docs/modules/ce308.md` (убрать обычный режим, оставить final),
  `AGENTS.md`,
  `sunReceiver.sample.json`.

### 5.5. Общее

- **О** `tools/bat_stat_scripts/`, `tools/seplos_bms_server.py`, `tools/ce308stat/`,
  `tools/map-loader/` — **решение 2026-10-17: оставить локально** (каталог в
  `.gitignore`, в репозиторий не входит; в git-объём не входят).
- **О** `internal/bmscontract` — контракт BMS26 (`malina26`).
- **У** после всех правок — тесты удалённых модулей и упоминания в `AGENTS.md`,
  `docs/README.md`, `docs/modules/*`, `Makefile`, `sunReceiver.sample.json`.

---

### 5.6. Конфиг: удаляемые и упрощаемые настройки

Выпиливается не только код, но и ставшие ненужными настройки; где метод/режим
остался один — вложенность убирается, поля поднимаются на уровень выше.

- **MAP — уплощение блока.** **Решение 2026-10-17: только новый вид** — убрать
  `map.method` и вложенный `map.malina26`; поля `host`, `port`, `password`,
  `serial`, `fallback_serial` — на уровне `map`. Старый вид
  (`method`/`malina26`) — ошибка загрузки; конфиг мигрируется с деплоем.

  Было:
  ```json
  "map": {
    "disabled": false,
    "method": "malina26",
    "malina26": {"host": "…", "port": 80, "password": "…", "serial": "…"}
  }
  ```
  Стало:
  ```json
  "map": {
    "disabled": false,
    "host": "…",
    "port": 80,
    "password": "…",
    "serial": "…",
    "fallback_serial": "…"
  }
  ```
  (`fallback_serial` — необязательный резервный серийник, см. раздел 8.)

- **Удалить разделы конфига целиком:** `antBms`, `enBms`.
- **`poll`:** убрать поля `antbms` (ANT-пулера нет) и `ce308` (в
  `final_readings_only` период фиксирован — 30 мин, `poll.ce308` не используется);
  обязательными остаются `inverter`, `map`, `meter`.
- **`ce308.final_readings_only`:** **убрать** (решение 2026-10-17) — режим
  единственный; поле больше не читается.
- **`poll.ce308`:** **убрать** (решение 2026-10-17) — в final-режиме период
  фиксирован (30 мин). Конфиг мигрировать вместе с деплоем; старый вид — ошибка
  загрузки.
- **`map.rs485` / `map.malina`:** убрать (следствие удаления методов).
- **Общий контроль после удаления:** `git grep` по `mapSection`/`pollSection`/
  `ce308Section` и конфиг-полям — не осталось неиспользуемых полей/структур.
  (`mapRS485Section`/`mapMalinaSection`/`mppt_path` удаляются целиком вместе с
  методами.)

Обязательно синхронизировать: `sunReceiver.sample.json` + локальный и продовый
`sunReceiver.json` (правило проекта — конфиг на проде и локальный совпадают).

---

## 6. Согласование (чек-лист)

- [ ] MAP: удалить `rs485` + `malina`, `mapgateway`; legacy-доки MAP — **решено
      удалить** (`read_json.md`, `malina-web-api.md`, `malina-bug_Inet_flag.md`,
      `map-signs-audit.md`).
- [ ] ANT BMS: удалить пулер + `bmslistener` + раздел; фильтр `antbms` в ошибках —
      **решено удалить**.
- [ ] EnBMS: удалить пулер + раздел; фильтр `enbms` в ошибках — **решено удалить**.
- [ ] Ошибки: добавить фильтр/подпись `bms` (живой kind) в `errors.html`/`errors.js`.
- [ ] CE308: удалить обычный режим + `ce308_averages_r`-писатель; поля
      `final_readings_only` и `poll.ce308` — **решено убрать жёстко** (конфиг
      мигрируется с деплоем).
- [ ] `tools/*` (bat_stat_scripts, seplos, ce308stat, map-loader) — **решено:
      оставить локально**, вне git-объёма.
- [ ] `cmd/probe/map` — удалить; `cmd/probe/main.go` — оставить.
- [ ] `enBms/` (tracked доки) — удалить; CE308-ассеты — оставить.
- [ ] `map:current`: удалить только поле `""`; 477 пустых PG-строк
      (`name=''/device_sn=''`, все метрики NULL) — удалить с backup/dry-run.
- [ ] Конфиг: MAP уплостить (**решено: только новый вид**, без `method`/`malina26`);
      убрать `antBms`, `enBms`, `poll.antbms`, `poll.ce308`; `ce308.final_readings_only`
      и `poll.ce308` — **решено убрать жёстко**.
- [ ] MAP fallback: `map.fallback_serial` (<FALLBACK_SERIAL>) — опрос при отсутствии основного,
      идентичность/ключи/`device_sn` остаются за основным (<PRIMARY_SERIAL>).
- [ ] Данные: старых ANT/EnBMS-ключей/таблиц нет — удалять нечего, `bms26` не
      трогать. CE308 — **решено**: телеметрию очистить (`ce308:current`, `ce308_averages_r`,
      `ts:ce308:*`), `ce308-energy:current` оставить.

---

## 7. Данные: результаты проверки и решения

Проверка прод-БД (только чтение, 2026-10-17): старых отдельных данных ANT/EnBMS
**нет** — ни в Redis (DB0…DB15), ни в PG. `sunreceiver:bms`/`bms:current` не
существуют, `bms_averages_r` отсутствует, других таблиц вне `bms26_*` нет.
Остаётся только живой унифицированный `bms26` — его не трогаем.

| Пункт | Что накоплено | Решение |
|---|---|---|
| ANT BMS | Отдельных ANT-ключей/таблиц нет (удалены ранее). Живые ANT-устройства — в общих `bms26`-ключах / PG `bms26_averages_r`. | Удалять/переносить нечего. `bms26` не трогать. |
| EnBMS | Отдельных EnBMS-ключей/таблиц нет. Устройство — в общих `bms26`-ключах / `bms26_averages_r`. | Удалять/переносить нечего. `bms26` не трогать. |
| CE308 | Живой `ce308-energy:current` + осиротевшие `ce308:current` (2026-10-05) и PG `ce308_averages_r` (~49.8 тыс. строк, 2026-09-23…10-05). | **Решено:** очистить/удалить телеметрию (ключ `ce308:current`, таблицу `ce308_averages_r`, ряды `ts:ce308:*`); `ce308-energy:current` оставить. |

**Мы ничего не удаляли:** все проверки были read-only (`SCAN`/`GET`/`TS.INFO`/
`SELECT`; `KEYS` на живом Redis не использовать). Старые ANT/EnBMS-данные
отсутствовали ещё до начала задачи.

---

## 8. Подзадача: fallback-серийник МАП (`malina26`)

**Контекст.** МАП `<PRIMARY_SERIAL>` уезжает в ремонт; временно подключается другой прибор
`<FALLBACK_SERIAL>`. Нужно, чтобы опрос продолжался на резервном приборе, а для системы это
было то же самое устройство (та же статистика, тот же ряд).

**Конфиг (после уплощения блока `map`):**
```json
"map": {
  "disabled": false,
  "host": "…",
  "port": 80,
  "password": "…",
  "serial": "<PRIMARY_SERIAL>",
  "fallback_serial": "<FALLBACK_SERIAL>"
}
```

- `serial` — **основной** и **идентичность** устройства (<PRIMARY_SERIAL>).
- `fallback_serial` — **необязательный** резервный серийник (<FALLBACK_SERIAL>).

**Поведение:**
1. При поиске «своего» МАП в `/api/map/devices` сначала ищем `serial` (<PRIMARY_SERIAL>).
2. Если основного в списке **нет** — ищем `fallback_serial` (<FALLBACK_SERIAL>).
3. Нашли резервный — опрашиваем его `/api/map/raw?id&gen` и декодируем как обычно
   (контракт MapData тот же).
4. Если основной снова появился — возвращаемся к нему (приоритет основного).
   Пока активен резервный, список `/api/map/devices` перечитывать **раз в 60 с**
   (решение 2026-10-17), чтобы не залипнуть на <FALLBACK_SERIAL> после возврата <PRIMARY_SERIAL>.
5. **Основной есть в списке, но `/api/map/raw` даёт 2 подряд 404/HTTP-ошибки/
   ошибки декодирования** — помечаем его временно недоступным (`primaryBrokenUntil`,
   60 с) и переключаемся на `fallback_serial`; возврат на основной — только
   после успешного raw-ответа. Детали состояний — §10.2.
6. Не нашли ни одного (или оба недоступны) — «данных нет» (как сейчас), без записи
   ошибки транспорта, повтор в следующем цикле.

**Идентичность (обязательно):**
- Все алгоритмы вычисления идентификатора/серийника/ключа опираются **только** на
  значение `map.serial` (<PRIMARY_SERIAL>) из настройки — фактический серийник прибора для
  этого не используется.
- Все ключи/ряды остаются прежними: Redis `sunreceiver:map:current`,
  `sunreceiver:ts:map:*`, PG `map_averages_r` — устройство `map`.
- `device_sn` (TS-метка и колонка PG `map_averages_r`) = **<PRIMARY_SERIAL>**, как и
  `DeviceSN` в снимке/дашборде — статистика пишется как по одному устройству.
- Имя источника, уведомления, ошибки устройства — по основному `serial`.

**Фактический серийник (<FALLBACK_SERIAL>):** **не сохраняется в телеметрии/
ключах/PG/API** (решение 2026-10-17). `device_sn`/ключи всегда `<PRIMARY_SERIAL>`.
Фактический серийник используется только для выбора устройства в
`/api/map/devices`; допускается диагностический лог (единственное исключение —
лог, не данные).

**Крайние случаи:**
- Оба прибора в списке одновременно → выигрывает основной `serial`.
- Кэш `id+gen` — для фактически выбранного прибора. 404 активного fallback —
  сбросить fallback/`haveMap` и искать заново. 404/битый raw основного —
  инкремент `primaryFailStreak`; после 2 подряд основной считается временно
  недоступным (`primaryBrokenUntil`, 60 с). Единичный сбой не переключает.
- Ошибки/логи могут указывать фактический серийник для диагностики, но ключи и
  `device_sn` — всегда основной.
- Конкурентность: `fetchMapRaw` вызывают основной цикл и error-цикл — состояние
  выбора/кэш под общим `mu`.

**Критерии приёмки:**
- [ ] При отсутствии <PRIMARY_SERIAL> и наличии <FALLBACK_SERIAL> опрос идёт, данные пишутся под `map`/<PRIMARY_SERIAL>.
- [ ] При возврате <PRIMARY_SERIAL> опрос переключается обратно (без ручного рестарта), список перечитывается раз в 60 с.
- [ ] При 2 подряд 404/HTTP-ошибках/битом raw основного — переключение на резервный (60 с), возврат только после успешного raw.
- [ ] `device_sn` в Redis-метках и PG `map_averages_r` = <PRIMARY_SERIAL> в обоих режимах.
- [ ] Ни один ключ/идентификатор не вычисляется из фактического серийника; он не пишется в current/TS/PG.
- [ ] Если ни одного нет — поведение как раньше (нет данных, повтор).
- [ ] Валидация: `fallback_serial` необязателен; пустой/отсутствует — ок; равен `serial` — ошибка загрузки с текстом; `serial` и `fallback_serial` — числовые строки.
- [ ] Юнит-тесты: выбор основного/резервного, 404/битый raw, возврат, оба в списке, ни одного; подмена времени для 60-с перепроверки.

---

## 9. Порядок работ

Каждый пункт — отдельная заявка; после каждого — `go build ./...`, `go vet ./...`,
`go test ./...`, `git grep` по удалённым идентификаторам (0 совпадений).

0. **Подготовка:** ветка/бэкап, baseline сборки/тестов.
1. **MAP → только `malina26`:** удалить `rs485`/`malina`, `mapgateway`,
   `cmd/probe/map`; упростить `mapSourceIdentity`/`mapErrors`; исправить
   stale-детект `mapTracker` для `malina26`; обновить тесты/доки.
2. **MAP конфиг + fallback:** плоский `map` (`host/port/password/serial/
   fallback_serial`), убрать проверку `Method` в `loadMalina26` (MAP/КЭС/BMS26
   стартуют при `disabled=false` и валидных полях); реализовать fallback
   (60 с, 2 подряд 404/битый raw, возврат); **код-устойчивость:** в
   `MapAverages`/реставрации пропускать строки со всеми NULL-метриками (не по
   `device_sn`); тесты.
3. **ANT BMS:** удалить пулер/`bmslistener`/раздел; вынести `bmsDevice`/`bmsKey`
   в общий файл; удалить legacy-кэш (`cacheBms26Snapshot`/`loadBms26SerialCache`)
   с правкой вызовов; `finalizeBms26` оставить; тесты/доки.
4. **EnBMS:** удалить пулер/раздел; вынести `enbmsExtGroups`/`enbmsExtBit` в файл
   алармов BMS26; удалить `enBms/` (tracked доки); тесты/доки.
5. **CE308 (код):** только final — удалить обычный режим/телеметрию/web-рамку/
   API серий; убрать `poll.ce308` и `ce308.final_readings_only` (включая
   `dashFlags`/ветки выбора цикла); убрать spec `ce308_averages_r`,
   `avgMigMaps`/script-ссылки, `IsEmpty`/`redisCE308DataPresent`; тесты.
6. **Конфиг:** подготовить миграцию прод/локального `sunReceiver.json` +
   `sample` (backup, преобразование, проверка); включить явное отклонение
   legacy-полей (`map.method`, `map.rs485`, `map.malina`, `map.malina26`,
   `poll.ce308`, `poll.antbms`, `antBms`, `enBms`, `ce308.final_readings_only`).
7. **Деплой кода** — только по отдельной команде пользователя (не автоматически).
8. **Data-cleanup (Python-скрипты, после соответствующего деплоя):**
   - CE308: `ts:ce308:*`, `ce308:current`, `DROP TABLE IF EXISTS
     sunreceiver.ce308_averages_r CASCADE` (с партициями);
     `ce308-energy:current` не трогать; повторная проверка после рестарта.
   - MAP: `HDEL map:current ""`; удалить 477 пустых `name=''/device_sn=''` строк
     (все метрики NULL); backup/dry-run/идемпотентность.
9. **Финал:** доки/i18n/sample/AGENTS; фильтры ошибок (`antbms`/`enbms` убрать,
   `bms` добавить в `errors.html`/`errors.js`); `go build/vet/test`; проверка на
   секреты.
10. **Ревью раундами** (п.8 глобальных правил); деплой/пуш — только по команде.

Всё — отдельными заявками, с `go build`/`go vet` и проверкой на секреты перед
коммитом.

**Ограничение пользователя (2026-10-17): после реализации деплой и push сразу НЕ
делать** — ждать отдельной команды. Изменения остаются в рабочем дереве.

---

## 9a. Критерии приёмки (общие)

- [ ] `go build ./...`, `go vet ./...`, `go test ./...` — чисто.
- [ ] `git grep -n -E '<id1>|<id2>|…' -- cmd/ Makefile` по удалённым
  идентификаторам (`read_json`, `mapMAPRegisters`, `mapClientFor`, `FetchMPPTs`,
  `FetchMemory`, `pollMAPAPI`, `pollMPPTFromArr`, `bmsSite`, `loadBmsSite`,
  `runBmsPoll`, `enBmsConfigFromSection`, `runEnBmsPoll`, `SaveCE308Current`,
  `CE308Current`, `SaveCE308History`, `apiCE308Current`, `apiCE308Series`,
  `ce308SeriesRange`, `CE308FinalReadingsOnly`, `final_readings_only`,
  `cacheBms26Snapshot`, `loadBms26SerialCache`, `bms26Serials`,
  `redisCE308DataPresent`, `mapgateway`, `bmslistener`) — 0 совпадений.
  Scope: `cmd/` и `Makefile`; `docs/**` исключаются; из `.gitignore` убрать
  устаревшее правило `daemons/mapgateway/review-*`.
- [ ] Проверка удаления путей (не `git grep`): `test ! -e cmd/probe/map`,
  `test ! -e enBms`, `test ! -e daemons/mapgateway`, `test ! -e
  daemons/bmslistener`; `git ls-files | grep -E '^(cmd/probe/map|enBms|daemons/(mapgateway|bmslistener))/'`
  — пусто.
- [ ] `loadConfig` успешно разбирает `sunReceiver.sample.json` (юнит-тест/фикстура,
  без реального запуска: sample содержит `CHANGE_ME_PASSWORD`, порт 80 и внешний
  Redis); старый конфиг даёт понятную ошибку загрузки.
- [ ] MAP/КЭС/BMS26 стартуют после уплощения конфига; fallback покрыт тестами.
- [ ] Дашборд: рамка CE308 «текущие данные»/ряды графиков отсутствуют; рамка
  «показания» и `/api/ce308/energy` работают.
- [ ] После cleanup: `ce308:current`/`ts:ce308:*`/`ce308_averages_r` отсутствуют;
  `ce308-energy:current` на месте; `map:current` содержит только поле `map`;
  пустых строк `name=''/device_sn=''` нет.
- [ ] Строки `map_averages_r` со всеми NULL-метриками не попадают в
  `map:current`/TS/AGM (тест `MapAverages`/реставрации).
- [ ] AGM/BMS-страницы работают на `bms26`; фильтры `antbms`/`enbms` удалены,
  фильтр `bms` доступен.

---

## 10. Результаты детального исследования (2026-10-17)

### 10.1. Критичные перекрёстные зависимости (нельзя удалять «заодно»)

- **`bmsDevice` (тип) и `bmsKey()`** живут в `bms_poller.go`, но используются
  живым BMS26 и дашбордом: `bms26.go:337-338` (`bmsDeviceFromBms26`),
  `dashboard.go:552,578,567`. **Решение 2026-10-17:** вынести в общий файл
  (например, `bms_view.go`); `bms_poller.go` удаляется.
- **`bmsSeriesPoint` / `bmsAveraged`** (`bms_accumulator.go`) — не «старый ANT»,
  а живые типы BMS26/дашборда (`bms26.go:390-410`, `dashboard.go:633-696,2407-2475`).
- **`enbmsExtGroups` / `enbmsExtBit`** (`enBms.go:747-853`) используются
  **живым** `bms26_alarm.go:203` (расшифровка alarm BMS26). **Решение 2026-10-17:**
  вынести в общий файл алармов BMS26; остальной `enBms.go` удаляется.
  `enbmsModeNames`, `enbmsCellStateBits`, `enbmsTempStateBits`,
  `enbmsBalanceMask` — **правило:** удалить, если `git grep` не находит
  использований вне удаляемых файлов; иначе вынести с Ext_Bit.
- **`finalizeBms26`** (`bms26_legacy.go`) вызывает живой `bms26.go:307` и
  использует live-`bms26_alarm.go` — **оставить**.
  `cacheBms26Snapshot`/`loadBms26SerialCache`/`bms26Serials` — legacy-кэш,
  **удалить**; поправить вызовы `bms26.go:308` и `main.go:2331`.
- **`kindMAP` / `kindMPPT` / `devKey` / `mapDevKey`** — живые: `malina26`
  использует синтетическую цель `kindMAP`, КЭС — `kindMPPT`/`mppt-<serial>`.
- **`mpptPollState` / `mpptEmptyTolerance`** — живые: prune КЭС в `malina26`
  (`main.go:2093`). Комментарии про `read_json.php` поправить, код оставить.
- **`requestTimeout`** объявлен в `mppt_api.go:36`, но используется только
  legacy (`bms_poller.go:166` и сам `mppt_api.go`). После удаления ANT и `malina`
  константа и `maxMPPTBody` становятся не нужны — удалить.
- **`internal/modbusmap`** остаётся: `meter_client.go` (DDS238/DTS017M) использует
  `ReadFull`. После удаления MAP-Modbus `Client`, `DefaultPort`, `ReadRegisters`
  останутся **без production-вызовов** (в тестах пакета используются). Пакет не
  удалять. EnBMS RS485 использует собственный PACE ASCII, не `modbusmap`.
- **BLE-хелперы CE308** (`ce308_agent_*`, `ce308_client.go`) нужны final-режиму
  CE308. Сегодня их переиспользует EnBMS (`enBms_client.go:88-108`,
  `enBms_poller.go:209-210`); после удаления EnBMS зависимости исчезают.

### 10.2. MAP (`rs485` / `malina` → только `malina26`)

Точки удаления (подтверждено):
- `main.go:158-164` `mapRS485Section`; `:166-175` `mapMalinaSection`;
  `:225-231` поля `Method`/`RS485`/`Malina`; `:448-475` ветки `rs485`/`malina`.
- `main.go:34` импорт `modbusmap` (в `main.go` нужен только MAP-Modbus).
- `main.go:1444-1461` `mapClients*`/`mapClientFor`; `:1463-1583`
  `mapMAPRegisters`; `:1779-1826` `pollDevice case kindMAP`; `:1849-1882`
  `pollMPPTFromArr`; `:2006-2027` цикл `targets kindMAP` + `mapAPI`;
  `:2037-2070` ветка `mppt != nil`; `:2141-2144` `loadMPPTSite`;
  `:2160-2164` `log.Fatalf`; `:2355,2379` условия; `:488` `mapAPI`.
- `map_errors.go:144-158` (Modbus) и `:163-175` (`read_memory`) — удалить;
  `:159-162` (ветка `malina26.mapErrCells`) — **оставить**;
  `runMapErrorPoll` теряет параметр `*invTarget` (всегда nil после удаления).
- `mppt_api.go` целиком; `mppt_api_test.go` целиком.
- Упоминания в комментариях (`main.go:64-68,77,121-123,167,219-221,643-656,
  687-739,1934-2040`; `map_errors.go:26-35`; `notify.go:99`;
  `dts017_config.go:59`).

Остаётся (живое):
- `malina26.go` — сохраняется, но правится (fallback, удаление проверки
  `Method`, запуск BMS26); `map_store.go`, `map_pg.go`,
  `kes_store.go`, `kes_pg.go`, `avg_r.go` (spec `map_averages_r`/`kes_averages_r`);
  `saveWindowSnapshot`, `mapErrState`, `mapDevKey`, `kindMAP`/`kindMPPT`.
- `mapSourceIdentity`/`mapSourceName`: убрать ветки `mapAPI` и Modbus-целей;
  источник определять по `malina26 != nil` (синтетическая цель `kindMAP` от
  `malina26` остаётся).

Идентичность МАП (подтверждено):
- Ключи: `sunreceiver:map:current` (HASH, поле `map`), `sunreceiver:ts:map:map:<tag>`,
  PG `sunreceiver.map_averages_r` (PK `name`, meta `device_sn`).
- `DeviceSN`/`device_sn` берётся из настроенного `serial` (`malina26.go:277`,
  `map_store.go:57`, `map_pg.go:31-32`). Ключи не зависят от прибора.
- `map_averages_r.device_sn` уже приводился к реальному `<PRIMARY_SERIAL>` отдельной
  миграцией (`docs/requirements/db-rudiment-cleanup.md`).

Fallback attach points:
- `malina26.go:81-112` `loadMalina26` — **убрать проверку `sec.Method`**
  (`:85-87`); MAP/КЭС/BMS26 включаются при `disabled=false` и валидных
  `host/password/serial`; добавить чтение/валидацию `fallback_serial`.
- `malina26.go:59-74` состояние `malina26Source` (`mu`, `haveMap`, `mapID`,
  `mapGen`) — добавить `activeSerial`/`activeFallback`, `lastListCheck`,
  `primaryBrokenUntil`.
- `malina26.go:293-317` `fetchMapRaw` — выбор: сначала `serial` (если не помечен
  broken), затем `fallback_serial`; при активном fallback периодически
  перечитывать `/api/map/devices` (60 с) и возвращаться на основной.
- **Primary raw unhealthy:** состояние — `primaryFailStreak` (счётчик подряд),
  `primaryBrokenUntil`. 2 подряд неуспеха raw основного (404/HTTP-ошибка или
  ошибка декодирования) → `primaryBrokenUntil = now+60s`, переключение на
  fallback. Пока метка активна, primary raw не пробуем (только список); по
  истечении — одна проба primary raw; успех → сброс счётчика/метки и возврат,
  неуспех → снова +60 с + счётчик.
- **Где считать decode-ошибки:** `fetchMapRaw` (`:293-317`) делает только HTTP и
  возвращает тело; декодирование — в `fetchMAP` (`:337-355`) и `mapErrCells`
  (`:359-378`). Счётчик/метку обновлять в общем decode-хелпере (или передавать
  результат decode в `fetchMapRaw`-состояние), а не внутри HTTP-функции.
- **404 активного устройства:** если 404 пришёл для fallback — сбросить
  fallback-состояние/`haveMap` и заново разрешить устройство; **не** сбрасывать
  `primaryBrokenUntil`. 404 основного — инкремент `primaryFailStreak`, метку не
  снимать.
- `malina26.go:275-280` — `res.DeviceSN` уже равен настроенному `serial`;
  менять на фактический нельзя. Фактический серийник активного прибора
  используется **только** для выбора в `/api/map/devices` и диагностического лога;
  в Redis/TS/PG/API он **не пишется** (решение 2026-10-17).
- `mapErrCells` (`malina26.go:359-378`) ходит через тот же `fetchMapRaw` —
  ошибки МАП автоматически пойдут по резервному прибору.
- Конкурентность: `fetchMapRaw` вызывают и основной цикл, и error-цикл —
  состояние под общим `mu`; иначе гонка/лишние запросы.
- Контракт: предполагается тот же MapData v5/402; иная версия будет отвергнута.

Риск (существующий): `mapTracker.trackAPITS` учитывается только для
`source == "api"` (`notify.go:247`), а `malina26` передаёт `"malina26"` — stale-
детект по timestamp для него фактически не работает. **Решение (2026-10-17):
починить заодно** — в `mapTrack.status` учитывать источники, дающие timestamp
API (`api` и `malina26`), обновив комментарии/тип `source`.

### 10.3. ANT BMS (legacy `read_bms.php`)

Удаление:
- `main.go:233-241` `antBmsSection`, `:263` `configFile.AntBms`, `:386-394`
  загрузка, `:735-737` `antBmsSec`, `:2153-2156` `loadBmsSite`,
  `:2403-2413` `runBmsPoll`, `:2200` `ShowBMS`.
- `poll.antbms`: `main.go:250,295,306`, `:45` (`bmsPollInterval`).
- `bms_poller.go`: `bmsApiClient`/`bmsSite`/`loadBmsSite`/`fetch`,
  `runBmsPoll`, `pollAndSaveBMS`, `saveBMSReading`, `bmsCollection`,
  `resolveBMSKey`, `decodeAntAlarms`, `recomputeMinMaxCells` — удалить.
  **Оставить/вынести:** `bmsDevice`, `bmsKey` (+ тест `TestBmsKey` в
  `bms_view_test.go`).
- `redis_store.go:159-190` `SetBMS` (ANT-only) — удалить после удаления пулера.
- `daemons/bmslistener/` (+ `web/read_bms.php`, systemd, Makefile), цель
  `Makefile` `bmslistener`, `make all`.
- `bms26_legacy.go` ANT-часть — удалить по правилу `git grep`: `antNameCap`,
  `antCapFromName`, `capFromRe`, `legacyAntSerial`, `legacyModeFromCurrent`,
  `antDeviceToBms26`, `itoa`, `antAlarmBytesFromCodes`; `setLegacyAlarmNibble`
  удалить, если после удаления EnBMS вне удаляемых файлов не используется.
- Доки: `docs/antbms.md`, `docs/antbms-worklog.md`,
  `docs/modules/bms-listener.md`, `docs/antbms/`; упоминания в AGENTS.md/README.

Хранение: ANT пишет только в unified `bms26` через шим — отдельных ключей/таблиц
нет. `bms26` не трогать.

### 10.4. EnBMS (прямой BLE/RS485)

Удаление:
- `main.go:262` `configFile.EnBms`, `:414-420` загрузка, `:2125,2128,2179-2184`
  `enBmsCfg`/лог, `:2494-2533` запуск и BLE-лимит. **Сигнатура `loadConfig`
  теряет `*enBmsConfig`** (`main.go:321,527`): обновить возврат, вызов
  `main.go:2128` и все 11 вызовов в `main_config_test.go`
  (`:38,48,56,117,133,140,157,165,177,211,226`).
- `enBms.go` (парсеры/протокол), `enBms_client.go`, `enBms_rs485.go`,
  `enBms_poller.go`, `enBms_store.go`.
- `enbmsMaxBTDevices` (`enBms.go:161-166`) и проверка лимита в `main.go`.
- `bms26_legacy.go` EnBMS-часть: `enbmsSnapshotToBms26`, `legacyEnbmsSerial`,
  `enbmsAlarmBytesFromStrings`, `legacyModeFromText`, `enbmsBalanceMask` —
  удалить по правилу `git grep` (если вне удаляемых файлов нет использований).
- **Оставить/вынести:** `enbmsExtGroups` + `enbmsExtBit` (живой
  `bms26_alarm.go`; покрытие — `bms26_alarm_test.go:33-44`, остаётся).
  `setLegacyAlarmBit`/`enbmsPackAlarm` — удалить, если `git grep` не находит
  использований вне удаляемых файлов.
- Тесты: `enBms_test.go`, `enBms_state_test.go` — удалить; в
  `bms26_legacy_test.go` убрать `TestEnbmsPackAlarmRoundtrip`, оставив проверку
  `finalizeBms26` на заранее сформированных `AlarmBytes` (или удалить тест
  целиком, если проверка покрыта `bms26_alarm_test.go`).
- Доки: `docs/modules/enbms.md`, tracked `enBms/`, упоминания в AGENTS.md/README.

BLE-хелперы CE308 не удалять (нужны final-режиму). Отдельных EnBMS-ключей/таблиц
нет; всё в `bms26`.

### 10.5. CE308 (обычный режим → только `final_readings_only`)

Обычный режим (удалить):
- `ce308_poller.go`: `runCe308Poll` (`:78-146`), `ce308PollConnected`
  (`:258-320`), `ce308PollOnce` (`:343-367`). Final: `runCe308FinalReadings`
  (`:163-245`) — оставить.
- `ce308_store.go`: `SaveCE308Current` (`:47-53`), `CE308Current` (`:79-93`),
  `SaveCE308History` (`:58-67`, пишет `ts:ce308:*`), `QueryCE308Series`
  (`:113-127`). Оставить `SaveCE308Energy`/`CE308Energy`.
- `ce308_accumulator.go` целиком (`runCe308Accumulator`, `backfillCe308`,
  `averageCe308`), PG-писатель `InsertCe308Average`, ридеры
  `QueryCE308Averages`/`CE308AveragesAll`, spec `ce308_averages_r` в `avg_r.go`;
  CE308 из `avgMigMaps` (`migrate_avg.go`) и `TABLES`
  (`scripts/step1_averages_partitioned.py`), ветка `scripts/migrate_zset_to_rt.py`;
  nil-guard в `migrateAvgScalar`.
- `main.go`: запуск обычного цикла + аккумулятора, восстановление CE308-ряда
  (`restoreCE308Series`, `:2312,3042-3080`), `CE308` в `IsEmpty` и
  `redisCE308DataPresent` + их вызов (`:2289-2300`); флаг
  `CE308FinalReadingsOnly` (`:2203`, `:2471`; `dashboard.go:88-91,1358,1952,
  3459,3528,3616`) — выбрать безусловный final.
- `dashboard.go`: `apiCE308Current`, `apiCE308Series`, `ce308SeriesRange`,
  `ce308SeriesPoint`, поля `CE308L1/L2/L3Voltage`/`CE308ActivePower`; downsample
  только `CE308L1Voltage`/`CE308ActivePower`; резерв мощности гаража
  `freshCE308Power`/`ce308Name`/параметр `ce308` в `buildAnimationResponse`.
- `ce308.go`: теги `ce308_*`, `ce308CurrentTags` (мёртвый), `buildCE308Reads`,
  `toWatts`, `ce308ReadsValid`, `valuesCE308`, `ce308Snapshot`; `ce308Floats`
  в `ce308_client.go`.
- `poll.ce308` (`main.go:249,294-299,305`; `:44`) — final его не использует.
- Web: блок `ce308Cur` в `index.html:73-94`, `renderCE308Current` +
  `/api/ce308/current` в `dashboard.js`, CE308-ряды в `charts.js:11-34,465-484,
  517-533`, атрибут `data-ce308-final` (согласованно с JS; `data-show-ce308`
  **остаётся**), CSS `.ce308-scroll/.ce308-table/.ce-num/.ce-pos/.ce-neg`.
- Тесты: `ce308_test.go:27-56,138-179` (normal), `ce308_probe_test.go`,
  `dashboard_animation_test.go` — править **весь файл** (сигнатура
  `buildAnimationResponse` во всех вызовах: `:50,83,148,159,168,179,196,223,245,
  257,330` + удалить CE308-кейсы); `ce308-probe.sh` (tracked).
  **Решение 2026-10-17: `ce308-probe.sh` и `ce308_probe_test.go` удалить.**

Оставить (final):
- `runCe308FinalReadings`, `ce308EnergyDelay`, `ce308CaptureEnergy` +
  метод `SaveCE308Energy`, `CE308Energy`,
  `ce308EnergySnapshot`/`readCE308Energy`/`parseCE308End`,
  `ce308Config`/`ce308ConfigFromSection`/`describeCE308Config`,
  `ce308_client.go` (BLE), `ce308_agent_*` (PIN/адаптер), ошибки
  `device_errors kind=ce308` (пишутся и в final).
- Web: рамка «показания» (`index.html:108-127`), `renderCE308Energy`,
  кнопка/POST `/api/ce308/energy`, `data-show-ce308`.
- `sunreceiver:ce308-energy:current`.

Данные (cleanup):
- Удалить `sunreceiver:ce308:current`, все `sunreceiver:ts:ce308:*`
  (**retention 60 суток, `PurgeOld` — no-op**, поэтому удалять префиксом, не ждать),
  PG `sunreceiver.ce308_averages_r` **вместе с партициями** `*_pYYYYMM`.
- Legacy-таблица `sunreceiver.ce308_averages` **отсутствует** (проверено);
  `migrate_avg.go` её только читает (не создаёт), `scripts/step1...` создаёт
  `ce308_averages_r`. В скрипте всё равно проверить обе и старый ключ
  `sunreceiver:ce308:energy` (не путать с `ce308-energy:current`).
- **Не трогать** `sunreceiver:ce308-energy:current` и `device_errors`.
- Обязательно: backup/дамп перед удалением, dry-run со счётчиками (ожидается
  `ce308_averages_r`: 49 840 строк; `ts:ce308:*`: 0 сейчас), идемпотентность,
  проверка после; `DROP TABLE IF EXISTS`; Redis — точные имена/`SCAN` по
  конкретному префиксу.
- Важно: пока stale-ключи `ce308:current`/`ts:ce308:*` существуют, `IsEmpty`
  (`redis_store.go:224`) и `redisCE308DataPresent` (`main.go:2852-2866`) считают
  Redis непустым и могут подавлять реставрацию остальных рядов из PG. После
  удаления CE308 из этих проверок они перестают на них реагировать.

### 10.6. Конфиг

- Текущие обязательные/используемые: `map.disabled`; `map.method`;
  `map.malina26.{host,password,serial}` (port опционален); `poll.*` (включая
  `antbms`, `ce308`); `antBms.disabled/url`; `enBms.disabled` + интервалы/devices;
  `ce308.disabled/final_readings_only/mac/pin`.
- `ce308.final_readings_only` обязателен **даже при `disabled=true`**
  (`main.go:404-406`); `poll.ce308` обязателен (`main.go:294-299`). **Решение
  2026-10-17: удалить оба поля жёстко**, конфиг мигрировать вместе с деплоем
  (старый вид → ошибка загрузки).
- После уплощения MAP: `map.{disabled,host,port,password,serial,fallback_serial}`
  (без `method`/`malina26`). **Решение 2026-10-17: старый вид не поддерживать** —
  ошибка загрузки, конфиг мигрировать с деплоем.
- **Явная проверка legacy-полей** (иначе `json.Unmarshal` молча их проглотит, и
  «ошибка загрузки» не наступит): `map.method`, `map.malina26`, `map.rs485`,
  `map.malina`, `ce308.final_readings_only`, `poll.ce308`, `poll.antbms`,
  `antBms`, `enBms`. Реализовать как валидацию после разбора (или
  `DisallowUnknownFields`) с понятным сообщением об ошибке.
- **Миграция конфига — две фазы.**
  - *Подготовка (можно сейчас):* backup прод- и локального `sunReceiver.json`
    в `.kilo/backups/` (gitignored, вне репозитория); преобразовать локальный +
    `sunReceiver.sample.json`; проверить `jq`. Прод-конфиг **не трогать**, пока
    деплой не разрешён (временное расхождение локального/продового конфига явно
    допускается).
  - *Окно деплоя (по команде):* stop сервиса → backup прод-конфига в
    `.kilo/backups/` → сохранить текущий бинарник как
    `/opt/sunreceiver/sunReceiver.bak-<ts>` (иначе `scp` затрёт его) → заменить
    прод-конфиг на новый вид → установить новый бинарник → start → health-check.
  - *Rollback:* stop → восстановить старый `sunReceiver.json` из backup →
    вернуть `sunReceiver.bak-<ts>` → start → проверить. Пара «бинарник+конфиг»
    откатывается вместе.

### 10.7. Инфраструктура, доки, тесты

- Makefile: цели/артефакты `bmslistener` (`:11,19,21,42-50,80-89`) и
  `mapgateway` (`:12,52-59,91-100`), `all` (`:21`).
- `rts_store.go:34` — устаревший комментарий «Retention 2 суток»: обновить на
  60 суток (фактическая константа `:39`).
- `daemons/mapgateway/`, `daemons/bmslistener/` — удалить целиком.
- `tools/` — **весь каталог в `.gitignore`**, в git не входит; к runtime не
  подключён (bat_stat_scripts/seplos ссылаются на старые ключи, ce308stat/map-loader
  исторические). **Решение 2026-10-17: оставить локально** (вне git-объёма).
- `ce308-probe.sh` — tracked, завязан на normal-телеметрию; **удалить** вместе
  с `ce308_probe_test.go`.
- `cmd/probe/map/main.go` (tracked) — проба МАП по Modbus, рудимент `rs485`:
  **удалить**. `cmd/probe/main.go` (Solarman) — отдельная проба, **оставить**.
- `enBms/` (tracked: `context.md`, `enBms-monitoring.md`) — **удалить** вместе с
  EnBMS.
- CE308-ассеты (`assets/animation/devices/ce308-meter.*`, копия в
  `web/static/img/animation/`) — **оставить**: спрайт узла «гараж» используется
  и при мощности из DTS017M.
- Доки к удалению/обновлению: `docs/read_json.md`, `docs/malina-web-api.md`,
  `docs/malina-bug_Inet_flag.md`, `docs/antbms.md`, `docs/antbms-worklog.md`,
  `docs/antbms/`, `docs/modules/bms-listener.md`, `docs/modules/enbms.md`,
  `docs/research/map-signs-audit.md`; обновить `docs/README.md`,
  `docs/modules/{README,map-mppt,notify,storage,dashboard,inverter-poller,bms26,
  ce308}.md`, `docs/dts017m-meter.md`, `docs/universal-contract.md`,
  `docs/research/README.md`, `docs/research/redis-storage-optimization.md`,
  `AGENTS.md` (в т.ч. убрать требование `zig` для целей `bmslistener`/`mapgateway`).
- Тесты к удалению/правке: `mppt_api_test.go`, `bms_poller_test.go` (legacy-часть),
  `enBms_test.go`, `enBms_state_test.go`, `bms26_legacy_test.go` (legacy-часть),
  часть `ce308_test.go`, `ce308_probe_test.go`, `dashboard_animation_test.go`,
  `main_config_test.go` (поля poll/ce308).
- i18n (`i18n.js`): удалить CE308-«текущие данные»/суффиксы графиков.
  **AGM/tooltip-подписи «ANT×2 + EnBMS» оставить** — состав BMS26 не менялся.
- Фильтры ошибок (`web/templates/errors.html`, `web/static/js/errors.js`,
  `KIND_LABEL`): удалить `antbms`/`enbms`, добавить `bms` (живой kind). Это не
  `i18n.js`.

### 10.8. Аудит данных (подтверждён)

- Redis DB0…DB15: ключей `*bms*`/`*enbms*`/`*ant*` вне `bms26` нет;
  `sunreceiver:bms`, `sunreceiver:bms:current` не существуют.
- PG: таблиц `bms`/`ant`/`enbms` вне `bms26_*` нет; `bms_averages_r` отсутствует.
- **Писатель `bms26`** — unified `malina26`. Фактическое состояние (локальный
  `sunReceiver.json`, синхронизированный с продом): `antBms.disabled=true`;
  `enBms.disabled=true` на уровне **раздела**, при этом
  `devices[0].disabled=false`/`method=rs485` (потому что раздел выключен,
  EnBMS-пулер не запускается). `AGENTS.md` («опрос EnBMS включён») устарел —
  обновить (см. §11 п.14). Перед удалением пулеров всё равно подтвердить по
  **каждому** из трёх серийников свежесть последней точки (`TS.GET`/last sample)
  и отсутствие новых записей от legacy-пулеров в логах; «свежий
  `bms26:current`» сам по себе не доказывает, кто писал.
- Retention RedisTimeSeries — **60 суток** (`rts_store.go:39`), а не 2. Наблюдаемое
  окно данных `bms26` ~2 суток объясняется отсутствием более старых точек (Redis
  сброшен/данные начались позже), а не retention.
- `map_averages_r`: 563 738 строк `device_sn='<PRIMARY_SERIAL>'` (name
  `MAP (батарея/сеть)`) и 477 строк с `name=''`/`device_sn=''`, у которых **все
  метрики NULL** и `ts` совпадает с живыми 5-минутными точками → это пустые
  заглушки, а не дубли данных. Удаление безопасно (с backup/dry-run).
- Остаётся живой `bms26` — не трогаем.
- Задачей ничего не удалялось (только чтение).

---

### 10.9. Фронтенд: что проверено

Проведена сплошная статическая проверка `cmd/sunreceiver/web/` (templates/js/css/i18n):
grep по эндпоинтам и терминам удаляемых фич.

- **MAP legacy:** ссылок на `read_json`/`rs485`/`malina` во фронте **нет**.
  Все «MPPT» в web (`dashboard.js`, `i18n.js`) — это КЭС (`mppt-<serial>`),
  остаются.
- **ANT/EnBMS:** отдельных панелей нет — `bms.html`/`agm.html`/`/api/bms*`
  показывают единый BMS26 и остаются. Во фронте только подписи/упоминания:
  AGM-текст «ANT×2 + EnBMS» (`agm.js:304`, `agm.html:17`, `i18n.js:78,133,142,149`),
  комментарии `bms.js`, фильтры ошибок `antbms`/`enbms`
  (`errors.html:28-29`, `errors.js:10`).
- **CE308:** 152 упоминания в 9 файлах. Normal-only артефакты:
  `templates/index.html:73-94` (блок `ce308Cur`), `dashboard.js:413-446`
  (`renderCE308Current`), `dashboard.js:481-484` (опрос `/api/ce308/current`),
  `charts.js:11-34,465-484,517-533` (ряды/заголовки), CSS
  `dashboard.css:83-90` (`.ce308-scroll/.ce308-table/.ce-num/.ce-pos/.ce-neg`),
  `animation.js` (спрайт/узел гаража — остаётся, DTS017M; резерв мощности
  убирается на бэке). Показания (`renderCE308Energy`, кнопка, `/api/ce308/energy`)
  остаются.
- **Важно:** JS **не вызывает** `/api/ce308/series` — графики берут CE308 из
  общего `/api/series`. Значит удаление `apiCE308Series` — только бэкенд.
- **Связанное удаление:** флаг `data-ce308-final` читают `dashboard.js:11` и
  `charts.js:15`. Если убрать атрибут из шаблона, но оставить JS, `ce308Final`
  станет `false` и фронт начнёт каждую секунду дергать удалённый
  `/api/ce308/current` (404). Убирать атрибут и JS-ветки нужно согласованно.
- **i18n:** при правках дашборда обязательны ru/en/zh (`i18n.js`); CE308-ключи
  «текущие данные»/суффиксы графиков — удалить, «показания» — оставить.

**Не проверялось (вживую):** рендеринг в браузере, ответы API, мобильная
вёрстка, переключение языков. Приложение не запускалось (локальный конфиг
указывает на прод-БД; запуск — только с изолированным хранилищем/по согласованию).

### 10.10. Ключи `*:current` — что реально осталось

Общий `sunreceiver:current` **действительно выпилен** (ключа нет). Но `current`
как механизм остался **по модулям**:
`sunreceiver:{deye,sofar,dds238,map,kes}:current` читаются в `allCurrent`
(`dashboard.go:1061-1086`); `dts017m:current`, `bms26:current`,
`ce308-energy:current` (и `ce308:current` — обычный режим, удаляется) читаются
своими хендлерами. Все значимы.

`sunreceiver:map:current` (аудит 2026-10-17):
- поле `map` — **живое и значимое**: снимок с 10 тегами, `device_sn=<PRIMARY_SERIAL>`,
  пишется каждую секунду `saveMapSnapshot`, читается `MapCurrent` →
  `/api/current` и `/api/agm`. Оставлять.
- поле `""` — **рудимент**: снимок от 2026-10-16 09:35, никем не читается
  (`MapCurrent` читает поле `map`), реставрация из PG его не создаёт
  (для МАП вызывается `saveMapSnapshot`). Кандидат на `HDEL`. Причина появления
  (регрессия старого бинарника/иного писателя) не установлена.
  **Проверка перед/после очистки:** зафиксировать версию/хеш прод-бинарника;
  после рестарта контролировать `HGETALL sunreceiver:map:current` в течение
  окна (например, 10 мин) — поле `""` не должно появляться; иначе старый
  писатель ещё работает и `HDEL` бессмыслен.

PG `sunreceiver.map_averages_r` (аудит 2026-10-17):
- `device_sn='<PRIMARY_SERIAL>'`, name `MAP (батарея/сеть)` — 563 738 строк
  (2020-12-31 … сейчас), живые.
- `device_sn=''`, name `''` — 477 строк (2026-10-15 … 2026-10-16 15:40); у всех
  **метрики NULL**, `ts` совпадает с живыми 5-минутными точками (PK `(name,ts)`
  различает их по пустому `name`) → это пустые заглушки, не дубли данных.

**Решение (2026-10-17): удалить только рудимент** (после ревью уточнено —
удаляются пустые заглушки, не «дубли»).
- `HDEL sunreceiver:map:current ""` — поле `map` и сам ключ остаются.
- `DELETE FROM sunreceiver.map_averages_r WHERE device_sn='' AND name='' AND
  <все метрики NULL>` — 477 пустых строк.
- Перед удалением: backup/дамп, dry-run с ожидаемым счётчиком 477, проверка
  отсутствия не-NULL метрик; идемпотентность.
- Выполнять отдельным Python-скриптом с dev-ПК (правило проекта), не в бинарнике.
- `saveMapSnapshot` уже пишет только поле `map` — менять не нужно.
- **Отдельное изменение кода (устойчивость):** в `MapAverages`/реставрации
  пропускать строки, у которых **все метрики NULL** (не фильтровать по
  `device_sn<>''`, чтобы не отбросить валидную историю с пустым `device_sn`).
  Это внести в §9 (до деплоя) и покрыть тестом. Перед реализацией проверить
  (ожидается 0):
  ```sql
  SELECT count(*) FROM sunreceiver.map_averages_r
  WHERE device_sn = ''
    AND (battery_current IS NOT NULL OR battery_power IS NOT NULL
      OR battery_voltage IS NOT NULL OR grid_frequency IS NOT NULL
      OR grid_power IS NOT NULL OR grid_voltage IS NOT NULL
      OR l1_voltage IS NOT NULL);
  ```

---

## 11. Вопросы

### Закрыты (решения 2026-10-17)

1. MAP fallback: перепроверка основного — **раз в 60 с**; при 2 подряд
   404/битом raw основного — переключение на резервный.
2. Фактический серийник — **не хранить в телеметрии/ключах/PG/API** (только
   выбор устройства; допускается диагностический лог).
3. Stale-детект `mapTracker` для `malina26` — **починить заодно**.
4. Legacy-доки MAP — **удалить**.
5. Фильтры ошибок `antbms`/`enbms` — **удалить**; добавить фильтр живого
   `kind=bms`.
6. CE308-конфиг (`poll.ce308`, `final_readings_only`) — **убрать жёстко**.
7. CE308 cleanup — отдельным Python-скриптом; выполнять **после** деплоя кода
   без CE308-спеки (иначе старый бинарник пересоздаст таблицу), затем повторная
   проверка.
8. MAP-конфиг — **только новый плоский вид**; legacy-поля явно отклонять.
9. `ce308-probe.sh` + `ce308_probe_test.go`, `cmd/probe/map` — **удалить**.
10. Общий код (`bmsDevice`/`bmsKey`, `enbmsExtGroups`/`enbmsExtBit`) —
    **вынести в общие файлы** (`bms_view.go`, файл алармов BMS26) как чистый
    рефакторинг.
11. `map:current` — ключ и поле `map` **живые**; рудимент — только поле `""`.
    PG: 477 пустых строк (`name=''/device_sn=''`, метрики NULL) — **удалить**.
12. `tools/` — **оставить локально** (в git не входит); `enBms/` (tracked) —
    **удалить**; CE308-ассеты — **оставить**.
13. Поле `map:current ""`: причина появления не установлена; `HDEL` — лечение
    симптома. Критерий: после рестарта нового бинарника поле не появляется в
    контрольном окне (≥10 мин); иначе старый писатель ещё работает.
14. `AGENTS.md` противоречит аудиту по EnBMS (там «опрос включён»): при
    обновлении явно зафиксировать фактическое `enBms.disabled=true` (section-level)
    и уточнить статус устройств.
