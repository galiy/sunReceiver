# Бэклог задач sunReceiver

## Актуализирован 2026-10-06

1. **МАП — подтвердить `_I2C_Err`.** Адрес `0x448` предположительный
   (`cmd/sunreceiver/map_errors.go`); сверить с `map_errors.log` при первом
   реальном событии.
2. **Гигиена.** Коммит/push по явному запросу (проверка на секреты); синхронизация
   локального `sunReceiver.json` с продом.

## Добавлено 2026-10-16

3. ~~BMS malina26 — трактовка `alarm` по типу и `device_errors` (этап 2.5).~~
   **Сделано** (`bms26_alarm.go`, `device_errors` kind=`bms`). Осталось только
   i18n: тексты тревог — как и на прежних BMS-страницах — составные/динамические,
   автоматический словарь их не переводит; при необходимости локализовать отдельно.
4. **BMS malina26 — миграция истории (этап 3).** Перенести ряды/историю из старых
   ключей (`sunreceiver:bms*`, `sunreceiver:enbms:*`) и PG (`bms_averages`,
   `enbms_averages`) в `sunreceiver:bms26:*` + `bms26_averages`, с пересборкой JSON в
   форму `bms26Snapshot`. Связка идентификаторов: ANT — по ёмкости из старого имени
   (`AntBms <cap> A/h`) → `serial` (`AntBMS-DSP-<cap>-NNNNN`); EnBMS — единственный MAC
   → единственный EnBMS-serial (при >1 — не мигрировать, неоднозначно). Идемпотентно
   по маркеру, как `migrateMPPTDeviceKeys`/`migrateMeterDeviceKey`.
