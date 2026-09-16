# Протокол Solarman V5 — результаты реверса

> Реверс-инжиниринг WiFi-даталоггеров (Solarman LSW-3/LSE, TCP 8899) по эталонной
> библиотеке `github.com/snowirbis/solarman v1.0.4` (проверено на живых логгерах
> 2026-09-03). Исходная реализация `Sofar_LSW3.py` **устарела** — её выводы по
> длинам/адресам/CRC отличаются от реального эталона, см. «Находки по CRC» ниже.

## Формат кадра

Запрос (read holding registers):

```
A5 | PayloadLen u16 LE | Control 10 45 (LE 0x4510) | Serial u16 LE | DeviceSN u32 LE | Payload | Checksum u8 | 15
```

- `PayloadLen` — длина payload (**НЕ** `0x1700` как в Sofar_LSW3.py). Для read:
  12 (заголовок) + 6 (Modbus PDU) + 2 (CRC) = **20** → `14 00` LE.
- Заголовок payload (12 байт): `02` (FrameType) + `0000` (SensorType u16 LE) +
  `00000000` (DeliveryTime u32 LE) + `00000000` (PowerOnTime u32 LE) +
  `00000000` (OffsetTime u32 LE).
- Modbus PDU (6 байт): `01` (адрес устройства) `03` (func) | StartReg u16 **BE** |
  Count u16 **BE**.
- CRC16-Modbus (init 0xFFFF, poly 0xA001 отражённый, без invert — стандартный) по
  6 байтам PDU, пишется **little-endian** (low byte первым). Sofar_LSW3.py пишет
  high-first — это баг старой реализации.
- Checksum = `sum(frame[1 : len-2]) mod 256` — сумма всех байтов от 2-го до
  предпоследнего (не включая сам checksum-байт и end-маркер). В эталоне:
  `calcCheckSum8(buf.Bytes()[1:])`, где buf — кадр без end-маркера.
- `DeviceSN` в запросе: Sofar отвечает и при SN=0, Deye тоже отвечает heartbeat'ом
  при SN=0. Для Sofar достаточно SN=0. Реальный SN логгера виден в ответах
  (поле после serial'а, LE u32).

Ответ: те же маркеры, control в ответе = `15 10` (LE 0x1510), serial u16
**big-endian** в ответе. Длина кадра = **11 + PayloadLen + 2**. Кадров может быть
несколько подряд в одном TCP-ответе (у Sofar .76 их 3: heartbeat + placeholder +
данные; у Deye — один heartbeat). Request serial/len/control — **little-endian**,
response serial — **big-endian**.

## Datafield-заголовок: Sofar (12 байт) vs Deye (15 байт)

- **Sofar** — 12-байтный заголовок payload (см. выше). Реализован как
  `BuildReadFrame`.
- **Deye** (LSE, rebrand Solarman) — **15-байтный** datafield-заголовок:
  `02` + 14 нулей (`02000000 00000000 00000000 0000`). PayloadLen = 15 + (6 для PDU
  + 2 CRC) = **23** (`17 00` LE). Если слать 14-байтный заголовок — логгер отвечает
  кодом 0x05. Реализован как `BuildDeyeReadFrame`.

## Структура ответа с данными (Sofar .76)

Ответ — 3-4 кадра: heartbeat (payload 16), placeholder (payload 99/137, data-область
нулями) и 1-2 кадра с данными. Кадр с данными: внутри:

- 0..13 — заголовок payload (frameType 02, status 01, deliveryTime, powerOnTime,
  offsetTime);
- затем (после возможного padding) Modbus-ответ: `01 03` | ByteCount u8 | данные
  (2 байта BE на регистр) | CRC16 LE (2 байта);
- Sofar LSW-3 отвечает ВЕСЬ блок 0x0000-0x0027 (bytecount 80 = 40 регистров)
  независимо от запрошенного диапазона. Парсить нужно по ByteCount.

PDU в payload — поиск `01 03 <vlen>`, а не по фиксированному смещению
(padding/заголовки бывают разными).

## Коды ошибок логгера (29-байтный heartbeat, payload[14])

- **0x05** — "Modbus device address does not match" (неверный unit / 14-байтный
  datafield для Deye).
- **0x06** — "Logger Serial Number does not match" (неверный SN даталоггера).

Проверено: inverter SN (напр. 2405018274 для .70) даёт 0x06, logger SN проходил и
данные читались.

## Поведение живых логгеров

- **Логгеры шлют данные МЕДЛЕННО и ПУТЬ (pacing)**: полный ответ (~300-400 байт)
  приходит частями на протяжении 15-30 с; при коротком read-deadline теряются кадры
  с данными (остаются heartbeat + placeholder). В клиенте — цикл чтения до «тишины»
  `IdleWindow` (4 с; для Sofar 8 с — у .76 паузы между кусками до ~6.5 с), первый
  байт ждётся до `Timeout` (15 с).
- **Sofar .76**: на любой запрос (func 03, 04, любой диапазон, любой SN, включая 0)
  возвращает ВЕСЬ блок 0x0000-0x0027 (40 регистров), плюс heartbeat-кадр (plen 16)
  и пустой placeholder-кадр (plen 99/137, data-область нулями). Данные в 1-2 PDU,
  CRC валидный. В одном ответе логгер шлёт полный блок (bytecount 80) И «дубль» —
  16 регистров 0x0010-0x001F (bytecount 32) в отдельном кадре, и повторяет
  последовательность 2-3 раза за ~30 с. Слив всех PDU от базы 0 затирает
  0x0000-0x000F (битые status/PV/частота) — в `main.go` берётся только САМАЯ
  БОЛЬШАЯ валидная PDU (полный блок от 0x0000). Сбойные кадры с валидным CRC (но
  мусором вместо регистров: частота ~220 Гц, пустая энергия) дают абсурдные пики
  мощности (напр. `ac_active_power = 220680 W` для 2.5 kVA инвертора) — отсеиваются
  `probableSofarBlock` по физическим порогам (частота 40-80 Гц, фазы ≤300 В,
  PV ≤450 В, активная/реактивная мощность ≤10 кВт) и минимальному размеру блока
  ≥20 регистров (чтобы не принять «дубль» 0x0010-0x001F за полный). Иногда (примерно
  каждый 3-й цикл) логгер не отвечает вовсе >15 с — это нормальная флейка, следующий
  цикл ок. Проверено 2026-09-07: 12-байтный `ReadRegisters`/`BuildReadFrame` Sofar
  LSW-3 отвечает только heartbeat с кодом 0x05; на 15-байтный кадр — полный блок
  0x0000-0x0027.
- **Deye .91/.70/.79/.92/.93**: отвечают данными ТОЛЬКО на Solarman-кадр с
  15-байтным datafield и реальным SN даталоггера (~8 с на оба диапазона). При
  неверном SN — 29-байтный heartbeat с кодом 0x06, при 14-байтном datafield —
  код 0x05. Логгеры отвечают стабильно и быстро.

## Находки по CRC (почему Sofar_LSW3.py несовместим с эталоном)

- `libscrc.modbus` в Sofar_LSW3.py — стандартный CRC16-Modbus (init 0xFFFF,
  poly 0xA001 отражённый, без invert); наш `CRC16Modbus` в `solarman/frame.go` —
  то же самое. Проверено: CRC всех PDU живых ответов сходятся при вычислении по
  `01 03 <vlen> <data>` (vlen = bytecount) и записи LE.
- В Sofar_LSW3.py CRC писался high-first — баг старой реализации, не воспроизводить.
- PayloadLength в запросе = длина payload (20 для read: `14 00` LE), НЕ `0x1700`
  как в Sofar_LSW3.py.
- Checksum кадра = `sum(bytes[1:len-2]) mod 256` — проверено на всех живых кадрах
  (match=True).
- Response serial u16 **big-endian**, request serial/len/control **little-endian**.
  Response control code = `10 15` (LE 0x1510).