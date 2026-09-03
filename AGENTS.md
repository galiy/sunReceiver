# sunReceiver

Go-приложение, которое опрашивает solar-инверторы через их WiFi-даталоггеры (Solarman LSW-3/LSE, порт 8899, TCP) и сохраняет распарсенные данные в JSON.

Язык/команды: Go 1.26, `go run .` — запуск, `go vet ./...` — проверки. Коммиты писать по-русски, как в истории репо.

## Устройство: даталоггеры, адреса, модели

| IP | Модель | Статус опроса (проверено 2026-09-03) |
|---|---|---|
| 192.168.13.76 | **Sofar K-TLX** (LSW-3) | РАБОТАЕТ: отвечает 205-380 байт (3-4 кадра), внутри — Modbus-ответ func 03 со ВСЕМИ 40 регистрами 0x0000-0x0027 (bytecount 80, CRC валидный) + дубль 16 регистров в отдельном кадре. SN логгера = `95b4d768`. |
| 192.168.13.91 | Deye (string) | РАБОТАЕТ: отдаёт данные по Solarman V5-кадру с 15-байтным datafield и реальным SN логгера. LoggerSN = **1774265353** (`69c12409`). |
| 192.168.13.70 | Deye (string) | То же, что .91. LoggerSN = **2947602822** (`afb0d986`). |

Требование: опрашивать **192.168.13.91, .70** (Deye) и **.76** (Sofar) раз в 10 секунд, парсить и сохранять в JSON.

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

### Deye (.91, .70) — как читать (решено 2026-09-03)
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
- 0x6D/0x6E PV1 V/I ×0.1, 0x6F/0x70 PV2 V/I ×0.1
- 0xC6-0xC7 Load power (32, signed) ×1 W, 0xC8 Daily load ×0.01 kWh, 0xC9-0xCA Total load (32) ×0.1 kWh
- 0xCB-0xCC Grid power (32) ×1 W, 0xCD Daily sold ×0.01 kWh, 0xCE-0xCF Total sold (32) ×0.1 kWh, 0xD0 Daily bought ×0.01 kWh, 0xD1-0xD2 Total bought (32) ×0.1 kWh
Регистры 0x005B IGBT temp не подключён (0 регистр → −100). Логгеры отвечают стабильно и быстро (~8 с на оба диапазона).

## Текущее состояние кода
- `solarman/` — пакет-клиент Solarman V5: `BuildReadFrame` (12-байтный datafield, для Sofar), `BuildDeyeReadFrame` (15-байтный datafield + реальный SN, для Deye), `SplitFrames` (длина кадра = **11** + PayloadLen + 2, префикс A5+len+control+serial+SN), `ParseModbusPDU` (01 03/04 | bytecount | data | crc16 LE), `Checksum8` (sum[1:len-2] mod 256), `CRC16Modbus` (стандартный, в PDU пишется LE).
- `main.go` — poller: каждые 10 с параллельно (goroutine) TCP-опрос 192.168.13.91, .70 и .76 (порт 8899). Целевые инверторы — в `targets []invTarget` {IP, LoggerSN, Kind}: Deye требуют реальный SN даталоггера (`ReadRegistersDeye`), Sofar — SN=0 (`ReadRegisters`). Маппинг регистров Sofar в `regMap`, Deye string в `deyeRegMap` (см. выше). JSON: на каждый инвертор отдельный файл `data/<YYYY-MM-DD>/<IP>-<HHMMSS>.json` (дата в директории, время опроса и IP — в имени файла; внутри IP нет). Файл пишется только при успешном чтении данных (`HasData`); при `heartbeat_only`/`no data`/ошибке не сохраняется. Структура: `{timestamp, device_sn, values, raw_registers}`.
- `probe/main.go` — диагностический инструмент: `go run ./probe <ip> 8899 <sn hex32> [sn2...] <start hex> <count hex>` — строит Deye-кадр (`BuildDeyeReadFrame`) с каждым SN по очереди, шлёт, дробит ответ (`SplitFrames`), печатает регистры (`ParseModbusPDU`) и код Deye-ошибки (`DeyeErrorCode`, 0x05/0x06). Перебор unit-адресов — через env `PROBE_UNITS=1,2,...`. Собственных копий CRC/checksum/сборки кадра нет.
- UDP-слушатель и `received/` — старое решение, можно удалить `received/`.

## Поведение живых логгеров (проверено 2026-09-03, важно)
- **Логгеры шлют данные МЕДЛЕННО и ПУТЬ (pacing)**: полный ответ (~300-400 байт) приходит частями на протяжении 15-30 секунд; при коротком read-deadline теряются кадры с данными (остаются heartbeat + placeholder). В клиенте — цикл чтения до «тишины» 4 с, общий дедлайн 15 с.
- **Sofar .76**: на любой запрос (func 03, 04, любой диапазон, любой SN, включая 0) возвращает ВЕСЬ блок 0x0000-0x0027 (40 регистров), плюс heartbeat-кадр (plen 16) и пустой placeholder-кадр (plen 99/137, data-область нулями). Данные в 1-2 PDU. CRC валидный.
- **Deye .91/.70**: отвечают данными ТОЛЬКО на Solarman-кадр с 15-байтным datafield и реальным SN даталоггера (~8 с на оба диапазона). При неверном SN — 29-байтный heartbeat с кодом 0x06, при 14-байтном datafield — код 0x05. Реализовано в `main.go`.
- PDU в payload — поиском `01 03 <vlen>`, а не по фиксированному смещению (padding/заголовки бывают разными).

## Находки по CRC (почему Sofar_LSW3.py несовместим с эталоном)
- `libscrc.modbus` в Sofar_LSW3.py — это стандартный CRC16-Modbus (init 0xFFFF, poly 0xA001 отражённый, без invert); моя `CRC16Modbus` в `solarman/frame.go` — то же самое, проверено: CRC всех PDU живых ответов сходятся при вычислении по `01 03 <vlen> <data>` (vlen = bytecount) и записи LE.
- В Sofar_LSW3.py CRC пишался high-first — это баг старой реализации, не воспроизводить.
- PayloadLength в запросе = длина payload (20 для read: `14 00` LE), НЕ `0x1700` как в Sofar_LSW3.py.
- Checksum кадра = `sum(bytes[1:len-2]) mod 256` — проверено на всех живых кадрах (match=True).
- Response serial u16 **big-endian**, request serial/len/control **little-endian**. Response control code = `10 15` (LE 0x1510).

## План реализации
1. ~~solarman/ пакет~~ — готово.
2. ~~poller 10s + JSON~~ — готово. `targets` включает все три инвертора.
3. ~~Deye string: чтение регистров~~ — готово (BuildDeyeReadFrame + deyeRegMap + poller).
4. Дальнейшее (по желанию): чтение настроек/др. диапазонов Deye, мониторинг microinverters, тесты.

## Окружение
- Репо: github.com/galiy/sunReceiver (remote git@github.com:galiy/sunReceiver.git, branch main).
- macOS, Go 1.26.5. `nc` доступен для быстрых проверок TCP. `timeout` в zsh нет — запускать через background_process или `&`.
- Эталонная библиотека (не в vendor, только для справки): `~/go/pkg/mod/github.com/snowirbis/solarman@v1.0.4/` (frame.go — формат кадра, read.go — payload).
