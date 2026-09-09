# sunReceiver

Go-приложение, которое опрашивает solar-инверторы через их WiFi-даталоггеры (Solarman LSW-3/LSE, порт 8899, TCP) и сохраняет распарсенные данные в Redis (с persistence RDB+AOF) + опционально в JSON-файлы. При полностью пустом Redis данные восстанавливаются из PostgreSQL. Включает веб-дашборд текущих параметров.

Язык/команды: Go 1.26, `go run .` — запуск, `go vet ./...` — проверки. Коммиты писать по-русски, как в истории репо. После каждого тестового запуска чистить папку `data/` (`rm -rf data`, в .gitignore она уже есть).

## Устройство: даталоггеры, адреса, модели

| IP | Модель | Статус опроса (проверено 2026-09-03) |
|---|---|---|
| 192.168.13.76 | **Sofar K-TLX** (LSW-3) | РАБОТАЕТ: отвечает 205-380 байт (3-4 кадра), внутри — Modbus-ответ func 03 со ВСЕМИ 40 регистрами 0x0000-0x0027 (bytecount 80, CRC валидный) + дубль 16 регистров в отдельном кадре. LoggerSN = **1758966933** (`68d7b495`, hex-строка логгера `95b4d768`). |
| 192.168.13.91 | Deye (string) | РАБОТАЕТ: отдаёт данные по Solarman V5-кадру с 15-байтным datafield и реальным SN логгера. LoggerSN = **1774265353** (`69c12409`). |
| 192.168.13.70 | Deye (string) | То же, что .91. LoggerSN = **2947602822** (`afb0d986`). |
| 192.168.13.79 | Deye (string) | РАБОТАЕТ (проверено 2026-09-03, ~8 с на оба диапазона). LoggerSN = **2947000147** (`afa7a753`). |
| 192.168.13.92 | Deye (string) | РАБОТАЕТ (проверено 2026-09-03, ~8 с). LoggerSN = **1774585911** (`69c60837`). |
| 192.168.13.93 | Deye (string) | РАБОТАЕТ (проверено 2026-09-03, ~8 с). LoggerSN = **1766715945** (`694df229`). |

Список опрашиваемых инверторов задаётся в **`sunReceiver.json` рядом с исполняемым файлом** (`os.Executable()`; при `go run .` — fallback в CWD). Структура: раздел **`invertors`** — список инверторов `[{"ip", "name", "type": "deye"|"sofar", "logger_sn": uint32, "disabled": bool}]`; `name` — логическое имя (обязательно); **`disabled` — ОБЯЗАТЕЛЬНОЕ поле** (`false` = опрашивается, `true` = временно отключён — устройство в конфиге, но не опрашивается, при старте логируется; отсутствие поля = ошибка загрузки конфига). МАП Титанатор — **отдельный раздел `map`** `{"name", "ip", "unit"}` (Modbus TCP, unit по умолч. 1; не входит в `invertors`). MPPT-контроллеры — раздел **`mppt`** `{"base_url", "mppt_path", "login", "password"}` (пароль в открытом виде, файл в git не выгружается). Опрос Deye/Sofar — раз в 10 секунд; МАП и MPPT — 1 раз в секунду (с сохранением 1 точки за 10 с).

## Протокол Solarman V5 — выводы из реверса (важно, Sofar_LSW3.py устарел)

### Формат кадра (по эталонной библиотеке github.com/snowirbis/solarman v1.0.4, проверено на живом .76)

Запрос (read holding registers):
```
A5 | PayloadLen u16 LE | Control 10 45 (LE 0x4510) | Serial u16 LE | DeviceSN u32 LE | Payload | Checksum u8 | 15
```
- `PayloadLen` = длина payload (НЕ 0x1700 как в Sofar_LSW3.py!). Для read: 12 (заголовок) + 6 (Modbus PDU) + 2 (CRC) = 20 → `14 00` LE.
- Заголовок payload (12 байт): `02` (FrameType) + `0000` (SensorType u16 LE) + `00000000` (DeliveryTime u32 LE) + `00000000` (PowerOnTime u32 LE) + `00000000` (OffsetTime u32 LE)
- Modbus PDU (6 байт): `01` (адрес устройства) `03` (func) | StartReg u16 **BE** | Count u16 **BE**
- CRC16-Modbus (init 0xFFFF, poly 0xA001 отражённый, без invert — стандартный) по 6 байтам PDU, пишется **little-endian** (low byte первым). Sofar_LSW3.py пишет high-first — это баг старой реализации.
- Checksum = `sum(frame[1 : len-2]) mod 256` — сумма всех байтов от 2-го до предпоследнего (не включая сам checksum-байт и end-маркер). В эталоне: `calcCheckSum8(buf.Bytes()[1:])`, где buf — кадр без end-маркера.
- DeviceSN в запросе: Sofar отвечает и при SN=0 (проверено), Deye тоже отвечает heartbeat'ом при SN=0. Для Sofar достаточно SN=0. Реальный SN логгера виден в ответах (поле после serial'а, LE u32).

Ответ: те же маркеры, control в ответе = `15 10` (LE 0x1510), serial u16 **big-endian** в ответе. Длина кадра = **11 + PayloadLen + 2**. Кадров может быть несколько подряд в одном TCP-ответе (у .76 их 3: heartbeat + placeholder + данные; у Deye — один heartbeat).

### Структура ответа с данными (Sofar .76)
Ответ — 3-4 кадра: heartbeat (payload 16), placeholder (payload 99/137, data-область нулями) и 1-2 кадра с данными.
Кадр с данными: внутри:
- 0..13 — заголовок payload (frameType 02, status 01, deliveryTime, powerOnTime, offsetTime)
- затем (после возможного padding) Modbus-ответ: `01 03` | ByteCount u8 | данные (2 байта BE на регистр) | CRC16 LE (2 байта)
- Sofar LSW-3 отвечает ВЕСЬ блок 0x0000-0x0027 (bytecount 80 = 40 регистров) независимо от запрошенного диапазона (проверено: запрос 4 рег. и запрос 0x0105 дают тот же полный блок). Парсить нужно по ByteCount.

### Маппинг регистров Sofar K-TLX (из SOFARMap.xml проекта Sofar_LSW3, проверен против живых значений)
Диапазон 1: 0x0000–0x0027, func 03:
- 0x0000 Inverter status (0 Stand-by, 1 Self-checking, 2 Normal, 3 FAULT, 4 Permanent)
- 0x0001–0x0005 Fault 1–5 (битовая маска: 1 ID01 Grid OV, 2 ID02 Grid UV, 4 ID03 Grid OF, 8 ID04 Grid UF, 16 ID05 PV UV, 32 ID06 LVRT, 256 ID09 PV OV, 512 ID10 PV current unbalanced, 1024 ID11, 2048 ID12 GFCI, 4096 ID13 phase sequence, 8192 ID14 boost OC, 16384 ID15 AC OC, 32768 ID16 grid current high)
- 0x0006 PV1 Voltage ×0.1 V, 0x0007 PV1 Current ×0.01 A, 0x0008 PV2 Voltage ×0.1 V, 0x0009 PV2 Current ×0.01 A
- 0x000A PV1 Power ×10 W, 0x000B PV2 Power ×10 W, 0x000C Output active power ×10 W, 0x000D Output reactive power ×0.01 kVar
- 0x000E Grid frequency ×0.01 Hz, 0x000F L1 V ×0.1 V, 0x0010 L1 I ×0.01 A, 0x0011 L2 V ×0.1 V, 0x0012 L2 I ×0.01 A, 0x0013 L3 V ×0.1 V, 0x0014 L3 I ×0.01 A
- 0x0015/0x0016 Total production (32 бит: high*65536+low) kWh, 0x0017/0x0018 Total generation time (32 бит) h
- 0x0019 Today production ×10 Wh, 0x001A Today generation time min
- 0x001B module temp ºC, 0x001C inner temp ºC, 0x001D bus voltage ×0.1 V
- 0x001E/0x001F PV1 sample slave CPU ×0.1 V / ×0.1 A, 0x0020 countdown s, 0x0021 alert, 0x0022 input mode, 0x0023 comm board msg
- 0x0024/0x0025/0x0026 insulation PV1+/PV2+/PV- to ground (Ом), 0x0027 Country (0 DE, 12 PL, 9 UK-G59, … см. SOFARMap.xml)
Диапазон 2: 0x0105–0x0114 (func 03): String 1–8 voltage ×0.1 V / current ×0.01 A (V на чётных: 0105,0107,0109,010B,010D,010F,0111,0113)
Диапазон HW: 0x2000–0x200D func 04: Product code, Serial Number, Software/Hardware/DSP versions (строки 2 байта/регистр, без ratio).

Значения регистров — int16 (знаковые, two's complement); для положительных величин обычно unsigned.

### Deye (.91, .70, .79, .92, .93) — как читать (решено 2026-09-03)
Deye-логгеры (LSE, rebrand Solarman) понимают Solarman V5-кадр, НО с двумя обязательными отличиями от Sofar:
1. **15-байтный datafield-заголовок** (НЕ 12): `02` + 14 нулей (`02000000 00000000 00000000 0000`). PayloadLen = 15 + (6 для PDU + 2 CRC) = **23** (`17 00` LE). Если слать 14-байтный заголовок — логгер отвечает 0x05.
2. **Реальный SN даталоггера** в DeviceSN (bin LE), НЕ 0. Если SN не совпадает — логгер отвечает heartbeat с кодом ошибки **0x06** ("serial number does not match").
Запрос: по сути наш `BuildReadFrame`, но строка datafield длиной 15 байт и SN логгера. Реализован как `solarman.BuildDeyeReadFrame(deviceSN, unit, startReg, regCount)` и `client.ReadRegistersDeye(start, count, unit)`.
Ответ: Modbus-ответ func 03 лежит в payload с offset 14 (после 15-байтного заголовка... на практике парсится поиском `01 03 <vlen>` через `ParseModbusPDU`).
Коды ошибок логгера в 29-байтном heartbeat (payload[14]): **0x05** = "Modbus device address does not match", **0x06** = "Logger Serial Number does not match". Проверено: inverter SN (2405018274 для .70) даёт 0x06, logger SN (2947602822) проходит и данные читаются.
Маппинг регистров Deye string (в `deyeRegMap` в main.go):
- 0x3C Production today ×0.1 kWh, 0x3E Uptime min, 0x3F-0x40 Total production (32 бит, LW first) ×0.1 kWh
- 0x46/0x47/0x48 Grid L12/L23/L31 V ×0.1, 0x49/0x4A/0x4B L1/L2/L3 V ×0.1, 0x4C/0x4D/0x4E L1/L2/L3 I ×0.1
- 0x4F AC Freq ×0.01 Hz, 0x50 Operating power ×0.1 W, 0x52 DC total power ×0.1 W, 0x54 AC apparent power ×0.1 W, 0x56-0x57 AC active power (32) ×0.1 W, 0x58 AC reactive power ×0.1 W
- 0x5A Radiator temp ×0.1 −100 offset, 0x5B IGBT temp ×0.1 −100 offset
- 0x6D/0x6E PV1 V/I ×0.1, 0x6F/0x70 PV2 V/I ×0.1, 0x71/0x72 PV3 V/I ×0.1, 0x73/0x74 PV4 V/I ×0.1 (на наших 2-цепных 1-фазных — 0, но часть string-маппинга kbialek)
- 0xC6-0xC7 Load power (32, signed) ×1 W, 0xC8 Daily load ×0.01 kWh, 0xC9-0xCA Total load (32) ×0.1 kWh
- 0xCB-0xCC Grid power (32) ×1 W, 0xCD Daily sold ×0.01 kWh, 0xCE-0xCF Total sold (32) ×0.1 kWh, 0xD0 Daily bought ×0.01 kWh, 0xD1-0xD2 Total bought (32) ×0.1 kWh
Пробелы в диапазоне чтения (не задокументированы в kbialek string-группе): 0x3D, 0x41-0x45, 0x51, 0x53, 0x55, 0x59, 0x5C-0x6C. Проверял mxbode/Deye-SUN-SG05LP3-EU-SM2-Modbus-TSV — это карта ГИБРИДНОЙ модели SG05LP3, не нашей string: её адреса противоречат нашим живым значениям (у них 0x6D = «Max A Charge», у нас 0x6D = PV1 voltage 212V), поэтому её имена для нашего диапазона не использовал. Из неё лишь совпадение по адресу: у них 0x3D = Fernsperre (дистанционный замок) и 0x51 = SchalterModus (режим работы) — у нас оба = 0, согласуется, но НЕ верифицировано, в JSON оставлены hex. Единственный непустой на всех 5 логгерах — 0x5D = 1000 (константа; вероятно ограничение мощности 100% ×10, не подтверждено). В `raw_registers` остаются под hex-адресом.
Регистры 0x005B IGBT temp не подключён (0 регистр → −100). Логгеры отвечают стабильно и быстро (~8 с на оба диапазона).

## Текущее состояние кода
- `solarman/` — пакет-клиент Solarman V5: `BuildReadFrame` (12-байтный datafield, для Sofar), `BuildDeyeReadFrame` (15-байтный datafield + реальный SN, для Deye), `SplitFrames` (длина кадра = **11** + PayloadLen + 2, префикс A5+len+control+serial+SN), `ParseModbusPDU` (01 03/04 | bytecount | data | crc16 LE), `Checksum8` (sum[1:len-2] mod 256), `CRC16Modbus` (стандартный, в PDU пишется LE).
- `main.go` — poller: каждые 10 с параллельно (goroutine) TCP-опрос целей из `sunReceiver.json` (порт 8899). Список целей читается из **`sunReceiver.json` рядом с бинарником** (`os.Executable()`; при `go run .` — fallback в CWD) через `loadConfig` в `targets []invTarget` {IP, Name, LoggerSN, Kind}: раздел `invertors` → `type` "deye"→`kindDeyeString`, "sofar"→`kindSofar` (поле `disabled` обязательно; `true` пропускается); раздел `map` → `kindMAP` (MPPT — `kindMPPT` — в конфиг НЕ задаётся, появляется динамически см. секцию МАП). `name` — **логическое имя** инвертора (обязательное поле, напр. `Deye Left`, `Sofar-2.5`, `Bineos Right`; для MPPT имя генерируется как `MPPT-<slot+1>`). Deye — реальный SN даталоггера (`ReadRegistersDeye`), Sofar — тоже `ReadRegistersDeye` с 15-байтным datafield и реальным SN (проверено 2026-09-07: 12-байтный `ReadRegisters`/`BuildReadFrame` Sofar LSW-3 отвечает только heartbeat с кодом 0x05; на 15-байтный кадр — полный блок 0x0000-0x0027). Маппинг регистров: Sofar — в `mapSofarRegisters` (регистрации 0x0000-0x0027), Deye string — в `deyeRegMap`/`mapDeyeRegisters` (у каждого сенсора `Tag` — имя контракта в values). МАП (kindMAP, Modbus TCP) и MPPT (kindMPPT, веб-API ПАК «Малина») — в отдельном 1-сек цикле `runMapPoll` (см. секцию МАП). **Poller пишет СРАЗУ только в Redis** (`redis_store.go`) — как live-хранилище; в PostgreSQL (`pg_store.go`) данные НЕ пишутся напрямую — туда идёт усреднённые 5-минутные точки от фонового процесса аккумуляции (см. `accumulator.go`). Запись JSON-файлов в `data/` — **только по флагу `-file`** (код вынесен в `writeFiles`). Флаги CLI: `-redis <addr>` (по умолч. `127.0.0.1:6379`), `-pg <dsn>` (по умолч. `postgres://localhost:5432/sunreceiver?sslmode=disable`, пустая строка выключает PG), `-pg-restore-window <dur>` (по умолч. `720h`), `-dashboard <addr>` (по умолч. `:8080`, пустая строка выключает), `-file`. Веб-дашборд — `dashboard.go`. Снимок (структура `deviceSnapshot`: **`{name, ip, timestamp, device_sn, values}`**) отправляется только при успешном чтении данных (`HasData`); при `heartbeat_only`/`no data`/ошибке не сохраняется. `name`/`ip` — изsunReceiver.json (для kindMAP-слота/kindMPPT `ip` — `devKey`, напр. `IP#mppt0`); `values` — **универсальный контракт** (см. ниже). **`raw_registers` в снимок НЕ пишется** — только общие теги `commonContractTags`.

### Хранение: Redis (последние 2 календарных суток) + PostgreSQL (5-минутные средние)
Схема хранения (2026-09-07): **Redis — горячие данные за последние 2 календарных суток** (полное разрешение ~10 с), **PostgreSQL — вся история, но только в виде усреднённых за 5 минут точек** (12 фиксированных промежутков в час). Распределение ролей:
- **Поток данных**: логгер → poller → Redis (сразу, каждые 10 с). Отдельный фоновый процесс `runAccumulator` в `accumulator.go` каждый час по завершении очередного 5-минутного промежутка читает из Redis накопленные за промежуток снимки, усредняет их (`averageValues`) и пишет одну точку в PG (`pgStore.InsertAveraged`). Прямой записи в PG из poller нет.
- **Redis хранит только последние 2 календарных суток**: окно = «сегодня» + «вчера», т.е. от 00:00 вчерашнего дня (`recentCutoff(now)`). Удалением старых точек занимается фоновый процесс `runRedisCleanup` → `redisStore.PurgeOld`: `ZRemRangeByScore` старше cutoff по каждому месячному сегменту, пустые сегменты удаляются. HASH `current` (последнее состояние) не трогается — хранится всегда.
- **PostgreSQL хранит всю историю как усреднённые 5-минутные точки** в таблице `sunreceiver.averages` (PK `(ip, ts)`, ts = начало 5-минутного промежутка). Усредняется каждый числовой тег `values` (для `ac_active_power` и пр. — среднее за окно). Точки старше двух календарных суток живут только здесь и никогда не удаляются.

#### Redis-хранилище (`redis_store.go`)
- Redis запускается **с persistent storage**: `redis-server --port 6379 --dir <data-dir> --save 900 1 --save 300 10 --appendonly yes` (RDB-снимки + AOF), данные живут в `<data-dir>` (используется `~/.cache/sunreceiver-redis`), поэтому не исчезают при перезапусках. Клиент — `github.com/redis/go-redis/v9`. При **полностью пустом** Redis при старте (например, очистили Redis) poller восстанавливает в нём данные из PostgreSQL (`restoreRedisFromPG` → `pg.Averages` за окно 2 календарных суток), после чего persist их сохраняет.
- Ключи:
  - **`sunreceiver:current`** — HASH текущих (последних) значений: поле=IP инвертора, значение=JSON `deviceSnapshot`. Один `HGETALL` отдаёт состояние всех инверторов — именно его читает дашборд для `/api/current`. Чистке старого не подлежит (хранится последнее состояние).
  - **`sunreceiver:series:<YYYY-MM>`** — временной ряд, месячный сегмент = ZSET: score=Unix (сек.), member=JSON `deviceSnapshot`. Чтение произвольного периода (`QuerySeries`) = `ZRANGEBYSCORE` по затронутым месяцам, отсортировано по времени. Хранятся данные за последние 2 календарных суток; старые удаляются `PurgeOld`/`runRedisCleanup`.
- Сохранение снимка одним циклом: `HSet(current, ip, snap)` + `ZAdd(series, epoch, snap)` (PIPELINE/TxPipeline). TTL на сегмент ~40 дней (запасной предохранитель; основную очистку делает `PurgeOld`).
- `recentCutoff(now)` — момент начала вторых из последних двух календарных суток (00:00 вчера в локальной зоне). Используется и очисткой Redis, и дашбордом для выбора источника.

#### PostgreSQL-хранилище (`pg_store.go`) и фоновые процессы (`accumulator.go`)
- Таблица `sunreceiver.averages(ip, name, ts, device_sn, values jsonb)`, PK `(ip, ts)`, индекс по `ts`. Вставка идемпотентна (`ON CONFLICT DO NOTHING`).
- `pgStore.InsertAveraged(ip, name, ts, deviceSN, vc)` — одна усреднённая точка (ts = начало 5-минутного промежутка).
- `pgStore.Averages(start, end)` — чтение усреднённых точек за период (для дашборда и реставрации Redis).
- `pgStore.MigrateLegacy()` — однократная конвертация старой таблицы сырых снимков `sunreceiver.snapshots` (существовала до 2026-09-07) в 5-минутные усреднённые точки `averages`, затем удаление `snapshots`. Идемпотентна: если таблицы нет — бездействует. Вызывается при старте после `openPG`.
- `runAccumulator(store, pg, stop)` — фоновый процесс усреднения: (1) `backfillAccumulator` конвертирует уже накопленные в Redis за 2 календарных суток снимки в PG (однократно при старте, чтобы не потерять промежутки до начала аккумуляции); (2) далее каждый час по завершении 5-минутного промежутка (граница `nextBoundary`/`floorToStep`, 12 раз в час) `averageBucket` читает из Redis снимки промежутка, усредняет и пишет в PG.
- `runRedisCleanup(store, stop)` — фоновый процесс очистки Redis: каждые 15 мин вызывает `PurgeOld` (удаление точек старше окна 2 календарных суток).
- `averageValues(snaps)` — усредняет все числовые теги снимков одного инвертора и одного 5-минутного промежутка (для `values` — только числовые общие теги; `ac_active_power` усредняется корректно, накопительные `energy_*` — как «репрезентативное» значение промежутка).

#### Веб-дашборд (`dashboard.go`)
- HTTP-сервер отдаёт HTML и JSON API. `/api/current` — `HGETALL sunreceiver:current` (последнее состояние). `/api/series` — временные ряды `ac_active_power` по инверторам, а также ряды МАП `map_grid_voltage`/`map_grid_power`/`map_battery_voltage`/`map_battery_power`.
- **Источник данных для рядов**: часть периода, попадающая в последние 2 календарных суток (от `recentCutoff`), читается из Redis (полное разрешение ~10 с); более старая часть читается из PostgreSQL (5-минутные средние). Реализовано в `dashboardHandler.loadRange`: если `pg != nil` и период начинается раньше cutoff — из PG берётся отрезок `[start, cutoff)`, из Redis — `[cutoff, end]`, результаты объединяются.
- Сам дашборд **не пишет** ни в Redis, ни в PG — только чтение. Запускается в poller по флагу `-dashboard`.
- **Периоды обновления на странице**: текущие параметры (KPI-плашки, `/api/current` и сводная таблица) обновляются каждую секунду; **графики** (`/api/series`) — раз в минуту, с кнопкой принудительного обновления («Обновить графики»). В сводной таблице добавлена строка «Актуально» — время последнего снимка каждого инвертора.
- **Плашки МАП (вверх)**: напряжение/мощность сети и напряжение/мощность батареи (из тегов `grid_voltage`/`grid_power`/`battery_voltage`/`battery_power` устройства kindMAP в `/api/current`). **Графики МАП**: «Напряжение сети, V» и «Мощность сети, W» (из `/api/series`).
- **Плашка счётчика (в самом верху, обновление 1 с)**: все текущие параметры DDS238 (`meter_voltage`/`meter_current`/`meter_active_power`/`meter_reactive_power`/`meter_power_factor`/`meter_frequency`/`meter_import`/`meter_export`/`meter_total`). Состав — массив `METER_PARAMS` в JS `dashboard.go`; знаковые мощности окрашиваются (отрицательная = отдача в сеть). Счётчик исключён из сводной таблицы инверторов и из «инверторов онлайн».
- При отключённом PG (`-pg ""`) дашборд отдаёт только данные из Redis в пределах окна удержания (старше 2 суток — пусто).
- **Страница электроэнергии `/energy`** (кнопка «Электроэнергия» на главной) — посуточные и помесячные тарифы DDS238 из `daily_tariffs` (после переноса со страницы графиков): график «по дням» (по умолч. текущий месяц) + график «по месяцам» (по умолч. текущий год). У каждого графика **независимый** диапазон (пресеты + свой «С/по»), между собой не синхронизируются. Данные — `/api/tariffs?from=&to=` (по умолч. текущий месяц), `pgStore.DailyTariffsRange` (финализированные дни в диапазоне).
- **Аномалии отключившихся устройств на графиках**: ряды устройств содержат только **реальные** снимки (`loadRange` ничего не достраивает). Чтобы отключившийся/замолчавший инвертор не «выглядел работающим»: (1) в хинте `drawCursorTooltip` ближайшая точка слева используется только если она не старше **20 минут** (`TOOLTIP_MAX_GAP`), иначе устройство в хинте пропускается; (2) в суммарном carry-forward `sumActive` вклад устройства тоже ограничен окном 20 минут (`staleWindow`) — замолкшая машина перестаёт суммироваться. МАП/счётчик ряды — только реальные точки.

### Универсальный контракт `values` (одинаков для Deye, Sofar, МАП/MPPT)
`values` хранит **только общие теги** (`commonContractTags` в `main.go`) с **одинаковыми именами и единицами измерения**; единица зашита в суффикс имени. Бренд-специфичные поля в файл **не пишутся**. Значения тегов `*voltage`/`*current`/`*power`/`energy*`/`temperature*` **округляются до 1 знака** после запятой (`round1`). Порядок тегов в файле фиксирован и **одинаков для обеих марок**. Полное описание каждого тега — в `docs/universal-contract.md`.
- **Общие (единственные в `values`):** `pv1/pv2_voltage` (V), `pv1/pv2_current` (A), `pv1/pv2_power` (W), `ac_active_power` (W), `ac_reactive_power` (**var**), `grid_frequency` (Hz), `l1/l2/l3_voltage` (V), `l1/l2/l3_current` (A), `energy_today` (**kWh**), `energy_total` (**kWh**), **плюс теги МАП/MPPT** (`grid_voltage`/`grid_power`/`battery_voltage`/`battery_power` — только у kindMAP) — см. секцию «МАП Титанатор…».
- **Sofar-only (не в файле, справочно):** `inverter_status` (string), `fault_1..5` ([]string), `country` (string), `pv1/pv2_power` (W), `temperature_module`/`temperature_inner` (C), `bus_voltage` (V), `time_total` (h), `time_today` (min), `insulation_*` (Ohm), `pv1_sample_cpu_*`, `countdown_time`, `alert`, `input_mode`, `comm_board_msg`.
- **Deye-only (не в файле, справочно):** `dc_total_power` (W), `ac_apparent_power` (W), `grid_l12/l23/l31_voltage` (V), `temperature_radiator`/`temperature_igbt` (C), `uptime` (min), `load_power` (W), `grid_power` (W), `energy_sold_today/total`, `energy_bought_today/total`, `energy_load_today/total` (kWh), `pv3/pv4_*` (на 2-цепных = 0).
- **Согласование единиц:** `energy_today`/`energy_total` — **kWh** у обеих (Sofar 0x0019 ×10 Wh → ×0.01 kWh, Sofar 0x0015/16 32-бит уже kWh; Deye 0x3C ×0.1 kWh, 0x3F/40 ×0.1 kWh). `ac_reactive_power` — **var** у обеих (Sofar 0x000D ×0.01 kVar → ×10 var; Deye 0x58 ×0.1 var). `ac_active_power`: Sofar 0x000C ×10 W; Deye 0x56/57 32-бит ×0.1 W (16-битный 0x50 «operating power» НЕ маппится в контракт — дубль). `grid_frequency`: Sofar 0x000E ×0.01 Hz, Deye 0x4F ×0.01 Hz.
- **Внимание 0x58 Deye:** kbialek помечает как «AC reactive power ×0.1» (raw 365 → 36.5 var, правдоподобно). ×10 даёт 3650 var — абсурд (P≈404 W, S≈356 VA); масштаб ×0.1 не переверифицирован документально, значения в var.
- **Данные, которых нет у обеих:** Deye не отдаёт статус/фолты/страну/изоляцию; Sofar не отдаёт apparent/reactive-load/energy-sold-bought. В `values` их нет вообще (в `values` только общие теги `commonContractTags`).
- `probe/main.go` — диагностический инструмент: `go run ./probe <ip> 8899 <sn hex32> [sn2...] <start hex> <count hex>` — строит Deye-кадр (`BuildDeyeReadFrame`) с каждым SN по очереди, шлёт, дробит ответ (`SplitFrames`), печатает регистры (`ParseModbusPDU`) и код Deye-ошибки (`DeyeErrorCode`, 0x05/0x06). Перебор unit-адресов — через env `PROBE_UNITS=1,2,...`. Собственных копий CRC/checksum/сборки кадра нет.
- UDP-слушатель и `received/` — старое решение, можно удалить `received/`.

## МАП Титанатор: MPPT («КЭС») — via HTTP read_json, батарея/сеть — via Modbus TCP

ПАК «Малина» (Raspberry Pi, 192.168.13.60) — веб-интерфейс Микроарт для МАП Титанатор (192.168.13.74, port **502**, unit 1) и до 16 параллельных **MPPT контроллеров** Микроарт (`(C)mART`). Один контроллер = одна цель/колонка дашборда.
- MPPT-контроллеры: источник данных — **веб-API ПАК «Малина»** `read_json.php?device=mppt` (HTTP Basic-auth; конфиг `base_url`/путь/логин/пароль — в разделе **`mppt`** sunReceiver.json, пароль в открытом виде, файл в git не выгружается). SSH-доступ к хосту ПАК для разработки — в `.kilo/malina-ssh.json` (в git не попадает, программой не используется). В `sunReceiver.json` MPPT **не регистрируются**: состав контроллеров определяется динамически по фактически подключённым к МАП/ПАК контроллерам из ответа API (каждый элемент массива = контроллер со своим `slot`). Контроллер появляется на дашборде, когда появляется в ответе, и исчезает, когда перестаёт отвечать (его ключ удаляется из `current` через `PruneMPPT`). Имя генерируется как `MPPT-<slot+1>`. Напрямую видны параметры панелей, которых нет в Modbus-гейте: Vc_PV, Ic_PV, P_PV, V_Bat, I_Ch, P_Out + `timestamp` актуальности данных. Реализация — `mppt_api.go` (`mpptSite`, `FetchMPPTs`, `mapMPPTAPI`) + динамический сбор в `pollAndSaveMap`, коннектор к `read_json.php` описан в `docs/read_json.md`.
- Раздел **`map`** в конфиге → `kindMAP` (Modbus TCP, `modbusmap/` + `mapClientFor`) — оставлен для **данных батареи и сети МАП на дашборд** (grid/battery). Цель `MAP (батарея/сеть)` (192.168.13.74, unit 1). Пер-слотовый MPPT через Modbus больше не опрашивается — MPPT переехали на HTTP.

**`values` углов МАП/MPPT (теги добавлены в общий контракт `commonContractTags`):**
- MPPT API (`kindMPPT`, `mapMPPTAPI`): `pv1_voltage/current/power` = Vc_PV/Ic_PV/P_PV панелей контроллера; `l1_voltage` = V_Bat (напряжение АКБ); `l1_current` = I_Ch (ток заряда); `ac_active_power` = P_Out (мощность на выходе контроллера); `energy_today` = Pwr_kW + Pwr_W/1000 (общий объём выработки за сутки, кВт·ч — строка «Выработка сегодня» таблицы). `timestamp` снимка = поле `timestamp` ответа API.
- MAP Modbus (`kindMAP`, `mapMAPRegisters`): `l1_voltage`=АКБ, `l1_current`=ток заряда, `ac_active_power`=V×I, `grid_frequency`=0, и **добавлено для дашборда**: `grid_voltage` (`_UNET` 0x422, 0 → нет сети, иначе +100 В), `grid_power` (`_PNET` 0x59A/0x59B, sign `_PNET_Sign_P` 0x587), `battery_voltage` (= `_UAcc_med` 0x405/0x406, `(VH*256+VL)/10`), `battery_power` (`_PLoad` 0x59E/0x59F, `((H*256+L)/8)*100`).
- PollDevice (case `kindMAP`) читает блоки `modbusmap`: 0x400 (0x20 слов = 0x400..0x43F: `_UAcc_med` 0x405/0x406, `_INET`/`_UNET` 0x422/0x423, `_IAcc_med` 0x432/0x433), 0x530 (`_I_Akb_MPPT`), 0x580 (`_PNET_Sign_P` 0x587, `_PNET` 0x59A/0x59B, `_PLoad` 0x59E/0x59F). Блок 0x420 отдельным чтением не запрашивается — это подмножество блока 0x400..0x43F (`_UNET` 0x422 читается с ним).
- **Ключ устройства в Redis/PG = `devKey(t)`**: для kindMPPT и kindMAP с slot — `IP#mppt<slot>` (напр. `192.168.13.60#mppt0`), чтобы контроллеры не сливались в одну колонку/ряд. `slot<0` у kindMAP = база батареи/сети (ключ = IP).
- **Кадр опроса МАП и MPPT API — 1 раз в секунду** (отдельный цикл `runMapPoll`, не в общем 10-сек `doPoll`: опрашивает `kindMAP`+`kindMPPT`), в Redis пишется через `SaveSnapshotWindow` (`redis_store.go`): score точки = начало 10-секундного окна (`ts.Truncate(10s)`; для MPPT — от `timestamp` ответа API), в пределах окна каждая новая запись **заменяет** предыдущую (ZREM+ZADD), поэтому по каждому устройству в Redis остаётся **ровно одна строка за каждые 10 секунд**. Аккумуляцией PG усредняется как у остальных (~1 точка за 10 с с дискретностью 5 мин).

## Электросчётчик DDS238 (мгновенные значения + посуточные тарифы)

Однофазный счётчик **DDS238** (192.168.13.77:502, Modbus TCP, holding registers, unit 1).
Конфигурация — отдельный файл **`dds238.json`** рядом с бинарником (как `sunReceiver.json`;
fallback в CWD при `go run .`), в git НЕ коммитится; шаблон — **`dds238.json.sample`** (git,
подставные значения), полное описание — `docs/dds238-meter.md`. Загрузка — `loadMeterConfig`
(`meter_config.go`); при отсутствии/неполноте полей опрос счётчика отключается.

**Две задачи:**
1. **Мгновенные значения** — как у МАП: отдельный **1-сек цикл** `runMeterPoll` (`meter_poller.go`),
   не в общем 10-сек `doPoll`. Читает регистры 0..26 (`read_holding_registers(0,27)`), пишет в Redis
   через `SaveSnapshotWindow` (одна строка за 10 с + актуальное current). В PG усредняется общим
   аккумулятором в `sunreceiver.averages` (5-минутные агрегаты). Теги `meter_*` добавлены в
   `commonContractTags` (иначе не сериализуются в `values`), поэтому счётчик НЕ попадает на графики
   мощности инверторов (`ac_active_power`) и суммарный ряд — только на собственную плашку/графики.
2. **Посуточные тарифы** — счётчик считает `Import`(потребление)/`Export`(отдача), но без разбивки
   «День/Ночь». Границы тарифной зоны — **константа** в коде: День 07:00–23:00, Ночь 23:00–07:00
   (`meterDayStartH`/`meterDayEndH` в `meter_tariff.go`). Каждую границу (00:00, 07:00, 23:00 лок.
   времени) опрос захватывает показание `Import`/`Export`, ближайшее к границе (окно ±5 с,
   `meterTariffCapture`, одна запись на всё время работы). Пишется в таблицу `sunreceiver.daily_tariffs`
   (`StoreMeterBoundary`); когда у дня есть все 4 граничных показания — день финализируется
   (`finalizeMeterDay`): `import_day=Imp(23)-Imp(07)`, `import_night=(Imp(07)-Imp(00))+(Imp(00 след.)-Imp(23))`,
   аналогично `export_*`. При отрицательной разности (сброс/замена счётчика) день не финализируется,
   показания сохраняются. Таблица создаётся из `ensureSchema` (`pg_store.go`) через
   `ensureMeterTariffSchema`. Посуточные значения — основа для статистики за месяц/год и графиков
   (дашборд тарифов — отдельная задача).
   - **Добор пропущенных границ** (`meter_backfill.go`, `runMeterBackfill`): если пулер был выключен/
     в перезапуске либо не было сети (счётчик молчал) — живой захват границы (±5 с) пропускается.
     Тогда фоновый процесс каждые 5 мин (и сразу на старте) проверяет границы за окно удержания
     Redis (последние 2 календарных суток), которые уже «безопасно в прошлом» (>10 мин назад) и
     не захвачены, и дописывает в `daily_tariffs` показание, **ближайшее** к границе среди
     зафиксированных в ряде Redis снимков (поиск в ±6 ч от границы, `nearestMeterReading`).
     Это и есть «ближайшее к точке из зафиксированного» по ТЗ. Для границ старше окна Redis
     (простой дольше ~2 суток) данных нет — такие границы не восстанавливаются.

**Регистры DDS238** (per dds238read.py): 0-1 Total / 8-9 Export / 10-11 Import — 32-бит `(hi*65536+lo)/100` kWh;
12 Voltage `/10` V; 13 Current `/100` A; 14 ActivePower `int16` W (отриц. = отдача в сеть);
15 ReactivePower `int16` var; 16 PF `/1000`; 17 Freq `/100` Hz. Проверено на живом счётчике
(2026-09-08): `Import+Export==Total`, мгновенные значения правдоподобны.

## Поведение живых логгеров (проверено 2026-09-03, важно)
- **Логгеры шлют данные МЕДЛЕННО и ПУТЬ (pacing)**: полный ответ (~300-400 байт) приходит частями на протяжении 15-30 секунд; при коротком read-deadline теряются кадры с данными (остаются heartbeat + placeholder). В клиенте — цикл чтения до «тишины» `IdleWindow` (4 с; для Sofar 8 с — у .76 паузы между кусками до ~6.5 с), первый байт ждётся до `Timeout` (15 с).
- **Sofar .76**: на любой запрос (func 03, 04, любой диапазон, любой SN, включая 0) возвращает ВЕСЬ блок 0x0000-0x0027 (40 регистров), плюс heartbeat-кадр (plen 16) и пустой placeholder-кадр (plen 99/137, data-область нулями). Данные в 1-2 PDU. CRC валидный. **Важно**: в одном ответе логгер шлёт полный блок (bytecount 80) И «дубль» — 16 регистров 0x0010-0x001F (bytecount 32) в отдельном кадре, и повторяет последовательность 2-3 раза за ~30 с. Слив всех PDU от базы 0 затирает 0x0000-0x000F (битые status/PV/частота) — в `main.go` берётся только САМАЯ БОЛЬШАЯ валидная PDU (полный блок от 0x0000). Сбойные кадры с валидным CRC (но мусором вместо регистров: частота ~220 Гц, пустая энергия) дают абсурдные пики мощности (напр. `ac_active_power = 220680 W` для 2.5 kVA инвертора) — отсеиваются `probableSofarBlock` по физическим порогам (частота 40-80 Гц, фазы ≤300 В, PV ≤450 В, активная/реактивная мощность ≤10 кВт) и минимальному размеру блока ≥20 регистров (чтобы не принять «дубль» 0x0010-0x001F за полный). Иногда (примерно каждый 3-й цикл) логгер не отвечает вовсе >15 с — это нормальная флейка, следующий цикл ок.
- **Deye .91/.70/.79/.92/.93**: отвечают данными ТОЛЬКО на Solarman-кадр с 15-байтным datafield и реальным SN даталоггера (~8 с на оба диапазона). При неверном SN — 29-байтный heartbeat с кодом 0x06, при 14-байтном datafield — код 0x05. Реализовано в `main.go`.
- PDU в payload — поиском `01 03 <vlen>`, а не по фиксированному смещению (padding/заголовки бывают разными).

## Находки по CRC (почему Sofar_LSW3.py несовместим с эталоном)
- `libscrc.modbus` в Sofar_LSW3.py — это стандартный CRC16-Modbus (init 0xFFFF, poly 0xA001 отражённый, без invert); моя `CRC16Modbus` в `solarman/frame.go` — то же самое, проверено: CRC всех PDU живых ответов сходятся при вычислении по `01 03 <vlen> <data>` (vlen = bytecount) и записи LE.
- В Sofar_LSW3.py CRC пишался high-first — это баг старой реализации, не воспроизводить.
- PayloadLength в запросе = длина payload (20 для read: `14 00` LE), НЕ `0x1700` как в Sofar_LSW3.py.
- Checksum кадра = `sum(bytes[1:len-2]) mod 256` — проверено на всех живых кадрах (match=True).
- Response serial u16 **big-endian**, request serial/len/control **little-endian**. Response control code = `10 15` (LE 0x1510).

## План реализации
1. ~~solarman/ пакет~~ — готово.
2. ~~poller 10s + JSON~~ — готово. Список целей — в `config.json` (все 6 инверторов: 5 Deye + 1 Sofar).
3. ~~Deye string: чтение регистров~~ — готово (BuildDeyeReadFrame + deyeRegMap + poller).
4. ~~Именование raw_registers~~ — готово (имена из SOFARMap.xml / kbialek string-группы; 32-битные — `_lo`/`_hi`; недокументированные — hex).
5. ~~Универсальный контракт значений~~ — готово: одинаковые имена тегов и единицы измерения для Deye и Sofar в `values` (см. «Универсальный контракт `values`»). Остальное по желанию: чтение настроек/др. диапазонов Deye, мониторинг microinverters, тесты.
6. ~~Redis-хранилище~~ — готово (`redis_store.go`): HASH `current` + месячные ZSET `series:<YYYY-MM>`, запись вместо JSON-файлов (файлы — по флагу `-file`), TTL на сегмент; Redis с persistence (RDB+AOF), при пустом Redis данные восстанавливаются из PG.
7. ~~Веб-дашборд текущих параметров~~ — готово (`dashboard.go`): HTML + `/api/current` из `HGETALL sunreceiver:current`, запускается в poller по флагу `-dashboard`.
8. ~~Redis — только последние 2 календарных суток + фоновая очистка~~ — готово (`accumulator.go`: `runRedisCleanup`/`redisStore.PurgeOld`, cutoff `recentCutoff`).
9. ~~PostgreSQL — только 5-минутные усреднённые точки~~ — готово (`pg_store.go`: таблица `averages`, `InsertAveraged`/`Averages`, миграция legacy `snapshots`→`averages`; запись из poller убрана, усреднение в фоне `runAccumulator`).
10. ~~Дашборд: <2 суток из Redis, старше — из PG~~ — готово (`dashboardHandler.loadRange`).
11. ~~МАП Титанатор («КЭС»): Modbus TCP (батарея/сеть) + MPPT через веб-API ПАК «Малина»~~ — готово (см. секцию «МАП Титанатор…»): тип `map` — МАП по Modbus TCP (192.168.13.74, unit 1) для данных батареи/сети на дашборд; тип `mppt` — контроллеры MPPT через read_json.php ПАК «Малина» (192.168.13.60). PV с контроллеров MPPT — через веб-API (источник ПАК «Малина»), на дашборд в значениях МАП.
12. ~~Электросчётчик DDS238~~ — готово (см. секцию «Электросчётчик DDS238…»): непрерывный опрос (1 с), мгновенные значения `meter_*` в Redis+PG, посуточные тарифы в `sunreceiver.daily_tariffs`, добор пропущенных границ из Redis.

## Запуск и эксплуатация
- **Прод-процесс** (собранный бинарник, непрерывно): `go build -o sunReceiver .` затем `./sunReceiver` как **persistent-фоновый процесс** (в Kilo — `background_process` с `persistent: true`, чтоб переживал сессии/завершение). Дашборд слушает `:8080`, счётчик и инверторы опрашиваются постоянно. Для теста — `go run .` (fallback конфигов в CWD).
- Локальный пурлер работает от конфига **`sunReceiver.json`** рядом с бинарником (все разделы: invertors, map, mppt, db, meter — в одном файле; файл приватный, в git не попадает). При `go run .` fallback в CWD.
- **При перезапуске после правок кода**: остановить старый persistent-процесс (kill PID старого `sunReceiver`), пересобрать бинарник, запустить заново как persistent.

## Окружение
- Репо: github.com/galiy/sunReceiver (remote git@github.com:galiy/sunReceiver.git, branch main).
- macOS, Go 1.26.5. `nc` доступен для быстрых проверок TCP. `timeout` в zsh нет — запускать через background_process или `&`.
- Эталонная библиотека (не в vendor, только для справки): `~/go/pkg/mod/github.com/snowirbis/solarman@v1.0.4/` (frame.go — формат кадра, read.go — payload).
