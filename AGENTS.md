# sunReceiver

Go-приложение, которое опрашивает solar-инверторы через их WiFi-даталоггеры (Solarman LSW-3/LSE, порт 8899, TCP) и сохраняет распарсенные данные в Redis (in-memory, без persistent storage) + опционально в JSON-файлы. Включает веб-дашборд текущих параметров.

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

Список опрашиваемых инверторов задаётся в **`config.json` рядом с исполняемым файлом** (`os.Executable()`; при `go run .` — fallback в CWD): `{"targets": [{"ip", "name", "type": "deye"|"sofar", "logger_sn": uint32}]}`. `name` — логическое имя инвертора (обязательно). Опрос раз в 10 секунд, парсинг и сохранение в JSON.

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
- `main.go` — poller: каждые 10 с параллельно (goroutine) TCP-опрос целей из `config.json` (порт 8899). Список целей читается из **`config.json` рядом с бинарником** (`os.Executable()`; при `go run .` — fallback в CWD) через `loadConfig` в `targets []invTarget` {IP, Name, LoggerSN, Kind}: `type` "deye"→`kindDeyeString`, "sofar"→`kindSofar`. `name` — **логическое имя** инвертора (обязательное поле, напр. `Deye Left`, `Sofar-2.5`, `Bineos Right`). Deye — реальный SN даталоггера (`ReadRegistersDeye`), Sofar — `ReadRegisters` (отвечает и при реальном SN). Маппинг регистров: Sofar — в `mapSofarRegisters` (регистрации 0x0000-0x0027), Deye string — в `deyeRegMap`/`mapDeyeRegisters` (у каждого сенсора `Tag` — имя контракта в values). По умолчанию результаты сохраняются **в Redis** (`redis_store.go`); запись JSON-файлов в `data/` — **только по флагу `-file`** (код вынесен в `writeFiles`). Флаги CLI: `-redis <addr>` (по умолч. `127.0.0.1:6379`), `-dashboard <addr>` (по умолч. `:8080`, пустая строка выключает), `-file`. Веб-дашборд — `dashboard.go`. Снимок (структура `deviceSnapshot`: **`{name, ip, timestamp, device_sn, values}`**) отправляется только при успешном чтении данных (`HasData`); при `heartbeat_only`/`no data`/ошибке не сохраняется. `name`/`ip` — из config.json; `values` — **универсальный контракт** (см. ниже). **`raw_registers` в снимок НЕ пишется** — только 15 общих тегов.

### Redis-хранилище (`redis_store.go`) и веб-дашборд (`dashboard.go`)
- Redis запускается **без persistent storage**: `redis-server --save "" --appendonly no` (in-memory only). Клиент — `github.com/redis/go-redis/v9`.
- Ключи:
  - **`sunreceiver:current`** — HASH текущих (последних) значений: поле=IP инвертора, значение=JSON `deviceSnapshot`. Один `HGETALL` отдаёт состояние всех инверторов — именно его читает дашборд.
  - **`sunreceiver:series:<YYYY-MM>`** — временной ряд, месячный сегмент = ZSET: score=Unix (сек.), member=JSON `deviceSnapshot`. Чтение произвольного периода (`QuerySeries`) = `ZRANGEBYSCORE` по затронутым месяцам, отсортировано по времени — эффективное чтение всего набора за период. На активный сегмент ставится TTL ~40 дней.
- Сохранение снимка одним циклом: `HSet(current, ip, snap)` + `ZAdd(series, epoch, snap)` (PIPELINE/TxPipeline).
- Дашборд: HTTP-сервер читает `HGETALL sunreceiver:current` и отдаёт JSON по `/api/current`. Сам дашборд **не пишет в Redis** — только репликация/запуск через `-dashboard` флаг в poller (см. ниже). Для старого UI (движок `github.com/badafit/gohttpmetrics`) вхождение в `go.mod` не требуется.

### Универсальный контракт `values` (одинаков для Deye и Sofar)
`values` хранит **только общие для обеих марок теги** (15 штук, `commonContractTags` в `main.go`) с **одинаковыми именами и единицами измерения**; единица зашита в суффикс имени. Бренд-специфичные поля в файл **не пишутся**. Значения тегов `*voltage`/`*current`/`*power`/`energy*`/`temperature*` **округляются до 1 знака** после запятой (`round1`). Порядок тегов в файле фиксирован и **одинаков для обеих марок**. Полное описание каждого тега — в `docs/universal-contract.md`.
- **Общие (единственные в `values`):** `pv1/pv2_voltage` (V), `pv1/pv2_current` (A), `ac_active_power` (W), `ac_reactive_power` (**var**), `grid_frequency` (Hz), `l1/l2/l3_voltage` (V), `l1/l2/l3_current` (A), `energy_today` (**kWh**), `energy_total` (**kWh**).
- **Sofar-only (не в файле, справочно):** `inverter_status` (string), `fault_1..5` ([]string), `country` (string), `pv1/pv2_power` (W), `temperature_module`/`temperature_inner` (C), `bus_voltage` (V), `time_total` (h), `time_today` (min), `insulation_*` (Ohm), `pv1_sample_cpu_*`, `countdown_time`, `alert`, `input_mode`, `comm_board_msg`.
- **Deye-only (не в файле, справочно):** `dc_total_power` (W), `ac_apparent_power` (W), `grid_l12/l23/l31_voltage` (V), `temperature_radiator`/`temperature_igbt` (C), `uptime` (min), `load_power` (W), `grid_power` (W), `energy_sold_today/total`, `energy_bought_today/total`, `energy_load_today/total` (kWh), `pv3/pv4_*` (на 2-цепных = 0).
- **Согласование единиц:** `energy_today`/`energy_total` — **kWh** у обеих (Sofar 0x0019 ×10 Wh → ×0.01 kWh, Sofar 0x0015/16 32-бит уже kWh; Deye 0x3C ×0.1 kWh, 0x3F/40 ×0.1 kWh). `ac_reactive_power` — **var** у обеих (Sofar 0x000D ×0.01 kVar → ×10 var; Deye 0x58 ×0.1 var). `ac_active_power`: Sofar 0x000C ×10 W; Deye 0x56/57 32-бит ×0.1 W (16-битный 0x50 «operating power» НЕ маппится в контракт — дубль). `grid_frequency`: Sofar 0x000E ×0.01 Hz, Deye 0x4F ×0.01 Hz.
- **Внимание 0x58 Deye:** kbialek помечает как «AC reactive power ×0.1» (raw 365 → 36.5 var, правдоподобно). ×10 даёт 3650 var — абсурд (P≈404 W, S≈356 VA); масштаб ×0.1 не переверифицирован документально, значения в var.
- **Данные, которых нет у обеих:** Deye не отдаёт статус/фолты/страну/изоляцию; Sofar не отдаёт apparent/reactive-load/energy-sold-bought. В `values` их нет вообще (в `values` только 15 общих тегов).
- `probe/main.go` — диагностический инструмент: `go run ./probe <ip> 8899 <sn hex32> [sn2...] <start hex> <count hex>` — строит Deye-кадр (`BuildDeyeReadFrame`) с каждым SN по очереди, шлёт, дробит ответ (`SplitFrames`), печатает регистры (`ParseModbusPDU`) и код Deye-ошибки (`DeyeErrorCode`, 0x05/0x06). Перебор unit-адресов — через env `PROBE_UNITS=1,2,...`. Собственных копий CRC/checksum/сборки кадра нет.
- UDP-слушатель и `received/` — старое решение, можно удалить `received/`.

## Поведение живых логгеров (проверено 2026-09-03, важно)
- **Логгеры шлют данные МЕДЛЕННО и ПУТЬ (pacing)**: полный ответ (~300-400 байт) приходит частями на протяжении 15-30 секунд; при коротком read-deadline теряются кадры с данными (остаются heartbeat + placeholder). В клиенте — цикл чтения до «тишины» `IdleWindow` (4 с; для Sofar 8 с — у .76 паузы между кусками до ~6.5 с), первый байт ждётся до `Timeout` (15 с).
- **Sofar .76**: на любой запрос (func 03, 04, любой диапазон, любой SN, включая 0) возвращает ВЕСЬ блок 0x0000-0x0027 (40 регистров), плюс heartbeat-кадр (plen 16) и пустой placeholder-кадр (plen 99/137, data-область нулями). Данные в 1-2 PDU. CRC валидный. **Важно**: в одном ответе логгер шлёт полный блок (bytecount 80) И «дубль» — 16 регистров 0x0010-0x001F (bytecount 32) в отдельном кадре, и повторяет последовательность 2-3 раза за ~30 с. Слив всех PDU от базы 0 затирает 0x0000-0x000F (битые status/PV/частота) — в `main.go` берётся только САМАЯ БОЛЬШАЯ валидная PDU (полный блок от 0x0000). Иногда (примерно каждый 3-й цикл) логгер не отвечает вовсе >15 с — это нормальная флейка, следующий цикл ок.
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
6. ~~Redis-хранилище без persistent storage~~ — готово (`redis_store.go`): HASH `current` + месячные ZSET `series:<YYYY-MM>`, запись вместо JSON-файлов (файлы — по флагу `-file`), TTL на сегмент.
7. ~~Веб-дашборд текущих параметров~~ — готово (`dashboard.go`): HTML + `/api/current` из `HGETALL sunreceiver:current`, запускается в poller по флагу `-dashboard`.

## Окружение
- Репо: github.com/galiy/sunReceiver (remote git@github.com:galiy/sunReceiver.git, branch main).
- macOS, Go 1.26.5. `nc` доступен для быстрых проверок TCP. `timeout` в zsh нет — запускать через background_process или `&`.
- Эталонная библиотека (не в vendor, только для справки): `~/go/pkg/mod/github.com/snowirbis/solarman@v1.0.4/` (frame.go — формат кадра, read.go — payload).
