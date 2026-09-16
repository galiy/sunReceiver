# Модули системы

Подробные описания модулей (функций) системы. Краткий обзор и навигация — в
[../README.md](../README.md).

| Модуль | Файл | Описание |
|---|---|---|
| Клиент Solarman V5 | [solarman-client.md](solarman-client.md) | пакет `solarman/` |
| Poller инверторов | [inverter-poller.md](inverter-poller.md) | `main.go` |
| Хранение | [storage.md](storage.md) | Redis + PG + аккумулятор |
| Веб-дашборд | [dashboard.md](dashboard.md) | `dashboard.go` |
| МАП + MPPT | [map-mppt.md](map-mppt.md) | `mppt_api.go`, `modbusmap/` |
| Счётчик DDS238 | [../dds238-meter.md](../dds238-meter.md) | `meter_*.go` |
| ANT BMS | [../antbms.md](../antbms.md) | `bms_poller.go`, `bmslistener/` |
| Универсальный контракт `values` | [../universal-contract.md](../universal-contract.md) | `main.go`, `commonContractTags` |