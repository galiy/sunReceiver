# Модуль `solarman/` — клиент протокола Solarman V5

Пакет-клиент WiFi-даталоггеров (Solarman LSW-3/LSE, TCP 8899). Работает поверх
протокола, описанного в [research/solarman-v5.md](../research/solarman-v5.md)
(формат кадра, datafield Sofar/Deye, коды ошибок, CRC).

## Функции сборки кадра (`frame.go`)

- `Checksum8(b []byte) byte` — `sum(b[1:len-2]) mod 256`, checksum кадра.
- `CRC16Modbus(data []byte) uint16` — стандартный CRC16-Modbus (init 0xFFFF,
  poly 0xA001 отражённый, без invert); в PDU пишется little-endian.
- `BuildReadFrame(deviceSN uint32, serial uint16, startReg, regCount uint16) []byte`
  — кадр с **12-байтным** datafield (для **Sofar**).
- `BuildDeyeReadFrame(deviceSN, unit uint32, serial uint16, startReg, regCount uint16) []byte`
  — кадр с **15-байтным** datafield + реальный SN (для **Deye**).
- `BuildDeyeReadFrameFn(deviceSN, unit uint32, serial uint16, startReg, regCount uint16, fn byte) []byte`
  — то же, но с произвольной Modbus-функцией `fn` (напр. func 04 для HW-диапазона
  Sofar 0x2000–0x200D).
- `DeyeErrorCode(frame Frame) (byte, bool)` — код ошибки логгера из 29-байтного
  heartbeat (0x05/0x06).
- `SplitFrames(raw []byte) []Frame` — разбивка TCP-потока на кадры (длина =
  **11 + PayloadLen + 2**), отбрасывает кадры с повреждённым checksum.
- `ParseModbusPDU(payload []byte, offsets ...int) []ModbusPDU` — поиск Modbus-ответа
  `01 03/04 <vlen>` в payload (не по фиксированному смещению); `offsets` —
  подсказки, откуда начинать поиск.
- `ParseResponseHeader`, `FrameHexDump` — диагностика/отладка.

## Клиент (`client.go`)

- `Client` — экспортируемая структура конфигурации TCP-клиента: `Address`,
  `DeviceSN`, `Timeout` (ожидание первого байта), `IdleWindow` (пауза «тишины»
  перед завершением чтения; для Sofar больше — паузы между кусками до ~6.5 с),
  `MaxTotal` (общий лимит приёма от «зомби»-логгера). Готового конструктора/опций
  нет — клиент собирается литералом (`&solarman.Client{...}`).
- `Exchange(ctx, req []byte) ([]Frame, error)` — отправить запрос, прочитать ответ.
- `ReadRegisters(ctx, startReg, regCount uint16) ([]ModbusPDU, []Frame, error)` —
  чтение через 12-байтный кадр (Sofar).
- `ReadRegistersDeye(ctx, startReg, regCount uint16, unit uint32) ([]ModbusPDU, []Frame, error)`
  — чтение через 15-байтный кадр (Deye).
- `ReadRegistersDeyeFn(...)` — то же с произвольной функцией (func 04).

Цикл чтения — до «тишины» `IdleWindow`, первый байт — до `Timeout`, общий предел
приёма — `MaxTotal`.

## Маппинг регистров

Регистры инверторов здесь **не** маппятся — маппинг живёт в poller (`main.go`):
[Sofar](../research/sofar-registers.md), [Deye](../research/deye-registers.md), а
теги универсального контракта — в
[universal-contract.md](../universal-contract.md).

## Диагностика

`probe/main.go` — самостоятельный инструмент: `go run ./probe <ip> 8899 <sn hex32>
[sn2...] <start hex> <count hex>` — строит Deye-кадр (`BuildDeyeReadFrame`) с каждым
SN по очереди, шлёт, дробит ответ (`SplitFrames`), печатает регистры
(`ParseModbusPDU`) и код Deye-ошибки (`DeyeErrorCode`). Перебор unit-адресов — через
env `PROBE_UNITS=1,2,...`. Собственных копий CRC/checksum/сборки кадра нет.