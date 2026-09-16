# Маппинг регистров Deye string (даталоггер LSE)

> Источники: string-группа карты `kbialek` + проверка против живых значений
> (инверторы Deye string в локальной сети, решено 2026-09-03). Поведение
> логгера и обязательные отличия кадра (15-байтный datafield, реальный SN) — в
> [solarman-v5.md](solarman-v5.md). Теги из этих регистров, попадающие в
> универсальный контракт `values`, документированы в
> [../universal-contract.md](../universal-contract.md).

Маппинг живёт в `deyeRegMap` в `main.go`.

- 0x3C Production today ×0.1 kWh, 0x3E Uptime min, 0x3F-0x40 Total production
  (32 бит, LW first) ×0.1 kWh
- 0x46/0x47/0x48 Grid L12/L23/L31 V ×0.1, 0x49/0x4A/0x4B L1/L2/L3 V ×0.1,
  0x4C/0x4D/0x4E L1/L2/L3 I ×0.1
- 0x4F AC Freq ×0.01 Hz, 0x50 Operating power ×0.1 W, 0x52 DC total power ×0.1 W,
  0x54 AC apparent power ×0.1 W, 0x56-0x57 AC active power (32) ×0.1 W,
  0x58 AC reactive power ×0.1 W
- 0x5A Radiator temp ×0.1 −100 offset, 0x5B IGBT temp ×0.1 −100 offset
- 0x6D/0x6E PV1 V/I ×0.1, 0x6F/0x70 PV2 V/I ×0.1, 0x71/0x72 PV3 V/I ×0.1,
  0x73/0x74 PV4 V/I ×0.1 (на наших 2-цепных 1-фазных — 0, но часть string-маппинга
  kbialek)
- 0xC6-0xC7 Load power (32, signed) ×1 W, 0xC8 Daily load ×0.01 kWh,
  0xC9-0xCA Total load (32) ×0.1 kWh
- 0xCB-0xCC Grid power (32) ×1 W, 0xCD Daily sold ×0.01 kWh, 0xCE-0xCF Total sold
  (32) ×0.1 kWh, 0xD0 Daily bought ×0.01 kWh, 0xD1-0xD2 Total bought (32) ×0.1 kWh

## Примечания по недокументированным и сомнительным регистрам

- **Пробелы в диапазоне чтения** (не задокументированы в kbialek string-группе):
  0x3D, 0x41-0x45, 0x51, 0x53, 0x55, 0x59, 0x5C-0x6C.
- **mxbode/Deye-SUN-SG05LP3-EU-SM2-Modbus-TSV** — это карта **ГИБРИДНОЙ** модели
  SG05LP3, не нашей string: её адреса противоречат нашим живым значениям (у них
  0x6D = «Max A Charge», у нас 0x6D = PV1 voltage 212V), поэтому её имена для
  нашего диапазона не использовались. Из неё лишь совпадение по адресу: у них
  0x3D = Fernsperre (дистанционный замок) и 0x51 = SchalterModus (режим работы) —
  у нас оба = 0, согласуется, но **НЕ верифицировано**, в JSON оставлены hex.
- **0x5D = 1000** — единственный непустой на всех 5 логгерах (константа; вероятно
  ограничение мощности 100% ×10, не подтверждено). В `raw_registers` остаётся под
  hex-адресом.
- **0x005B IGBT temp** не подключён (0 регистр → −100).
- **Внимание 0x58**: kbialek помечает как «AC reactive power ×0.1» (raw 365 →
  36.5 var, правдоподобно). ×10 даёт 3650 var — абсурд (P≈404 W, S≈356 VA);
  масштаб ×0.1 не переверифицирован документально, значения в var.

## Серийный номер инвертора (`inverter_sn`)

`inverter_sn` — ASCII-строка в регистрах **0x0003–0x0007** (10 цифр, проверено:
.70=`2405018274`, .79=`2405018294`, .91=`2103014266`, .92=`2103014293`,
.93=`2103014015`). Сборка строки — `asciiFromRegisters`.