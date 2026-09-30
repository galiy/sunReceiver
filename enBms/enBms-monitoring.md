# План разработки: мониторинг BMS EnBMS (Enjie EMU110x) по BLE

Статус-легенда: `[ ]` — не сделано, `[~]` — в работе, `[x]` — сделано.

## 0. Контекст и источники

- Протокол и живой срез: `/home/sasha/src/energybms/PROTOCOL.md`,
  `/home/sasha/src/energybms/DEVICE_SNAPSHOT.md`, `/home/sasha/src/energybms/REVERSE_ENGINEERING.md`.
- Образец BT-транспорта (соединение, поддержание, восстановление, ошибки):
  модуль `ce308_*.go` этого репозитория.
- Образец опроса/хранения/усреднения данных BMS: модуль ANT BMS
  (`bms_poller.go`, `bms_accumulator.go`, `redis_store.go` SetBMS/SaveBMSSeries,
  `pg_store.go` bms_averages).
- Задача: только пулер и сервисные процессы обслуживания данных. Фронтенд и
  бэкенд дашборда НЕ трогаем.

### Что читаем

Только блок `Battery`, CID2 = `0x61` (телеметрия). Остальные блоки
(BasicInfo/ParallelBattery/ReadBMSParams/…) не читаем.

### Протокол (кратко)

Транспорт BLE GATT:

| Роль | UUID |
|---|---|
| Сервис | `0000ff00-0000-1000-8000-00805f9b34fb` |
| Запись команд (host→BMS) | `0000ff02-…` |
| Уведомления/ответы (BMS→host) | `0000ff01-…` |

Кадр (big-endian):

```
Запрос: 7E 10 ADR 46 CID2 LEN(2) INFO CHKSUM(2) 0D
Ответ:  7E 14 ADR CID2 00 LEN(2) INFO CHKSUM(2) 0D
```

- CRC-16/CCITT (полином `0x1021`, init 0) по `frame[1 : len-3]` (без SOI,
  CRC-поля и EOI); запись `CHKSUM` — big-endian.
- Полная длина кадра = `10 + LENID`, где `LENID = frame[5]<<8 | frame[6]`.
  Сборка — **по длине**, а не по `0x0D` (байт `0x0D` встречается внутри payload).
- Запрос Battery: `7E 10 00 46 61 00 01 00 <crc> 0D` (ADR=0, INFO=`[00]`).
- Ответ приходит несколькими notify-фрагментами — склеиваем буфер до полного
  кадра.

### Payload Battery (`0x61`), 106 Б

| Поле | Тип | Масштаб |
|---|---|---|
| dataflag | u8 | — |
| slaveNo | u8 | индекс пакета |
| batterynum | u8 | число ячеек |
| voltagelist | batterynum × u16 | ×0.001 В |
| tempnum | u8 | число датчиков |
| templist | tempnum × u16 | ×0.1 K, −273.1 → °C |
| chargecurrent | **s16** | ×0.01 А (минус = разряд) |
| totalvoltage | u16 | ×0.01 В |
| leftcapacity | u16 | ×0.01 А·ч |
| customerp | **u8** | (1 байт!) |
| totalcapacity | u16 | ×0.01 А·ч |
| soc | u16 | ×0.1 % |
| ratedcapacity | u16 | ×0.01 А·ч |
| cycles | u16 | ×1 |
| soh | u16 | ×0.1 % |
| portvoltage | u16 | ×0.01 В |
| предупреждения/статусы | остаток | не парсим |

Мощность отдельным полем не отдаётся — считаем `P = totalvoltage × chargecurrent`.

## 1. Конфигурация `sunReceiver.json`

Новый раздел `enBms`: объект с общим обязательным `disabled` и коллекцией
`devices`. Каждый элемент: `mac` (обязателен для BLE-подключения) и/или `name`
(необязательное отображаемое имя) + обязательный `disabled`.

```json
"enBms": {
  "disabled": false,
  "devices": [
    { "name": "BMS BP00", "mac": "AA:BB:CC:DD:EE:00", "disabled": false }
  ]
}
```

- `disabled` (общий) отсутствует → ошибка загрузки конфига (как у ce308).
- `disabled` устройства отсутствует → ошибка загрузки конфига.
- Все устройства `disabled=true` или пустая коллекция → пулер не запускается.
- Ключ устройства в Redis/PG — **MAC** (уникален); `name` — только отображение
  (заводское BLE-имя `BP00` не уникально, см. DEVICE_SNAPSHOT).
- Обновить `sunReceiver.sample.json`.
- Приватный `sunReceiver.json` — добавить реальный MAC `AA:BB:CC:DD:EE:00`.

## 2. Модуль BT-транспорта `enBms_client.go`

По образцу `ce308_client.go`/`ce308_agent_linux.go`:

- [x] `enbmsConn` — постоянное BLE-соединение: dev, tx (ff02), notify (ff01),
  буфер приёма, таймаут, ctx.
- [x] `openEnBms(mac, ctx)` — подключение: ensure powered, direct Connect,
  обработка «залипшего» состояния (StopDiscovery+Disconnect), fallback-discovery
  если BlueZ не знает устройство. Переиспользовать generic-хелперы
  `ce308EnsurePowered`/`ensureCE308Known`/`ce308ClearStuck`/`ce308DeviceKnown`/
  `ce308AdapterIDReset` (они не CE308-специфичны).
- [x] `enbmsConn.readBattery()` — запись кадра запроса + ожидание полного кадра
  с валидным CRC-16/CCITT. Разбор буфера по длине; таймаут → retry-ошибка;
  отмена ctx → закрыть.
- [x] классификация ошибок: `errEnBmsReadTimeout` (ретрай), `errEnBmsClosed`
  (остановка), `isEnBmsConnStuck`.
- [x] CRC-16/CCITT + `buildEnBmsFrame` + `extractEnBmsFrame`.
- [x] `Close()`.

## 3. Парсинг и модель данных `enBms.go`

- [x] `logEnBms`.
- [x] `enBmsSection`/`enBmsDeviceSection`/`enBmsConfig`/`enBmsDeviceConfig`.
- [x] `enBmsConfigFromSection` (+ `describeEnBmsConfig`).
- [x] `enbmsParsed` — декодированный Battery.
- [x] `parseEnBmsBattery(payload []byte) (enbmsParsed, error)` с защитой от
  коротких/битых payload.
- [x] `enbmsSnapshot` — снимок (текущее состояние): те же поля, что у ANT BMS
  (`cells_v`, `temperatures_c`, `current_a`, `power_w`, `soc`, `capacity_ah`,
  `remaining_ah`, max/min/avg cell и т.п.) + `mac`.
- [x] `enbmsReadsValid` — грубая валидация (диапазоны, конечность).

## 4. Пулер `enBms_poller.go`

- [x] `enbmsPollInterval = 1s` — общий цикл не чаще 1 раза в секунду.
- [x] `runEnBmsPoll(store, pg, cfg, ctx)`: один цикл = последовательный опрос
  всех устройств по одному; каждое устройство держит ПОСТОЯННОЕ соединение
  (не рвём между опросами); при обрыве — переподключение с ограниченным
  бэкоффом (как `ce308Backoff`), ретрай таймаута чтения.
- [x] состояние на устройство: `enbmsPollerDev{conn, reconnect, lastFailLog,
  consecFails}`.
- [x] после цикла — пауза до `1s` от начала цикла (если цикл дольше — без паузы).
- [x] запись `Current` + `History` (в ряд) + кормление аккумулятора.

## 5. Лимит BT-устройств

- [x] Константа `enbmsMaxBTDevices = 5` (уточнить реальный предел адаптера).
- [x] В main перед запуском: `len(activeEnBms) + (ce308 active ? 1 : 0) > 5` →
  НЕ запускать опрос enBms, писать ошибку в лог.

## 6. Хранение данных

По образцу ANT BMS:

- [x] Redis: `sunreceiver:enbms:current` (HASH, поле=MAC, JSON `enbmsSnapshot`),
  `sunreceiver:enbms:series:<YYYY-MM>` (ZSET, score=Unix сек,
  member=JSON `enbmsSeriesPoint`, 5-мин усреднённые точки, TTL 40 дней).
- [x] `enbms_store.go`: `SaveEnBmsCurrent`, `SaveEnBmsHistory`, `QueryEnBmsSeries`.
- [x] `enbms_accumulator.go`: 5-минутные бакеты (`avgStep`), усреднение
  мгновенных полей/ячеек/температур, дискретные (число ячеек) — по последнему;
  `Samples`. `closed`/`drain`.
- [x] `enbmsSeriesPoint`/`enbmsAveraged` (аналог bmsSeriesPoint/bmsAveraged).
- [x] PG: таблица `sunreceiver.enbms_averages` (name, ts, values jsonb, PK
  (name, ts)); `InsertEnBmsAveraged` + `EnBmsAverages` + `insertEnBmsAveragedExec`
  с sample-count guard.
- [x] `saveEnBmsClosedBuckets` (Redis всегда; PG только полные бакеты;
  при остановке — drain только в Redis).

## 7. Интеграция в `main.go`

- [x] `configFile.EnBms *enBmsSection`.
- [x] `loadConfig`: проверка `disabled`, вызов `enBmsConfigFromSection`, возврат
  `*enBmsConfig` (обновить сигнатуру и тесты).
- [x] Запуск `runEnBmsPoll` + `runEnBmsAccumulator` при активном конфиге и
  соблюдении лимита BT.
- [x] Логи о состоянии (настроен/отключён/превышен лимит BT).

## 8. Документация и тесты

- [x] `enBms/enBms-monitoring.md` (этот план) — отметки по ходу.
- [x] `enBms/context.md` — локальный контекст задачи (решения, ссылки).
- [x] `docs/modules/enbms.md` — описание модуля; ссылка из `docs/README.md` и
  `AGENTS.md`.
- [x] `sunReceiver.sample.json` — раздел `enBms`.
- [x] Тесты: CRC/кадр/парсинг Battery (по живому кадру из DEVICE_SNAPSHOT),
  валидация конфига, аккумулятор.
- [x] `go vet ./...`, `go build ./...`, `go test ./...`.

## Журнал выполнения

| Дата | Пункт | Статус |
|---|---|---|
| 2026-09-30 | План составлен | [x] |
| 2026-09-30 | Конфиг, кодек/CRC, парсинг Battery, модель снимка (`enBms.go`) | [x] |
| 2026-09-30 | BLE-транспорт (`enBms_client.go`) | [x] |
| 2026-09-30 | Пулер (`enBms_poller.go`) | [x] |
| 2026-09-30 | Аккумулятор 5-мин (`enBms_accumulator.go`) | [x] |
| 2026-09-30 | Хранение Redis/PG (`enBms_store.go`, `pg_store.go`) | [x] |
| 2026-09-30 | Лимит BT + интеграция в `main.go` | [x] |
| 2026-09-30 | Конфиги (sample + приватный) | [x] |
| 2026-09-30 | Документация (`docs/modules/enbms.md`, `enBms/context.md`) | [x] |
| 2026-09-30 | Тесты, `go vet`, `go build`, `go test` | [x] |
| 2026-09-30 | Прод: деплой, живая проверка чтения BP00 (Redis current/series) | [x] |
| 2026-09-30 | Независимое ревью + исправление замечаний (PurgeOld, restore, гонка адаптера, троттлинг логов, валидация) | [x] |
| 2026-09-30 | Интеграция в дашборд: `/api/bms*` отдают EnBMS (kind), блок «BMS (батареи)» на главной, страница `/bms/<MAC>` | [x] |

## Заметки по реализации

- **Формат кадра** (уточнено по живому кадру, снятому bleak с устройства, и по
  CRC-16/CCITT):
  `Запрос: 7E 10 ADR 46 CID2 LEN(2) INFO(с 7) CHKSUM(2) 0D`;
  `Ответ:  7E 14 ADR CID2 RTN LEN(2) INFO(с 7) CHKSUM(2) 0D`;
  полная длина `10 + LENID`, `LENID = frame[5]<<8 | frame[6]`.
  В `PROTOCOL.md` §3.1 ошибочно указано «9 + LENID», а в `DEVICE_SNAPSHOT.md`
  полные кадры приведены без байта `RTN` (CRC тогда не сходится). Ориентир —
  живой кадр; в `enBms_test.go` он зафиксирован как `TestEnBmsRealFrameFromDevice`.
- Лицензионные заголовки — как во всех `.go` файлах репозитория.
- Живая проверка 2026-09-30: после исправления формата enBms штатно читает
  Battery и пишет снимки в Redis.
