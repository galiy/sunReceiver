# ANT BMS — команды UART: управление и изменение настроек

Полный свод по write-командам ANT BMS (записи параметров и управления) через
UART (классический протокол `A5A5`/`5A5A`/`DBDB`, скорость 19200). Источники —
исходники реальных приложений `FastDeath/VBMS` (AtyMain.java) и `imval/AntBMS.cpp`
плюс `klotztech/VBMS` wiki («Serial protocol», «Control/Parameter addresses»).

## Формат 6-байтовой команды (send_6bit)

| Байт | Поле | Пример (запрос телеметрии) |
|------|------|------|
| 0–1 | Header | `5A 5A` |
| 2 | Address | `00` |
| 3 | Data high (BE) | `00` |
| 4 | Data low | `01` |
| 5 | Checksum = `(addr + data_hi + data_lo) mod 256` | `00+00+01 = 01` |

- `5A 5A` — **чтение** (read) данных из BMS. Live-данные по типу:
  - BT: `0xDBDB 0000 0000` → 140 байт (в апке: plain BT → send 5A)
  - Serial/BMS: `5A 5A 00 00 01 01` → 140-байтный live-кадр (это наш keep-alive!).
- `A5 A5` — **запись** (write): параметры и управление.
- `DB DB` — запись на дисплей (экранная команда, напр. `DBDB 0000 0000` → на
  дисплей, `DBDB 1 0` → screen switch).

## Адреса чтения (5A5A)

| Address | Данные | Назначение |
|---------|--------|-----------|
| 0 | 0x0001 | live-телеметрия (140 байт) — подтверждено `imval/AntBMS.cpp` |

## Запрос текущего состояния (live-телеметрия)

Кадр чтения текущего состояния (подтверждён `imval/AntBMS.cpp` request_data):
```
5A 5A | 00 | 00 01 | 01
read  | addr0 | data=0x0001 | checksum=0x01
```
После него BMS отвечает **140-байтным live-кадром** (заголовок `AA 55 AA FF`,
ячейки с offset 6, текущий ток/SOC/ёмкости/температуры/MOS-флаги/мощность).
Расшифровка полей — `antbms-live-frame-decoded.md`. Этот же кадр дисплей шлёт
каждые ~250 мс («keep-alive»), то есть это и есть способ активации BMS.

## Чтение текущих значений настроек

Настройки читаются read-кадром `5A 5A` + адрес параметра из таблицы ниже
(те же адреса, что и для записи `A5A5`). Формат:
```
5A 5A | <address> | <value_hi> <value_lo> | checksum
read  | параметр  | (для read-запроса обычно 0x0001) | = addr+data
```
BMS отвечает кадром со значением настройки (значение масштабируется так же,
как при записи). На нашей модели отдельно не верифицировано — проверить на живом
устройстве: например прочитать число ячеек (адрес 24), ёмкость (31–32), лимит
тока защиты (9/11).

> Примечание: адреса параметров ниже — номер параметра в `send_6bit`-кадре (как
> в `klotztech/VBMS` «Parameter addresses»). В новой прошивке 2.x (`7E A1`) те же
> настройки адресуются 16-битными регистрами — см. блок «Регистры настроек» ниже.

## Control addresses (управление) — klotztech/VBMS

| Address | Type | Действие |
|---------|------|----------|
| 247 | control | Close BMS power |
| 248 | control | Current zeroing (кулон-счёт обнуление) |
| 249 | control | Discharge MOS switch (data 1=on, 0=off) |
| 250 | control | Charge MOS switch (data 1=on, 0=off) |
| 251 | control | Change parameter to lithium iron phosphate (LiFePO4) |
| 252 | control | Battery auto-balancing |
| 253 | control | Factory settings reset |
| 254 | control | Reboot button |
| 255 | control | Apply button |

Подтверждение из `AtyMain.java`: `A5A5 249/250` (Enable/Disable Dis/Ch MOS),
`A5A5 252` auto balance, `A5A5 253` factory reset, `A5A5 254` reboot,
`A5A5 246` modify BT address, `A5A5 248` zero current,
`A5A5 251`/`A5A5 240` LiFePO4 params.

## Parameter addresses (настройки) — klotztech/VBMS (AtyParam)

Те же адреса используются и для **записи** (изменение настройки, `A5A5`), и для
**чтения текущего значения** настройки (`5A5A` + тот же адрес). Значение
масштабируется (тысячные/десятые) как указано.

| Address | Назначение |
|---------|-----------|
| 1 | Monomer (ячейка) over-voltage alarm 000.000V |
| 2 | Monomer under-voltage alarm (warning) |
| 3 | Monomer over-voltage protection |
| 4 | Monomer under-voltage protection |
| 5 | Monomer over-voltage recovery |
| 6 | Monomer under-voltage recovery |
| 7 | Total voltage over-voltage protection |
| 8 | Total voltage under-voltage protection |
| 9 | Charge over-current protection 000.0A |
| 10 | Charging over-current protection delay (s) |
| 11 | Discharge over-current protection |
| 12 | Discharge over-current protection delay (s) |
| 13 | Equilibrium voltage limit |
| 14 | Equilibrium starting voltage during charging |
| 15 | Equilibrium voltage difference |
| 16 | Equilibrium current value (1–20) |
| 17 | System voltage reference |
| 18 | Current sensor range |
| 19 | Start current (A) |
| 20 | Short-circuit protection current (A) |
| 21 | Short-circuit protection delay (µs) |
| 22 | No-current auto-standby time (s) |
| 23 | Total voltage AD value (4-digit) → конвертируется в реальное напряжение |
| 24 | Number of battery strings (число ячеек) |
| 25 | Battery high-temperature charge protection |
| 26 | Battery high-temperature charge recovery |
| 27 | Battery high-temperature discharge protection |
| 28 | Battery high-temperature discharge recovery |
| 29 | Power tube (MOSFET) high-temperature protection |
| 30 | Power tube high-temperature recovery |
| 31–32 | Battery physical capacity .000 000Ah (76 low, 77 high) |
| 33–34 | Remaining capacity .000 000Ah (78 low, 79 high) |
| 35–36 | Total cycle capacity .000Ah (80 low, 81 high) |
| 41 | Tire length |
| 42 | Number of pulses per week |
| 51–74 | Battery internal resistances |
| 100 | Runtime (70–71 two spaces) |

## Регистры настроек (новая прошивка `7E A1`, syssi/esphome-ant-bms)

Для ориентира — в современном протоколе 2.x (`7E A1`, command `0x51`) настройки
читаются/пишутся по 16-битным регистрам (адрес = index). Перечень (scale/unit):
- `0x0000` CellOvervoltageProtection 0.001V
- `0x0008` PackOvervoltageProtection 0.1V
- `0x000C` CellUndervoltageProtection 0.001V
- `0x0014` PackUndervoltageProtection 0.1V
- `0x0018` CellVoltageDifferenceProtection 0.001V
- `0x0020..` CellOvervoltageWarning / PackOvervoltageWarning
- `0x0068` ChargeOvercurrentProtection 0.1A, `0x006C` DischargeOvercurrentProtection
- `0x0074` ShortCircuitProtection A, `0x007C/0x0080` Charge/DischargeOvercurrentWarning
- `0x0084/0x0086` SOCLowLevel1/2Warning %
- `0x008C` CellBalancingVoltage, `0x008E` CellBalancingStartVoltage
- `0x0094` BalancingCurrent mA
- `0x009A` CellNumber, `0x0098` CellType
- `0x00A2` NominalCapacity, `0x00A6` RemainingCapacity, `0x00AA` TotalCycleCapacity (Ah)
- `0x00C4` StateOfChargeMethod, `0x017A` TireLength, `0x017C` PulseValue
(У нашей модели этот протокол 2.x не наблюдается — вещание идёт классическим
`5A5A`-live-кадрами; для записи настроек уместно использовать `A5A5`+parameter.)

## Статусные флаги записи (реализация в аппке)

Charge/Discharge MOS/Unbalance статусы в live-кадре имеют те же коды, что и при
записи (см. `antbms-protocol-status-frame.md`).

## Вывод
По UART (19200) BMS полностью управляема: read (`5A5A`), write-control (`A5A5` +
строки 247–255), write-параметры (`A5A5` + 1–100). Это тот же функционал, что
заводское приложение: переключение MOS, сброс счётчика, авто-балансировка,
смена LiFePO4, и регулировка лимитов/ёмкости. Это важно для интеграции и
безопасности (запись через UART может менять поведение BMS).