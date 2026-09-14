# ANT BMS — формат live-кадра телеметрии (заголовок `AA 55 AA FF`)

Спецификация классического кадра телеметрии ANT BMS, передаваемого по UART
(тот самый 140-байтный кадр с заголовком `AA 55 AA FF` в начале). Соответствует
наблюдаемому в дампе нашей платы 22PHB-TB-8-22S-240A (16S).

Первоисточник — `klotztech/VBMS` wiki «Serial protocol» (live data), проверено
против `syssi/esphome-ant-bms` (компонент ESPHome) и `RoboDurden/AntBms-Arduino`.

## Общий формат frame (запрос/запись-фрейм, BT и serial)

| Offset | Функция |
|--------|---------|
| 1–2 | Frame header |
| 3 | Address |
| 4–5 | Data |
| 6 | Checksum |

Заголовки кадра (BT/serial):
- `A5A5` — запись данных/параметров в BMS
- `5A5A` — чтение данных/параметров из BMS
- `DBDB` — запись данных на дисплей/главную плату

Чексумма (для 6-байтовых команд): `checksum = (address + data[0] + data[1]) mod 256`.

## Live data — кадр телеметрии (140 байт)

**Bluetooth**: запрос `0xDBDB00000000` → возвращается **140 байт** данных.
**Serial-порт BMS**: запрос `0x5A5A00000000`.
Плата в активном режиме (дисплей подключён/инициализирован, вещание включено)
может транслировать кадр 140 байт **самостоятельно** с шагом ~140 байт.

### Структура 140-байтного кадра

| Offset | Длина | Поле | Тип | Ед. | Коэфф. |
|--------|-------|------|-----|-----|--------|
| 0 | 4 | Frame Header (`0xAA 0x55 0xAA 0xFF`) | — | — | — |
| 4 | 66 | Voltage data (32 ячейки × 2) | u16 | V | 0.000 (=×0.001) |
| 70 | 4 | Current | int | A | 0.0 (×0.1) |
| 74 | 1 | Percentage of remaining battery (SOC) | u8 | % | 1.0 |
| 75 | 4 | Battery physical capacity | u32 | Ah | 0.000001 |
| 79 | 4 | Remaining battery capacity | u32 | Ah | 0.000001 |
| 83 | 4 | Total battery cycle capacity | u32 | Ah | 0.000 (×0.001) |
| 87 | 4 | Accumulated from boot (uptime) | u32 | s | 1.0 |
| 91 | 12 | Actual temperature (6 × 2) | short | °C | 1.0 |
| 103 | 1 | Charge mos tube status flag | u8 | — | — |
| 104 | 1 | Discharge mos tube status flag | u8 | — | — |
| 105 | 1 | Balanced status flag | u8 | — | — |
| 106 | 2 | Tire length | u16 | mm | — |
| 108 | 2 | Number of pulses per week | u16 | N | — |
| 110 | 1 | Relay switch | u8 | — | — |
| 111 | 4 | Current Power | int | W | 1.0 |
| 115 | 1 | Maximum number of monomer strings (max cell idx) | u8 | — | — |
| 116 | 2 | The highest monomer (max cell voltage) | u16 | V | 0.000 |
| 118 | 1 | Lowest monomer string (min cell idx) | u8 | — | — |
| 119 | 2 | Lowest monomer (min cell voltage) | u16 | V | 0.000 |
| 121 | 2 | Average | u16 | V | 0.000 |
| 123 | 1 | Number of effective batteries (cell count) | u8 | S | — |
| 124 | 2 | Detected discharge tube D–S | u16 | V | 0.0 |
| 126 | 2 | Discharge MOS driving voltage | u16 | V | 0.0 |
| 128 | 2 | Charging MOS driving voltage | u16 | V | 0.0 |
| 130 | 2 | Comparator initial value / control equalization | u16 | — | — |
| 132 | 4 | Balance bitmask (bit 1 = cell 1, …) | u32 | — | — |
| 136 | 2 | System log (0–4 Status, 5–9 battery#, 10–14 order, 15 chg/dis) | u16 | — | — |
| 138 | 2 | Sum check | u16 | — | — |

**Порядок байт** внутри 140-байтного кадра — **big-endian** при вычислении
значений вида `(hi<<8)|lo` в этой структуре (в klotztech перечислены как
последовательность байтов).

### Контрольная сумма live-кадра

Сумма байтов **offset 4 .. 137** (без 4-байтного заголовка и без последних двух
байт чексуммы), сравнивается с `(checksum_hi<<8 | checksum_lo)` на offset 138–139:

```
int expected = 0;
for (i = 4; i < 138; i++) expected += data[i];
int checksum = (data[138] << 8) + data[139];   // big endian
valid = (checksum == expected);
```

## Датчики температуры T1–T6 (offset 91..102)

Шесть значений int16 (°C) — каналы измерения температуры. В протоколе и
официальном приложении именованы нейтрально: «Actual temperature (6 × 2)»
(klotztech/VBMS wiki), «T1..T4» в основной сетке приложения FastDeath/VBMS
(`AtyParam2.java`, `strTitle[10..13]`), «Temperature 1..6» (syssi/esphome-ant-bms),
«Probe 0..5» (imval/AntBMS). **Производитель не публикует официальное
соответствие шести каналов конкретным физическим датчикам.**

Реконструкция по открытым источникам (использована в sunReceiver):

| Канал | offset | Имя (рус.) | Что это (реконструкция) |
|-------|--------|------------|--------------------------|
| T1 | 91..92 | Батарея 1 | внешний NTC-датчик температуры батареи, позиция 1 |
| T2 | 93..94 | Батарея 2 | внешний NTC-датчик температуры батареи, позиция 2 |
| T3 | 95..96 | Силовая плата | температура силовых ключей (MOSFET) / силовой части |
| T4 | 97..98 | Плата управления | температура главной (управляющей) платы |
| T5 | 99..100 | Резерв | доп. канал, не показывается в основной сетке приложения |
| T6 | 101..102 | Резерв | доп. канал, не показывается в основной сетке приложения |

Опорные факты:

- **AFE Sino Wealth SH367309U** (на платах ANT 8–22S) имеет ровно 3 входа
  термоисторов T1/T2/T3: два — под внешние датчики, один — для измерения
  температуры на самой плате (даташит, разбор
  electricmotiontech.com/home/ev-tech-101/ant-bms).
- **Официальный установочный мануал ANT BMS**: плата поддерживает несколько
  (до 4) внешних температурных датчиков (Temp 1–4); датчики — NTC 10K
  (подтверждено замером, endless-sphere.com, тред «What resistor value is this
  (16S ANT BMS)»).
- **Параметры защит BMS** (адреса 25–30 протокола): «Battery high temperature
  charge/discharge protection» (батарея) и «Power tube high temperature
  protection» (силовые ключи) — BMS оперирует двумя семантическими группами
  температур: батареи и силовой части.
- В основной сетке приложения (FastDeath/VBMS) выводятся 4 канала — T1..T4;
  T5/T6 в ней отсутствуют (доп. каналы).
- В нашей установке (16S, батарея и плата в одном корпусе) все 6 каналов
  отдают одинаковую температуру (27–29 °C, см.
  `antbms-live-frame-decoded.md`) — согласуется с несколькими датчиками в
  одном термическом объёме.

## Статусы флагов

### Charge MOSFET status flag
0 Off, 1 Open, 2 Overvoltage protection, 3 Over current protection,
4 battery full, 5 total overpressure, 6 battery over temperature,
7 power over temperature, 8 Abnormal current, 9 Balanced line dropped string,
10 motherboard over temperature, 13 Discharge tube abnormality, 15 Manually closed.

### Discharge MOSFET status flag
0 Off, 1 Open, 2 Over-discharge protection, 3 Over current protection,
5 Total pressure undervoltage, 6 Battery over temperature,
7 Power over temperature, 8 Abnormal current, 9 Balanced line dropped string,
10 Motherboard over temperature, 11 Charge on, 12 Short circuit protection,
13 Discharge tube abnormality, 14 Start exception, 15 Manually closed.

### Balancing status flag
0 Off, 1 Exceeds the limit equilibrium, 2 Charge differential pressure balance,
3 Balanced over temperature, 4 Automatic equalization,
10 Motherboard over temperature.

## Команды read/write (заголовки A5A5 / 5A5A / DBDB) — подтверждено из VBMS и imval

Формат 6-байтовой команды (`send_6bit` в `FastDeath/VBMS` AtyMain.java,
`AntBMS::request_data` в `imval/AntBMS.cpp`):

| Байт | Поле |
|------|------|
| 0–1 | Header: `A5A5`=write, `5A5A`=read(данные), `DBDB`=write на дисплей |
| 2 | Address |
| 3 | Data high (BE) |
| 4 | Data low |
| 5 | Checksum = `(address + data_hi + data_lo) mod 256` |

Скорость UART в библиотеках — **19200** (`BMS_SERIAL.begin(19200)`).

### Запрос телеметрии (read): `5A 5A 00 00 01 01`
Точный кадр из `AntBMS::request_data()`:
```
5A 5A | 00 | 00 01 | 01
read  | addr0 | data=0x0001 | checksum=0x00+0x00+0x01=0x01
```
После него BMS отвечает 140-байтным live-кадром. Это ровно тот кадр, что плата
ANT 22PHB получает по своей RXD линии (зелёный провод дисплея) каждые ~250 мс.

### Адреса write-команд (`A5A5`) из AtyMain.java
- `249` — Discharge MOS switch (data 1/0)
- `250` — Charge MOS switch (data 1/0)
- `248` — Zero current
- `252` — Auto balance
- `253` — Factory reset
- `254` — Reboot
- `246` — Modify BT address
- `241..244` — password телеметрии (verification)
- `102..105` — set password
- `251`/`240` — выбрать тип ячейки (Li-Ion → LiFePO4)
- `DBDB 1 0` — экран switch (screen)

## Примечание: отличать от протокола 2.x (`7E A1`)

Новая прошивка ANT BMS (syssi/esphome-ant-bms, baud 19200) использует **другой**
формат: кадр начинается с `0x7E 0xA1`, длина `6 + data[5] + 4`, CRC16, маркер
`AA 55` в конце. В нашем дампе 19200 кадр имеет заголовок `AA 55 AA FF` на
offset 117 (шаг 140 байт) — это **классический 140-байтный live-кадр**, а не
новый `7E A1`. Единый UART-канал платы вещает 140-байтные live-кадры.

## Источники

- klotztech/VBMS wiki «Serial protocol»: https://github.com/klotztech/VBMS/wiki/Serial-protocol
- syssi/esphome-ant-bms (README протокол + ant_bms.cpp): https://github.com/syssi/esphome-ant-bms
- RoboDurden/AntBms-Arduino (реализация UART): https://github.com/RoboDurden/AntBms-Arduino