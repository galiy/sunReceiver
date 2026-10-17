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
(`method=malina26`, токен по паролю + бинарные контракты). Остальные методы
выпиливаем полностью.

**Оставляем:**
- `method=malina26`: МАП (`/api/map/raw`) и КЭС (`/api/kes/raw`), блок конфига
  `map.malina26` (`host`, `port`, `password`, `serial`);
- хранилище МАП/КЭС (ключи `sunreceiver:map:*`, `sunreceiver:kes:*`, `sunreceiver:ts:map:*`,
  `sunreceiver:ts:kes:*`, PG `map_averages_r`, КЭС-таблицы) — оно общее для `malina26`.

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
(DDS238/DTS017M/EnBMS RS485), это не рудимент МАП.

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
  тест `bmsKey` оставить (функция живёт).

**Не трогаем:**
- BMS26 через `malina26` (`bms26.go`, `bms26_store.go`) — единый тип ANT+EnBMS,
  ключи `sunreceiver:bms26:*`, PG `bms26_averages_r`;
- `bmsSeriesPoint`/`bmsAveraged` (`bms_accumulator.go`) — используются живым BMS26
  и дашбордом;
- общие функции `bms26_legacy.go`, нужные живому BMS26: `finalizeBms26`,
  `cacheBms26Snapshot` (вызов из `bms26.go`; сам кэш serial — legacy);
- EnBMS-часть `bms26_legacy.go` — удаляется вместе с EnBMS (раздел 3).

**Факт по накопленным данным (read-only проверка прод-БД, 2026-10-17):**

- Полный аудит: Redis **DB0…DB15** — ключей `*bms*`/`*enbms*`/`*ant*` вне `bms26`
  нет; `sunreceiver:bms` и `sunreceiver:bms:current` **не существуют**.
  В PG таблиц с `bms`/`ant`/`enbms` вне `bms26_*` нет; `bms_averages_r` отсутствует.
- Есть объединённые ключи BMS26:
  - `sunreceiver:bms26:current` — 3 устройства:
    `<ANT_SERIAL_1>`, `<ANT_SERIAL_2>`,
    `<ENBMS_SERIAL>`; снимок свежий (2026-10-17).
  - `sunreceiver:ts:bms26:*` — 195 серий (65 на устройство), ~8.2 млн сэмплов на
    устройство, окно ~2 суток (retention RedisTimeSeries).
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
  `enbmsModeNames`/`enbmsCellStateBits`/`enbmsTempStateBits`/`enbmsBalanceMask`
  после удаления шимов проверить (вероятно, legacy-only);
- EnBMS-часть «костыля» `bms26_legacy.go` (`enbmsSnapshotToBms26`,
  `legacyEnbmsSerial`, `enbmsAlarmBytesFromStrings`, `legacyModeFromText`);
- тесты `enBms_test.go`, `enBms_state_test.go` (после извлечения Ext_Bit —
  соответствующие проверки перенести), упоминания в доках.

**Не трогаем:**
- BMS26 через `malina26` (`bms26.go`, `bms26_store.go`) — единый тип ANT+EnBMS,
  ключи `sunreceiver:bms26:*`, PG `bms26_averages_r`;
- BLE-PIN-агент и BLE-транспорт CE308 (`ce308_agent_*`, `ce308_client.go`) —
  CE308 final-режим от них зависит.

**Факт по накопленным данным (read-only проверка прод-БД, 2026-10-17):**

- Redis-ключей `sunreceiver:enbms*` (и `*enbms*`) **нет**.
- EnBMS-устройство (`<ENBMS_SERIAL>`) присутствует в
  общих `sunreceiver:bms26:current` / `sunreceiver:ts:bms26:*` (65 серий,
  ~8.2 млн сэмплов за ~2 суток retention) и живо пишется через `malina26`.
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
  - серий `sunreceiver:ts:ce308:*` **нет**.
- PG:
  - `ce308_averages_r` — ~49 840 строк, 2026-09-23 … 2026-10-05 (устаревшие
    средние обычного режима);
  - таблиц `ce308_daily_tariffs` / `ce308_energy` нет.

**Вывод:** живые данные `final_readings_only` — только
`sunreceiver:ce308-energy:current`; их удалять нельзя. Артефакты обычного режима
(`ce308:current` HASH и PG `ce308_averages_r`) после удаления режима становятся
осиротевшими.

**Решение (2026-10-17): очистить, ключи/таблицы удалить.**
- Удалить ключ телеметрии `sunreceiver:ce308:current` (HASH, устарел 2026-10-05).
- Удалить ряды `sunreceiver:ts:ce308:*`, если/когда появятся (сейчас их нет).
- Удалить таблицу PG `ce308_averages_r` (~49 840 строк, 2026-09-23…10-05) —
  писатель удаляется вместе с обычным режимом.
- **Оставить** `sunreceiver:ce308-energy:current` — это данные режима
  `final_readings_only` (живые итоговые показания, панель «показания»).
- Выполнять отдельным Python-скриптом с dev-ПК напрямую в БД (правило проекта),
  не в бинарнике. **Решение 2026-10-17: выполнить сразу** — на проде
  `final_readings_only=true`, обычный писатель/аккумулятор не работает.

---

## 5. Инвентаризация удаляемого (backend + web + инфраструктура)

Ниже — полный инвентарь по каждому пункту. Пометки: **У** — удалить,
**О** — оставить, **?** — требует решения/согласования.

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
- **О** `method=malina26`, `malina26.go`, `pollMAPMalina26`, `FetchKES`,
  `mapErrCells`; `internal/modbusmap` (счётчики).

Инфраструктура:
- **У** `daemons/mapgateway/`, цель `mapgateway` в `Makefile`.
- **У** legacy-доки: `docs/read_json.md`, `docs/malina-web-api.md`,
  `docs/malina-bug_Inet_flag.md`, `docs/research/map-signs-audit.md`
  (решение 2026-10-17 — удалить).

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
  дашбордом — вынести в общий файл.
- **У** ANT-часть `bms26_legacy.go`: `antDeviceToBms26`, `antCapFromName`,
  `legacyAntSerial`, `antAlarmBytesFromCodes`, `antNameCap`.
  **О** в этом же файле общее для живого BMS26: `cacheBms26Snapshot`,
  `loadBms26SerialCache`, `finalizeBms26`, `capFromRe` (если нужен EnBMS-шим и
  удаляется — сверить).
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
  `legacyEnbmsSerial`, `enbmsAlarmBytesFromStrings`, `legacyModeFromText`,
  вспомогательные map-таблицы, если больше не используются.
  **О** `cacheBms26Snapshot`/`loadBms26SerialCache`/`finalizeBms26` (живой BMS26).
- **У** раздел `enBms` из `sunReceiver.json` / sample.

Доки:
- **У** `docs/modules/enbms.md`.

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
  поля `CE308L1/L2/L3Voltage`, `CE308ActivePower` в ответе графиков и их
  downsample; **О** `apiCE308Energy`.
- **У** в `pg_store.go`: `InsertCe308Average`, `QueryCE308Averages`,
  `CE308AveragesAll`; spec `ce308_averages_r` из `avg_r.go`.
- **У** в `main.go`: восстановление CE308-ряда (`restoreCE308Series`,
  `CE308AveragesAll` → `SaveCE308History`/`SaveCE308Current`), упоминания
  `redisCE308CurrentKey` в `IsEmpty`/`redisCE308DataPresent`.
- **У** резерв CE308 для мощности «гаража» в анимации: `freshCE308Power`,
  `ce308Name`, параметр `ce308` в `buildAnimationResponse` / `apiAnimation`
  (основной источник — DTS017M).
- **О** `ce308_client.go`, `ce308_agent_*` (BLE-транспорт и PIN нужны final-режиму),
  `ce308.go` (маппинг энергии).
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
- **У/обновить** `docs/modules/ce308.md` (убрать обычный режим), `AGENTS.md`,
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
- **Проверить на этапе реализации:** grep по конфиг-структурам на прочие поля,
  ставшие неиспользуемыми (например, `mapRS485Section.Unit`, `mppt_path` и т.п.).

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
- [ ] CE308: удалить обычный режим + `ce308_averages_r`-писатель; поля
      `final_readings_only` и `poll.ce308` — **решено убрать жёстко** (конфиг
      мигрируется с деплоем).
- [ ] `tools/*` (bat_stat_scripts, seplos, ce308stat) — в объём не входят; подтвердить.
- [ ] Конфиг: MAP уплостить (**решено: только новый вид**, без `method`/`malina26`);
      убрать `antBms`, `enBms`, `poll.antbms`, `poll.ce308`; `ce308.final_readings_only`
      и `poll.ce308` — **решено убрать жёстко**.
- [ ] MAP fallback: `map.fallback_serial` (<FALLBACK_SERIAL>) — опрос при отсутствии основного,
      идентичность/ключи/`device_sn` остаются за основным (<PRIMARY_SERIAL>).
- [ ] Данные: старых ANT/EnBMS-ключей/таблиц нет — удалять нечего, `bms26` не
      трогать. CE308 — **решено**: телеметрию очистить (`ce308:current`, `ce308_averages_r`,
      `ts:ce308:*`), `ce308-energy:current` оставить.

---

## 7. Открытые решения (данные)

Проверка прод-БД (только чтение, 2026-10-17): старых отдельных данных ANT/EnBMS
**нет** — ни в Redis (DB0…DB15), ни в PG. `sunreceiver:bms`/`bms:current` не
существуют, `bms_averages_r` отсутствует, других таблиц вне `bms26_*` нет.
Остаётся только живой унифицированный `bms26` — его не трогаем.

| Пункт | Что накоплено | Решение |
|---|---|---|
| ANT BMS | Отдельных ANT-ключей/таблиц нет (удалены ранее). Живые ANT-устройства — в общих `bms26`-ключах / PG `bms26_averages_r`. | Удалять/переносить нечего. `bms26` не трогать. |
| EnBMS | Отдельных EnBMS-ключей/таблиц нет. Устройство — в общих `bms26`-ключах / `bms26_averages_r`. | Удалять/переносить нечего. `bms26` не трогать. |
| CE308 | Живой `ce308-energy:current` + осиротевшие `ce308:current` (2026-10-05) и PG `ce308_averages_r` (~49.8 тыс. строк, 2026-09-23…10-05). | **Решено:** очистить/удалить телеметрию (ключ `ce308:current`, таблицу `ce308_averages_r`, ряды `ts:ce308:*`); `ce308-energy:current` оставить. |

**Мы ничего не удаляли:** все проверки были read-only (`KEYS`/`GET`/`TS.INFO`/
`SELECT`). Старые ANT/EnBMS-данные отсутствовали ещё до начала задачи.

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
5. Не нашли ни одного — «данных нет» (как сейчас), без записи ошибки транспорта,
   повтор в следующем цикле.

**Идентичность (обязательно):**
- Все алгоритмы вычисления идентификатора/серийника/ключа опираются **только** на
  значение `map.serial` (<PRIMARY_SERIAL>) из настройки — фактический серийник прибора для
  этого не используется.
- Все ключи/ряды остаются прежними: Redis `sunreceiver:map:current`,
  `sunreceiver:ts:map:*`, PG `map_averages_r` — устройство `map`.
- `device_sn` (TS-метка и колонка PG `map_averages_r`) = **<PRIMARY_SERIAL>**, как и
  `DeviceSN` в снимке/дашборде — статистика пишется как по одному устройству.
- Имя источника, уведомления, ошибки устройства — по основному `serial`.

**Фактический серийник (<FALLBACK_SERIAL>):** **нигде не сохраняется** (решение 2026-10-17).
В статистике отдельного поля нет; `device_sn`/ключи всегда `<PRIMARY_SERIAL>`. Фактический
серийник используется только для выбора устройства в `/api/map/devices` и может
попадать в лог для диагностики.

**Крайние случаи:**
- Оба прибора в списке одновременно → выигрывает основной `serial`.
- Кэш `id+gen` — для фактически выбранного прибора; при 404 сбрасывать и искать
  заново (как сейчас).
- Ошибки/логи могут указывать фактический серийник для диагностики, но ключи и
  `device_sn` — всегда основной.

**Критерии приёмки:**
- [ ] При отсутствии <PRIMARY_SERIAL> и наличии <FALLBACK_SERIAL> опрос идёт, данные пишутся под `map`/<PRIMARY_SERIAL>.
- [ ] При возврате <PRIMARY_SERIAL> опрос переключается обратно (без ручного рестарта).
- [ ] `device_sn` в Redis-метках и PG `map_averages_r` = <PRIMARY_SERIAL> в обоих режимах.
- [ ] Ни один ключ/идентификатор не вычисляется из фактического серийника.
- [ ] Если ни одного нет — поведение как раньше (нет данных, повтор).
- [ ] `fallback_serial` валидируется: необязателен, не равен `serial`, число строкой.

---

## 9. Порядок работ (предложение)

1. MAP → `malina26` (изолированное удаление, данные совместимы).
2. MAP → `fallback_serial` (нужен к моменту отправки <PRIMARY_SERIAL> в ремонт; можно
   отдельной заявкой сразу после п. 1).
3. ANT BMS → удалить legacy-пулер/демон/раздел; данные не трогать.
4. EnBMS → удалить legacy-пулер/раздел; данные не трогать.
5. CE308 → оставить `final_readings_only`; **сразу** отдельным Python-скриптом
   очистить телеметрию (`ce308:current`, `ce308_averages_r` с партициями,
   `ts:ce308:*`, legacy `ce308_averages`), сохранив `ce308-energy:current`.

Всё — отдельными заявками, с `go build`/`go vet` и проверкой на секреты перед
коммитом.

**Ограничение пользователя (2026-10-17): после реализации деплой и push сразу НЕ
делать** — ждать отдельной команды. Изменения остаются в рабочем дереве.

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
  `enbmsBalanceMask` — после удаления шимов проверить (по вызовам остаются
  legacy-only).
- **`finalizeBms26` и `cacheBms26Snapshot`** (`bms26_legacy.go`) вызывает живой
  `bms26.go:307-308`; `finalizeBms26` также использует live-`bms26_alarm.go`.
  `cacheBms26Snapshot`/`loadBms26SerialCache` нужны только legacy-serial’ам
  (после удаления ANT/EnBMS кэш можно убрать, поправив вызов в `bms26.go`).
- **`kindMAP` / `kindMPPT` / `devKey` / `mapDevKey`** — живые: `malina26`
  использует синтетическую цель `kindMAP`, КЭС — `kindMPPT`/`mppt-<serial>`.
- **`mpptPollState` / `mpptEmptyTolerance`** — живые: prune КЭС в `malina26`
  (`main.go:2093`). Комментарии про `read_json.php` поправить, код оставить.
- **`requestTimeout`** объявлен в `mppt_api.go:36`, но используется только
  legacy (`bms_poller.go:166` и сам `mppt_api.go`). После удаления ANT и `malina`
  константа и `maxMPPTBody` становятся не нужны — удалить.
- **`internal/modbusmap`** остаётся: `meter_client.go` использует `ReadFull`.
  После удаления MAP-Modbus `Client`, `DefaultPort`, `ReadRegisters` в пакете
  останутся без вызовов (не критично; пакет не удалять).
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
- `map_errors.go:144-175` Modbus/`read_memory` ветки; `runMapErrorPoll` теряет
  параметр `*invTarget` (всегда nil после удаления).
- `mppt_api.go` целиком; `mppt_api_test.go` целиком.
- Упоминания в комментариях (`main.go:64-68,77,121-123,167,219-221,643-656,
  687-739,1934-2040`; `map_errors.go:26-35`; `notify.go:99`;
  `dts017_config.go:59`).

Остаётся (живое):
- `malina26.go` целиком (MAP + КЭС + BMS26); `map_store.go`, `map_pg.go`,
  `kes_store.go`, `kes_pg.go`, `avg_r.go` (spec `map_averages_r`/`kes_averages_r`);
  `saveWindowSnapshot`, `mapErrState`, `mapDevKey`, `kindMAP`/`kindMPPT`.
- `mapSourceIdentity`/`mapSourceName`: убрать ветки `targets kindMAP` и `mapAPI`,
  оставить `malina26`.

Идентичность МАП (подтверждено):
- Ключи: `sunreceiver:map:current` (HASH, поле `map`), `sunreceiver:ts:map:map:<tag>`,
  PG `sunreceiver.map_averages_r` (PK `name`, meta `device_sn`).
- `DeviceSN`/`device_sn` берётся из настроенного `serial` (`malina26.go:277`,
  `map_store.go:57`, `map_pg.go:31-32`). Ключи не зависят от прибора.
- `map_averages_r.device_sn` уже приводился к реальному `<PRIMARY_SERIAL>` отдельной
  миграцией (`docs/requirements/db-rudiment-cleanup.md`).

Fallback attach points:
- `malina26.go:81-112` `loadMalina26` — читать/валидировать `fallback_serial`.
- `malina26.go:59-74` состояние `malina26Source` (`mu`, `haveMap`, `mapID`,
  `mapGen`) — добавить `activeSerial`/`activeFallback` + `lastListCheck`.
- `malina26.go:293-317` `fetchMapRaw` — выбор: сначала `serial`, затем
  `fallback_serial`; при активном fallback периодически перечитывать
  `/api/map/devices` и возвращаться на основной.
- `malina26.go:318-333` — 404 сбрасывает `haveMap` и признак fallback.
- `malina26.go:275-280` — `res.DeviceSN` уже равен настроенному `serial`;
  менять на фактический нельзя. Фактический серийник активного прибора писать
  отдельным `physical_serial` (Redis current + TS-метка + PG), см. решение выше.
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
  `resolveBMSKey`, `decodeAntAlarms`, `recomputeMinMaxCells`, `bmsKey`-тест
  legacy-части. **Оставить/вынести:** `bmsDevice`, `bmsKey`.
- `redis_store.go:159-190` `SetBMS` (ANT-only) — удалить после удаления пулера.
- `daemons/bmslistener/` (+ `web/read_bms.php`, systemd, Makefile), цель
  `Makefile` `bmslistener`, `make all`.
- `bms26_legacy.go` ANT-часть: `antNameCap`/`antCapFromName`/`capFromRe` (если
  больше не нужен), `legacyAntSerial`, `legacyModeFromCurrent`, `antDeviceToBms26`,
  `itoa`, `antAlarmBytesFromCodes`, `setLegacyAlarmNibble` (если не нужен EnBMS).
- Доки: `docs/antbms.md`, `docs/antbms-worklog.md`,
  `docs/modules/bms-listener.md`, `docs/antbms/`; упоминания в AGENTS.md/README.

Хранение: ANT пишет только в unified `bms26` через шим — отдельных ключей/таблиц
нет. `bms26` не трогать.

### 10.4. EnBMS (прямой BLE/RS485)

Удаление:
- `main.go:262` `configFile.EnBms`, `:414-420` загрузка, `:2125,2128,2179-2184`
  `enBmsCfg`/лог, `:2494-2533` запуск и BLE-лимит.
- `enBms.go` (парсеры/протокол), `enBms_client.go`, `enBms_rs485.go`,
  `enBms_poller.go`, `enBms_store.go`.
- `enbmsMaxBTDevices` (`enBms.go:161-166`) и проверка лимита в `main.go`.
- `bms26_legacy.go` EnBMS-часть: `enbmsSnapshotToBms26`, `legacyEnbmsSerial`,
  `enbmsAlarmBytesFromStrings`, `legacyModeFromText`, `enbmsBalanceMask` (если
  legacy-only).
- **Оставить/вынести:** `enbmsExtGroups` + `enbmsExtBit` (живой
  `bms26_alarm.go`), `setLegacyAlarmBit` (используется `enbmsPackAlarm`; после
  удаления EnBMS проверить, нужен ли live-коду).
- Тесты `enBms_test.go`, `enBms_state_test.go` (проверки Ext_Bit при
  необходимости перенести).
- Доки: `docs/modules/enbms.md`, упоминания в AGENTS.md/README.

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
  `QueryCE308Averages`/`CE308AveragesAll`, spec `ce308_averages_r` в `avg_r.go`.
- `main.go`: запуск обычного цикла + аккумулятора, восстановление CE308-ряда
  (`restoreCE308Series`, `:2312,3042-3080`), `CE308` в `IsEmpty` и
  `redisCE308DataPresent`.
- `dashboard.go`: `apiCE308Current`, `apiCE308Series`, `ce308SeriesRange`,
  `ce308SeriesPoint`, поля `CE308L1/L2/L3Voltage`/`CE308ActivePower`, их
  downsample; резерв мощности гаража `freshCE308Power`/`ce308Name`/параметр
  `ce308` в `buildAnimationResponse`.
- `ce308.go`: теги `ce308_*`, `ce308CurrentTags` (мёртвый), `buildCE308Reads`,
  `toWatts`, `ce308ReadsValid`, `valuesCE308`, `ce308Snapshot`; `ce308Floats`
  в `ce308_client.go`.
- `poll.ce308` (`main.go:249,294-299,305`; `:44`) — final его не использует.
- Web: блок `ce308Cur` в `index.html:73-94`, `renderCE308Current` +
  `/api/ce308/current` в `dashboard.js`, CE308-ряды в `charts.js:11-34,465-484,
  517-533`, атрибуты `data-ce308-final`/`show-ce308` (согласованно с JS),
  CSS `.ce308-scroll/.ce308-table/.ce-num/.ce-pos/.ce-neg`.
- Тесты: `ce308_test.go:27-56,138-179` (normal), `ce308_probe_test.go`,
  `dashboard_animation_test.go:131-183`; `ce308-probe.sh` (tracked).
  **Решение 2026-10-17: `ce308-probe.sh` и `ce308_probe_test.go` удалить.**

Оставить (final):
- `runCe308FinalReadings`, `ce308EnergyDelay`, `saveCE308Energy`,
  `CE308Energy`, `ce308EnergySnapshot`/`readCE308Energy`/`parseCE308End`,
  `ce308Config`/`ce308ConfigFromSection`/`describeCE308Config`,
  `ce308_client.go` (BLE), `ce308_agent_*` (PIN/адаптер), ошибки
  `device_errors kind=ce308` (пишутся и в final).
- Web: рамка «показания» (`index.html:108-127`), `renderCE308Energy`,
  кнопка/POST `/api/ce308/energy`, `data-show-ce308`.
- `sunreceiver:ce308-energy:current`.

Данные (cleanup):
- Удалить `sunreceiver:ce308:current`, все `sunreceiver:ts:ce308:*`
  (**retention 60 суток, `PurgeOld` — no-op**, поэтому удалять префиксом, не ждать),
  PG `sunreceiver.ce308_averages_r` **вместе с партициями** `*_pYYYYMM`;
  проверить и удалить legacy `sunreceiver.ce308_averages` (может быть создана
  `migrate_avg.go`/`scripts/step1_averages_partitioned.py`), а также старый
  `sunreceiver:ce308:energy` (не путать с `ce308-energy:current`).
- **Не трогать** `sunreceiver:ce308-energy:current` и `device_errors`.
- Важно: пока stale-ключи `ce308:current`/`ts:ce308:*` существуют, `IsEmpty`
  (`redis_store.go:224`) и `redisCE308DataPresent` (`main.go:2852-2866`) считают
  Redis непустым и могут подавлять реставрацию остальных рядов из PG.

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

### 10.7. Инфраструктура, доки, тесты

- Makefile: цели/артефакты `bmslistener` (`:11,19,21,42-50,80-89`) и
  `mapgateway` (`:12,52-59,91-100`), `all` (`:21`).
- `daemons/mapgateway/`, `daemons/bmslistener/` — удалить целиком.
- `tools/` — **весь каталог в `.gitignore`**, в git не входит; к runtime не
  подключён (bat_stat_scripts/seplos ссылаются на старые ключи, ce308stat/map-loader
  исторические). **Решение 2026-10-17: оставить локально** (вне git-объёма).
- `ce308-probe.sh` — tracked, завязан на normal-телеметрию.
- Доки к удалению/обновлению: `docs/read_json.md`, `docs/malina-web-api.md`,
  `docs/malina-bug_Inet_flag.md`, `docs/antbms.md`, `docs/antbms-worklog.md`,
  `docs/antbms/`, `docs/modules/bms-listener.md`, `docs/modules/enbms.md`,
  `docs/research/map-signs-audit.md`; обновить `docs/README.md`,
  `docs/modules/{README,map-mppt,notify,storage,dashboard,inverter-poller,bms26,
  ce308,dts017m-meter,universal-contract}.md`, `docs/research/README.md`,
  `docs/research/redis-storage-optimization.md`, `AGENTS.md`.
- Тесты к удалению/правке: `mppt_api_test.go`, `bms_poller_test.go` (legacy-часть),
  `enBms_test.go`, `enBms_state_test.go`, `bms26_legacy_test.go` (legacy-часть),
  часть `ce308_test.go`, `ce308_probe_test.go`, `dashboard_animation_test.go`,
  `main_config_test.go` (поля poll/ce308).
- i18n: CE308-«текущие данные»/суффиксы графиков; ANT/EnBMS-подписи в AGM/tooltip;
  фильтры ошибок `antbms`/`enbms` (исторические записи `device_errors`).

### 10.8. Аудит данных (подтверждён)

- Redis DB0…DB15: ключей `*bms*`/`*enbms*`/`*ant*` вне `bms26` нет;
  `sunreceiver:bms`, `sunreceiver:bms:current` не существуют.
- PG: таблиц `bms`/`ant`/`enbms` вне `bms26_*` нет; `bms_averages_r` отсутствует.
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
как механизм остался **по модулям** и используется дашбордом
(`allCurrent`, `dashboard.go:1061-1086`):
`sunreceiver:{deye,sofar,dds238,map,kes,dts017m,bms26}:current` и
`sunreceiver:ce308-energy:current` (+ `ce308:current` — обычный режим CE308,
удаляется). Все они значимы.

`sunreceiver:map:current` (аудит 2026-10-17):
- поле `map` — **живое и значимое**: снимок с 10 тегами, `device_sn=<PRIMARY_SERIAL>`,
  пишется каждую секунду `saveMapSnapshot`, читается `MapCurrent` →
  `/api/current` и `/api/agm/current`. Оставлять.
- поле `""` — **рудимент**: снимок от 2026-10-16 09:35, никем не читается
  (`MapCurrent` читает поле `map`), реставрация из PG его не создаёт
  (для МАП вызывается `saveMapSnapshot`). Кандидат на `HDEL`.

PG `sunreceiver.map_averages_r` (аудит 2026-10-17):
- `device_sn='<PRIMARY_SERIAL>'` — 563 734 строки (2020-12-31 … сейчас), живые.
- `device_sn=''` — 477 строк (2026-10-15 … 2026-10-16 15:40); **все 477 имеют
  строку-дубль с тем же `ts` и `device_sn='<PRIMARY_SERIAL>'`** → дубликаты, удаление
  истории не теряет.

**Решение (2026-10-17): удалить только рудимент.**
- `HDEL sunreceiver:map:current ""` — поле `map` и сам ключ остаются.
- `DELETE FROM sunreceiver.map_averages_r WHERE device_sn=''` — 477 дублей.
- Выполнять отдельным Python-скриптом с dev-ПК (правило проекта), не в бинарнике.
- Код не меняется: `saveMapSnapshot` уже пишет только поле `map`.

---

## 11. Вопросы

### Закрыты (решения 2026-10-17)

1. MAP fallback: перепроверка основного — **раз в 60 с**.
2. Фактический серийник — **нигде не хранить** (только выбор устройства/лог).
3. Stale-детект `mapTracker` для `malina26` — **починить заодно**.
4. Legacy-доки MAP — **удалить**.
5. Фильтры ошибок `antbms`/`enbms` — **удалить**.
6. CE308-конфиг (`poll.ce308`, `final_readings_only`) — **убрать жёстко**.
7. CE308 cleanup — **сразу**, отдельным Python-скриптом.
8. MAP-конфиг — **только новый плоский вид**.
9. `ce308-probe.sh` + `ce308_probe_test.go` — **удалить**.
10. Общий код (`bmsDevice`/`bmsKey`, `enbmsExtGroups`/`enbmsExtBit`) —
    **вынести в общие файлы** (`bms_view.go`, файл алармов BMS26) как чистый
    рефакторинг.
