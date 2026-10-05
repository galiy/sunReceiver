# Модули системы

Подробные описания модулей (функций) системы. Краткий обзор и навигация — в
[../README.md](../README.md).

| Модуль | Файл | Описание |
|---|---|---|
| Клиент Solarman V5 | [solarman-client.md](solarman-client.md) | пакет `solarman/` |
| Poller инверторов | [inverter-poller.md](inverter-poller.md) | `main.go` |
| Хранение | [storage.md](storage.md) | Redis + PG + аккумулятор |
| Веб-дашборд | [dashboard.md](dashboard.md) | `dashboard.go`, `web/` (templates/static, `go:embed`) |
| МАП + MPPT | [map-mppt.md](map-mppt.md) | `mppt_api.go`, `modbusmap/` |
| Счётчик DDS238 | [../dds238-meter.md](../dds238-meter.md) | `meter_*.go` |
| Счётчик DTS017M | [../dts017m-meter.md](../dts017m-meter.md) | `dts017_*.go` (Modbus RTU; обособленные ключи/ряд Redis + таблицы PG; API/дашборд — отдельно) |
| Счётчик Энергомера CE308 (BLE) | [ce308.md](ce308.md) | `ce308_*.go` |
| BMS EnBMS (Enjie EMU110x, BLE) | [enbms.md](enbms.md) | `enBms_*.go` |
| ANT BMS | [../antbms.md](../antbms.md) | `bms_poller.go`, `bmslistener/` |
| bmslistener (демон) | [bms-listener.md](bms-listener.md) | `bmslistener/` (C + systemd, установка) |
| Шлюз Modbus TCP↔RTU | [../../mapgateway/README.md](../../mapgateway/README.md) | `mapgateway/` (C + systemd) |
| Уведомления в MAX | [notify.md](notify.md) | `notify.go` |
| Сетевое реле SR-201 (лампы) | [../relay_sr-201(2light).md](../relay_sr-201(2light).md) | `relay_control.go` |
| Проброс Bluetooth (usbip) — исторически, отключено | [../ce308-bluetooth/README.md](../ce308-bluetooth/README.md) | `.9` → `.253`, watchdog, systemd |
| Универсальный контракт `values` | [../universal-contract.md](../universal-contract.md) | `main.go`, `commonContractTags` |
| Windows-трей и логирование | [tray-logging.md](tray-logging.md) | `tray_*.go`, `logfile_*.go` |