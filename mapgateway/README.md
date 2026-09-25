# mapgateway — шлюз Modbus TCP ↔ Modbus RTU (USB) для МАП Титанатор

Часть проекта sunReceiver. Небольшой C-демон, который держит открытым USB-COM,
на котором МАП находится в режиме **ModBus RTU** (например FTDI, `115200 8N1`), и
слушает TCP-порт (по умолчанию **502**), транслируя запросы **Modbus TCP (MBAP)**
в кадры **Modbus RTU** и обратно. Получается локальный Modbus TCP-сервер МАП.

Зачем: отвязаться от внешнего Modbus-шлюза `192.168.13.74:502` и/или получить
прямой Modbus-доступ к МАП с Малины. sunReceiver адресует МАП жёстко как
`<ip>:502` (`modbusmap.DefaultPort = "502"`), поэтому достаточно сменить
`map.rs485.ip` в `sunReceiver.json` на IP Малины — без правок кода.

## Файлы

- `mapgateway.c` — исходник демона.
- `mapgateway.service` — systemd-юнит для Малины.

## Сборка (кросс-сборка на машине разработки, НЕ на Малине)

Требуется `zig`. Из корня репозитория:

```sh
make VERSION=0.1.0 mapgateway      # -> dist/mapgateway-armv7l-0.1.0 (armv7, static)
```

Вручную (то же самое):

```sh
zig cc -O2 -std=gnu99 -Wall -Wextra -target arm-linux-musleabihf -static \
    -DVERSION='"0.1.0"' -o dist/mapgateway-armv7l-0.1.0 mapgateway/mapgateway.c
```

Быстрая нативная сборка для отладки на Linux/PC:
```sh
gcc -O2 -std=gnu99 -Wall -Wextra -DVERSION='"dev"' -o /tmp/mapgateway mapgateway/mapgateway.c
```

## Установка на Малину (`.60`)

На Малине ничего не собираем — только копируем готовый артефакт:

```sh
scp dist/mapgateway-armv7l-0.1.0 root@192.168.13.60:/settings/daemons/mapgateway
ssh root@192.168.13.60 'chmod 755 /settings/daemons/mapgateway'
# юнит:
scp mapgateway/mapgateway.service root@192.168.13.60:/etc/systemd/system/mapgateway.service
ssh root@192.168.13.60 'systemctl daemon-reload && systemctl enable --now mapgateway.service'
```

(У Малины корень `/` смонтирован `ro`; запись в `/etc` и `/usr` — после
`mount -o remount,rw /`, обратно `mount -o remount,ro /`. Файл демона удобно
держать на `/settings/daemons` — это отдельный rw-раздел.)

## Использование

```
mapgateway [-d device] [-b baud] [-p tcp_port] [-l listen_addr] [-v]
```

- `-d` — путь к COM **явно** (тогда без автопоиска).
- `-u` — Modbus-адрес МАП для опроса при автопоиске (по умолчанию `1`).
- `--sn N` — ожидаемый **серийный номер** МАП (16 бит; можно `51510` или `0xC936`). Если задан,
  кандидат принимается только при точном совпадении `_SerialNum0/1` (0x18 мл., 0x19 ст.).
- `--sn-letter C` — ожидаемая буква серийного номера (`_SerialNum3=0x23`, ASCII).
- Автопоиск (по умолчанию, без `-d`): перебираем свободные `/dev/ttyUSB*` (занятые другими
  процессами пропускаем) и коротким Modbus-опросом блока идентификации (`0x03`, ячейки `0x00..0x2F`)
  ищем свой МАП. Критерий: валидный кадр + сигнатура семейства МАП (`_VerPO(0x02)!=0`,
  `_DevOpt(0x07) in {1,2,3}`) + (при `--sn`) совпадение серийника. «Залипаем» только на найденном;
  при пропаже/10 подряд неудачных транзакциях — ищем снова. Найденные `sn/devopt/verPO` пишутся в лог.
- `-b` — `9600|19200|38400|57600|115200` (по умолчанию `115200`).
- `-p` — TCP-порт (по умолчанию `502`; `<1024` требует root).
- `-l` — адрес прослушивания (по умолчанию `0.0.0.0`).
- `-v` — подробный лог (tx/rx) в stderr в дополнение к syslog.
- `--version` / `-V` — печатает `mapgateway <VERSION>`.

## Проверка

Чтение по TCP (функция 0x03):
```sh
python3 - <<'PY'
import socket,struct
s=socket.create_connection(("192.168.13.60",502),3); s.settimeout(3)
txn=1
for a in (438,1099,1027):
    pdu=struct.pack('>BHH',3,a,1); s.sendall(struct.pack('>HHHB',txn,0,len(pdu)+1,1)+pdu)
    r=s.recv(64); print(a, r.hex()); txn+=1
s.close()
PY
```

## Как это встроить в sunReceiver

В `sunReceiver.json` достаточно поменять хост МАП на Малину (порт 502 зашит в коде):
```json
"map": { "rs485": { "name": "МАП", "ip": "192.168.13.60", "unit": 1, "disabled": false } }
```
Шлюз прозрачен: функция `0x03`, побайтовая адресация ячеек МАП, `unit` берётся из
MBAP и пробрасывается в RTU.

## Заметки

- **Порт 502 < 1024** → нужен root (в Raspbian Jessie systemd 215 нет
  `AmbientCapabilities`), поэтому сервис работает от root; альтернативно порт >1024.
- Демон — **единственный владелец** этого COM. Нельзя одновременно с ним
  запускать другие читалки (наш прежний `modbus_read_map.py` и т.п.).
- Устройство ищется автоматически среди **свободных** `/dev/ttyUSB*` коротким Modbus-опросом
  (номера `/dev/ttyUSB*` плавают после ребута) — по аналогии с `bmslistener`; занятые порты
  (напр. BMS-адаптеры, которые держит `bmslistener`) пропускаются.
- Сервис под systemd: `Restart=always`, `RestartSec=3`, `StartLimitInterval=0` (без лимита частоты
  рестартов). Если демон упадёт — systemd поднимет его.
- Малинин `usbscanner` этот порт не забирает: в режиме ModBus он не отвечает как
  MAP (`/var/map/devices_found` → `D0`), а `mapd` продолжает работать на `ttyAMA0`.
  При подключении/отключении USB один раз срабатывает `udev`→`add_tty`→`tty_scan`
  (кратковременно перезапускает `mapd`) — это нормально.
- Поддерживается обобщённый pass-through PDU (ответ определяется по ответу
  устройства), поэтому `0x03/0x06/0x10` работают; таймаут транзакции 1000 мс,
  при сбое клиенту возвращается исключение `0x0B` (gateway target failed).
- Лог — в syslog (`/dev/log` → rsyslog → `.253`), как у `bmslistener`. Пишутся жизненный цикл
  (старт/стоп/открытие порта), коннекты/дисконнекты клиентов и ошибки шины (`serial timeout`,
  `serial bad crc`); **пофреймовых** tx/rx нет — они только под флагом `-v`. Повторяющиеся ошибки
  шины троттлятся: не чаще одного сообщения в 5 c (подавленные — счётчиком `(+N suppressed)`).

## Статус

Проверено на `.60` (2026‑09‑25): сервис `mapgateway.service` активен и включён,
слушает `:502`; чтение через TCP отдаёт живые значения МАП (например
`0x1B6=185`, `0x44B=4`, `0x587=1`).
