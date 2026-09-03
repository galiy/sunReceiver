# sunReceiver

Go-приложение, которое опрашивает solar-инверторы через их WiFi-даталоггеры (Solarman LSW-3/LSE, порт 8899, TCP) и сохраняет распарсенные данные в JSON.

Язык/команды: Go 1.26, `go run .` — запуск, `go vet ./...` — проверки. Коммиты писать по-русски, как в истории репо.

## Устройство: даталоггеры, адреса, модели

| IP | Модель | Статус опроса (проверено 2026-09-03) |
|---|---|---|
| 192.168.13.76 | **Sofar K-TLX** (LSW-3) | РАБОТАЕТ: отвечает 205-380 байт (3-4 кадра), внутри — Modbus-ответ func 03 со ВСЕМИ 40 регистрами 0x0000-0x0027 (bytecount 80, CRC валидный) + дубль 16 регистров в отдельном кадре. SN логгера = `95b4d768`. |
| 192.168.13.91 | Deye | Возвращает только heartbeat-кадр 29 байт (control 0x1510, payload 16: frameType 02, status 01, uptime, SN, счётчики, 2×00). Данные регистров НЕ приходят. SN = `0924c169`. |
| 192.168.13.70 | Deye | То же, что .91. SN = `86d9b0af`. |

Требование: опрашивать **192.168.13.91 и 192.168.13.70** (Deye) раз в 10 секунд, парсить и сохранять в JSON. Sofar на .76 — эталон для отладки протокола. UDP-слушатель удалён (это было старое решение, не работает: даталоггеры не шлют push-датаграммы по UDP).

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

### Deye (.91, .70) — НЕ РАБОТАЕТ по классике
На любой запрос (func 03/04, любой SN, любой диапазон) возвращают только heartbeat-кадр 29 байт. Payload heartbeat: `02 01 <uptime u32 LE> <counter u32 LE> <SN u32 LE> 06 00` — похоже на Deye-логгер с другой политикой локального режима (возможно, требует другой control code или handshake-кадр первым, либо локальный modbus отключён и работает только push на cloud). Не решать это перебором — для Deye нужен отдельный ресивер их push-телеметрии или отдельный research.

## Текущее состояние кода
- `solarman/` — пакет-клиент Solarman V5: `BuildReadFrame`, `SplitFrames` (длина кадра = **11** + PayloadLen + 2, префикс A5+len+control+serial+SN), `ParseModbusPDU` (01 03/04 | bytecount | data | crc16 LE), `Checksum8` (sum[1:len-2] mod 256), `CRC16Modbus` (стандартный, в PDU пишется LE).
- `main.go` — poller: каждые 10 с параллельно (goroutine) TCP-опрос 192.168.13.91 и 192.168.13.70 (порт 8899, SN=0 — логгеры не требуют свой SN в запросе). Выбор PDU: первый с CRC=CRCCalc и >=40 регистров, иначе самый большой. JSON: `data/<YYYY-MM-DD>/<HHMMSS>.json` с `{timestamp, devices: {ip: {ok, device_sn, frames, register_data, heartbeat, values, raw_registers}}}`.
- `probe/main.go` — диагностический инструмент: `go run ./probe <ip> 8899 <sn hex> <start hex> <count hex>` — строит кадр, шлёт, дробит ответ, печатает регистры.
- UDP-слушатель и `received/` — старое решение, можно удалить `received/`.

## Поведение живых логгеров (проверено 2026-09-03, важно)
- **Логгеры шлют данные МЕДЛЕННО и ПУТЬ (pacing)**: полный ответ 40 регистров (~300-400 байт) приходит частями на протяжении 15-30 секунд; при коротком read-deadline теряются кадры с данными (остаются heartbeat + placeholder). В клиенте — цикл чтения до «тишины» 4 с, общий дедлайн 15 с.
- **Sofar .76**: на любой запрос (func 03, 04, любой диапазон, любой SN, включая 0) возвращает ВЕСЬ блок 0x0000-0x0027 (40 регистров), плюс heartbeat-кадр (plen 16) и пустой placeholder-кадр (plen 99/137, data-область нулями). Данные приходят в 1-2 PDU: первый — 40 регистров (bytecount 80), второй (в отдельном кадре plen 51) — дубль регистров 0x0010-0x001F (bytecount 32). CRC в PDU — валидный Modbus.
- PDU лежит в payload на offset **14** (после 14-байтного заголовка), но padding перед PDU бывает больше — парсить нужно поиском `01 03 <vlen>`, а не по фиксированному смещению.
- **Deye .91/.70**: на любой запрос только heartbeat-кадр (29 байт, payload 16: `02 01 <uptime u32 LE> <cnt u32 LE> <SN u32 LE> 06 00`). Данные регистров локальным modbus не отдаются — это поведение Deye-логгера (push только на cloud). Poller пишет для них `register_data: false` + heartbeat.

## Находки по CRC (почему Sofar_LSW3.py несовместим с эталоном)
- `libscrc.modbus` в Sofar_LSW3.py — это стандартный CRC16-Modbus (init 0xFFFF, poly 0xA001 отражённый, без invert); моя `CRC16Modbus` в `solarman/frame.go` — то же самое, проверено: CRC всех PDU живых ответов сходятся при вычислении по `01 03 <vlen> <data>` (vlen = bytecount) и записи LE.
- В Sofar_LSW3.py CRC пишался high-first — это баг старой реализации, не воспроизводить.
- PayloadLength в запросе = длина payload (20 для read: `14 00` LE), НЕ `0x1700` как в Sofar_LSW3.py.
- Checksum кадра = `sum(bytes[1:len-2]) mod 256` — проверено на всех живых кадрах (match=True).
- Response serial u16 **big-endian**, request serial/len/control **little-endian**. Response control code = `10 15` (LE 0x1510).

## План реализации
1. ~~solarman/ пакет~~ — готово.
2. ~~poller 10s + JSON~~ — готово (heartbeat_only для Deye).
3. Дальнейшее: реальный мониторинг Deye возможен только через их push-телеметрию (отдельный research) или другой local-mode. Для Sofar `.76` парсер уже готов — достаточно добавить адрес в `targets` в main.go.

## Окружение
- Репо: github.com/galiy/sunReceiver (remote git@github.com:galiy/sunReceiver.git, branch main).
- macOS, Go 1.26.5. `nc` доступен для быстрых проверок TCP. `timeout` в zsh нет — запускать через background_process или `&`.
- Эталонная библиотека (не в vendor, только для справки): `~/go/pkg/mod/github.com/snowirbis/solarman@v1.0.4/` (frame.go — формат кадра, read.go — payload).
