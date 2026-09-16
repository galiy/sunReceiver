# Модуль poller инверторов (`main.go`)

`main.go` — точка входа и poller Deye/Sofar-инверторов. Каждый инвертор опрашивается
в своём независимом цикле `runInverterPoll` (раз в 10 с), так что завис/таймаутил
одно устройство — остальные продолжают без влияния; снимок пишется в Redis сразу по
готовности с фактическим временем.

## Цели и конфиг

Список целей читается из **`sunReceiver.json` рядом с бинарником**
(`os.Executable()`; при `go run .` — fallback в CWD) через `loadConfig` в
`targets []invTarget` {IP, Name, LoggerSN, Kind}:

- раздел `invertors` → `type` "deye"→`kindDeyeString`, "sofar"→`kindSofar`
  (поле `disabled` обязательно; `true` — пропускается);
- раздел `map` → `kindMAP` (MPPT — `kindMPPT` — в конфиг НЕ задаётся, появляется
  динамически, см. [map-mppt.md](map-mppt.md)).

Название — «логическое имя» инвертора (обязательное поле, напр. `Deye Left`,
`Sofar-2.5`, `Bineos Right`; для MPPT генерируется как `MPPT-<slot+1>`). Опрос:
Deye — реальный SN даталоггера (`ReadRegistersDeye`); Sofar — тоже
`ReadRegistersDeye` с 15-байтным datafield и реальным SN (12-байтный
`ReadRegisters`/`BuildReadFrame` Sofar LSW-3 отвечает только heartbeat с кодом 0x05).

## Маппинг регистров

- Sofar — `mapSofarRegisters` (0x0000–0x0027), фильтрация сбойных блоков
  `probableSofarBlock`. Полный маппинг — [research/sofar-registers.md](../research/sofar-registers.md).
- Deye string — `deyeRegMap`/`mapDeyeRegisters` (у каждого сенсора `Tag` — имя
  контракта в values). Полный маппинг — [research/deye-registers.md](../research/deye-registers.md).

## Универсальный контракт `values`

`DeviceResult`/`deviceSnapshot` содержат `values` — **универсальный контракт**
(`valuesContract`, только общие теги `commonContractTags`), см.
[universal-contract.md](../universal-contract.md):

- округление до 1 знака — `round1`/`needsRounding`;
- сериализация в фиксированном порядке — `valuesContract.MarshalJSON`;
- маппинг Sofar — `mapSofarRegisters` (`putSofarSimple`/`putSofarU32`), фильтр
  сбойных блоков — `probableSofarBlock` (физические пороги + мин. размер ≥20
  регистров);
- маппинг Deye — `mapDeyeRegisters`.

## Маппинг МАП/MPPT

`mapMAPRegisters`/`mapMAPAPI`/`mapMPPTAPI` — значения МАП/MPPT в тот же контракт,
см. [map-mppt.md](map-mppt.md). `pollDevice` (case `kindMAP`) читает блоки
`modbusmap`.

## Серийные номера в снимке

- `device_sn` — SN даталоггера, **десятичный**, из поля кадра `DeviceSN`.
- `inverter_sn` — SN самого инвертора: для Deye — ASCII-строка в регистрах
  **0x0003–0x0007**; для Sofar — ASCII-строка HW-диапазона func **04 0x2000–0x200D**
  (может быть строкой, не числом). Сборка — `asciiFromRegisters`, обрезка суффикса
  версий — `trimSofarVersions`. При выключенном инверторе регистры SN читаются
  нулями (`asciiFromRegisters` → ""), поэтому `runInverterPoll` до сохранения
  снимка **донасывает** отсутствующие `inverter_sn`/`device_sn` из предыдущего
  снимка (`redisStore.CurrentOne` — аппаратный номер постоянен).

## Чистое завершение

По сигналу main закрывает `stopBG`, ждёт завершения ВСЕХ фоновых горутин
(`bgWg.Wait()` — инверторы, аккумулятор, чистка, МАП/MPPT, BMS, счётчик,
реставрация, дашборд через `http.Server.Shutdown`, бюджет 5 с) и только потом
defer'ы закрывают пулы Redis/PG — записи при остановке не гоняются с `pool close`.

## Флаги CLI

`-redis <addr>`, `-pg <dsn>` (дефолты из раздела `db`, пустая `-pg` выключает PG),
`-pg-restore-window <dur>` (по умолч. `720h`; `restoreRedisFromPG` всё равно
обрезает окно до `recentCutoff`), `-dashboard <addr>` (по умолч. `:8080`, пустая
выключает).

## Связанные документы

- [Клиент Solarman V5](solarman-client.md) — пакет `solarman/`.
- [research/solarman-v5.md](../research/solarman-v5.md) — протокол и поведение живых
  логгеров.
- [research/sofar-registers.md](../research/sofar-registers.md),
  [research/deye-registers.md](../research/deye-registers.md) — маппинг регистров.
- [universal-contract.md](../universal-contract.md) — контракт `values`.
- [МАП/MPPT](map-mppt.md), [BMS](../antbms.md) — прочие типы целей.