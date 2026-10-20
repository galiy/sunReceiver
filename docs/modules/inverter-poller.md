# Сетевые инверторы через API «Малина26»

Модуль `grid_poller.go`. Сетевые инверторы Deye/Sofar больше **не опрашиваются
напрямую** (Solarman V5 из sunReceiver удалён). Единственный источник — HTTP-API
«Малина26». Настроек инверторов в `sunReceiver.json` нет: имя, размещение, тип и
серийник приходят из API.

## Источники

- `GET /api/grid/status` — список инверторов:
  `id, gen, serial, addr, kind, status, last_seen, name, placement, logger_sn`.
  `logger_sn` — настроенный серийный номер логгера (uint32, decimal); не ключ
  хранения.
- `GET /api/grid/device?serial=<serial>` — снимок: `serial, name, placement,
  type, kind, gen, status, inverter_status, last_seen, energy_today,
  energy_total, fields[1..24], alarm_words, alarms`.
  Значения в `fields` уже приведены к физическим единицам (В, А, Вт, вар, Гц,
  °C, кВт·ч) — повторный масштаб не применяется.
- Транспорт (host/port/password) — из раздела `map`; `map.disabled=true`
  отключает и сетевые инверторы. `map.serial` для grid не требуется.

## Опрос

- Период — фиксированные 5 с (константа `gridPollInterval`).
- Каждый цикл: список, затем параллельный забор снимков (до 6 воркеров) с
  дедлайном цикла; не успевшие устройства пропускают цикл.
- Устройства с пустым `serial` или `last_seen=0` пропускаются.
- Время точки — `last_seen` из API. В историю точка пишется только при
  продвижении `last_seen`; current обновляется каждый цикл. Офлайн-инвертор
  становится stale (окно `inverterStaleWindow`).

## Маппинг в `values`

Поля `fields[]` сопоставляются по `name_key` (срез префикса `stats.f.grid.`):

| Поле API | Тег `values` |
|---|---|
| `pv1_voltage/current/power`, `pv2_*` | одноимённые |
| `ac_active_power`, `ac_reactive_power`, `grid_frequency` | одноимённые |
| `l1/l2/l3_voltage`, `l1/l2/l3_current` | одноимённые |
| `temperature_1` / `temperature_2` | Deye: `temperature_igbt` / `temperature_radiator`; Sofar: `temperature_module` / `temperature_inner` |
| `energy_today`, `energy_total` (top-level) | одноимённые (кВт·ч) |

`inverter_status` и `alarm_w0..w5` в `values` **не попадают**. `pv_power` для
анимации = `pv1_power + pv2_power`.

## Идентичность и хранение

- Ключ — серийник инвертора (`serial` API = аппаратный `inverter_sn`),
  нормализуется `normalizeInverterSN` (срез хвостовых `V<цифры>` у Sofar).
- Redis: `sunreceiver:deye:current` / `sunreceiver:sofar:current`,
  `sunreceiver:ts:<kind>:<serial>:<tag>` (метки `kind/device/tag/name/ip/
  device_sn/placement`). Ключ хранения — `serial`; `inverter_sn` — поле
  снимка из `serial`; `device_sn` — настроенный SN логгера из API (decimal).
  При `logger_sn = 0` (нет конфига или shm от старой конфигурации)
  `device_sn` пуст.
- PG: `deye_averages_r` / `sofar_averages_r`, PK `(serial, ts)`, мета
  `name/device_sn/ip/placement`.
- Старые ряды продолжаются без переименования; для исторических точек без
  `placement` при чтении подставляется текущее размещение по серийнику.

## Ошибки

Аварии берутся из `alarms[]` (резерв — непустые `alarm_words[]`) и пишутся в
`sunreceiver.device_errors` только появлением: `device=serial`, `name` — имя,
`kind=inverter`, `code` — код аварии API.

## Дашборд

- «Дом» и «Гараж» — в схемах анимации, формуле `house_power`/ряде
  `house_inverter_power` и плашках «Мощности инверторов».
- Прочие размещения — только в «Сумме», «Активной мощности по инверторам» и
  «Температурах».
- Порядок — по имени; длина/раскладка шины — автоматически.
