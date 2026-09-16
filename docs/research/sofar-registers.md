# Маппинг регистров Sofar K-TLX (даталоггер LSW-3)

> Источники: `SOFARMap.xml` проекта Sofar_LSW3 + проверка против живых значений
> (инвертор Sofar K-TLX в локальной сети, вер. 2026-09-03). Полное поведение логгера — в
> [solarman-v5.md](solarman-v5.md). Отдельные теги из этих регистров, попадающие в
> универсальный контракт `values`, документированы в
> [../universal-contract.md](../universal-contract.md).

Значения регистров — int16 (знаковые, two's complement); для положительных величин
обычно unsigned.

## Диапазон 1: 0x0000–0x0027 (func 03)

- 0x0000 Inverter status (0 Stand-by, 1 Self-checking, 2 Normal, 3 FAULT,
  4 Permanent)
- 0x0001–0x0005 Fault 1–5 (битовая маска: 1 ID01 Grid OV, 2 ID02 Grid UV,
  4 ID03 Grid OF, 8 ID04 Grid UF, 16 ID05 PV UV, 32 ID06 LVRT, 256 ID09 PV OV,
  512 ID10 PV current unbalanced, 1024 ID11, 2048 ID12 GFCI, 4096 ID13 phase
  sequence, 8192 ID14 boost OC, 16384 ID15 AC OC, 32768 ID16 grid current high)
- 0x0006 PV1 Voltage ×0.1 V, 0x0007 PV1 Current ×0.01 A, 0x0008 PV2 Voltage ×0.1 V,
  0x0009 PV2 Current ×0.01 A
- 0x000A PV1 Power ×10 W, 0x000B PV2 Power ×10 W, 0x000C Output active power ×10 W,
  0x000D Output reactive power ×0.01 kVar
- 0x000E Grid frequency ×0.01 Hz, 0x000F L1 V ×0.1 V, 0x0010 L1 I ×0.01 A,
  0x0011 L2 V ×0.1 V, 0x0012 L2 I ×0.01 A, 0x0013 L3 V ×0.1 V, 0x0014 L3 I ×0.01 A
- 0x0015/0x0016 Total production (32 бит: high*65536+low) kWh, 0x0017/0x0018 Total
  generation time (32 бит) h
- 0x0019 Today production ×10 Wh, 0x001A Today generation time min
- 0x001B module temp ºC, 0x001C inner temp ºC, 0x001D bus voltage ×0.1 V
- 0x001E/0x001F PV1 sample slave CPU ×0.1 V / ×0.1 A, 0x0020 countdown s,
  0x0021 alert, 0x0022 input mode, 0x0023 comm board msg
- 0x0024/0x0025/0x0026 insulation PV1+/PV2+/PV- to ground (Ом), 0x0027 Country
  (0 DE, 12 PL, 9 UK-G59, … см. SOFARMap.xml)

## Диапазон 2: 0x0105–0x0114 (func 03)

String 1–8 voltage ×0.1 V / current ×0.01 A (V на чётных: 0105, 0107, 0109, 010B,
010D, 010F, 0111, 0113).

## Диапазон HW: 0x2000–0x200D (func 04)

Product code, Serial Number, Software/Hardware/DSP versions (строки 2 байта/регистр,
без ratio). Регистр 0x2000 = длина, далее ASCII 2 байта/регистр; может быть НЕ
числом (строка, напр. `.76` = `SA3ES127LC1055`, `.95` = `SA3ES033KAB575`).
Суффикс версий платформ (`V<цифры>V<цифры>V<цифры>`, напр. `V480V100V480`)
отрезается `trimSofarVersions`.

## Серийный номер инвертора (`inverter_sn`)

`inverter_sn` — ASCII-строка HW-диапазона func 04 0x2000–0x200D (см. выше). Сборка
строки — `asciiFromRegisters`.