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
инверторы `<ip-суффикс>`=`##########`). Сборка строки — `asciiFromRegisters`.
(реальные серийные номера и адреса — приватно, вне git.)
## Alert-регистры Deye (аварии) — найдено 2026-10-01

В апстриме `StephanJoubert/home_assistant_solarman` есть отдельная группа **Alert**
(rule 6 — битфилд, 6 регистров по 16 бит), но **без имён битов** и отдельно для
разных семейств:

| Семейство (yaml) | Регистры Alert |
|---|---|
| `deye_hybrid.yaml` (1-фазный hybrid) | `0x0065..0x006A` |
| `deye_sg04lp3.yaml` (3-фазный hybrid) | `0x0229..0x022E` |
| `deye_string.yaml` (grid-tie/string) | **Alert отсутствует** |

`parser.py` для rule 6 просто возвращает список hex-значений регистров — карты
bit→fault в апстриме нет. В `kbialek/deye-inverter-mqtt` (`deye_events.py`,
`metric_group_string.md`) аварии также не декодируются, есть только статус
(`Running Status`, рег. `0x003B`: 0 standby, 1 self-checking, 2 normal, 3 FAULT).

**Что сделано в sunReceiver:** наш блок опроса Deye расширен до `0x3B..0x74`
(ранее `0x3C..0x74`) — теперь читается и `Running Status` (`0x3B`), и кандидатные
Alert-регистры `0x65..0x6A`. Значения логируются сырыми при изменении (и раз в
10 мин) строкой:

```
deye alert <ip>: status(0x3B)=N regs 0x65-0x6A = 0x<24 hex>
```

Живая базовая линия (2026-10-01, все 5 инверторов .70/.79/.91/.92/.93):
`status=2`, Alert = `0x000000000000000000000000` — регистры действительно нулевые
в норме. Декод битов включим после кадра при `status=3 (FAULT)`.

**Ссылки-источники:** `github.com/StephanJoubert/home_assistant_solarman`
(`inverter_definitions/deye_*.yaml`, `parser.py`),
`github.com/kbialek/deye-inverter-mqtt` (`docs/metric_group_string.md`).

## Декод аварий Deye (F1–F64) — реализовано 2026-10-02

Источник карты: проект `github.com/pbix/HA-solark-PV` (Sol-Ark/Deye, те же
регистры): `FAULT_INFO_RAW` = holding R103–R106 = `0x67–0x6A`, тип UINT64,
wordorder LSW-first (младшее слово — первый регистр). **Бит N−1 = код F(N)**:

```
bitmap = reg[0x67] | reg[0x68]<<16 | reg[0x69]<<32 | reg[0x6A]<<48
```

Проверено на живом событии (2026-10-02, отключение 220В у трёх grid-tie Bineos
`.91/.92/.93`): Alert = `0000 0000 0000 0000 0004 0000` → `0x69.2` → bit34 →
**F35 AC_NoUtility** («нет сети»), статус `0x3B`: 2 normal → 4 → 1 self-checking → 2.
Регистры `0x65`/`0x66` при этом всегда нулевые (Sol-Ark их не использует).

В `sunReceiver` добавлена таблица имён `deyeFaultNames` (F1–F64, RU) и
`decodeDeyeFaults`; активные коды пишутся в `device_errors` (device=IP,
kind=`inverter`) только появлением. Сырой лог Alert (`deye alert …`) сохранён.

Ссылки-источники: `HA-solark-PV` (`const.py` FAULT_TABLE, `fault_info.py`,
`solark_register_map.py`), invertererrorcodes.com/deye, solaranalytica.com.

### Предупреждения W1–W32 (2026-10-02)

Источник: сохранённая страница Inversol «Deye error codes W1–W32 F1–F64»
(`~/Загрузки/deye/1/…Inversol.html`). Регистры предупреждений — `0x65/0x66`
(32 бита, LSW-first), бит N−1 = W(N). В `sunReceiver` — `deyeWarnNames` (W1–W32, RU)
и `decodeDeyeWarnings`; предупреждения **не пишутся в историю** (не аварии), а
декодируются в сыром логе `deye alert …` (поле `warns=[ … ]`). Локализация ru/en/zh
в `cmd/sunreceiver/web/static/js/i18n.js`.
