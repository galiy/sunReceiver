# Проброс Bluetooth-адаптера по usbip (MediaTek MT7921K → .253)

## Зачем это нужно

BLE-адаптер на прод-сервере `.253` (gsrv) нестабилен (несколько RTL-донглов,
`hci5`/`hci6`). Рабочий контроллер — **MediaTek MT7921K (RZ608)**, который физически
стоит в основном ПК `.9` (sasha-nout) как **внутренняя USB-функция** `0e8d:0608`
(под встроенным xHCI PCIe `0a:00.3`), а не съёмный донгл. Вытащить и воткнуть его в
`.253` нельзя, но по интерфейсу это обычное USB-устройство, поэтому он **проброшен по
сети через usbip** с `.9` на `.253`.

Это даёт на `.253` стабильный HCI-контроллер `hci0` (BD `C8:94:02:C0:FA:DA`), который
используется CE308-пулером (см. [`docs/modules/ce308.md`](ce308.md)).

## Архитектура

```
.9 (sasha-nout)                          .253 (gsrv)
┌──────────────────────────────┐         ┌──────────────────────────────┐
│ MediaTek BT 0e8d:0608        │         │  vhci_hcd (виртуальный USB)  │
│  usbip bind → usbip-host     │         │  usbip attach → HCI hci0     │
│  usbipd (TCP :3240)          │ ──────► │  BlueZ bluetoothd            │
│                              │  TCP    │  sunreceiver → ce308-пулер  │
│ systemd: usbip-share.service │         │ systemd: usbip-bt-watchdog   │
└──────────────────────────────┘         └──────────────────────────────┘
```

- **Сервер** (на `.9`): `usbipd` слушает `0.0.0.0:3240`, устройство `0e8d:0608`
  привязано к `usbip-host`. Управляется юнитом `usbip-share.service`.
- **Клиент** (на `.253`): `vhci_hcd` импортирует устройство как `usbip attach`.
  После attach ядро поднимает `hci0`; BlueZ регистрирует его как `[default]`.
  Связность поддерживает `usbip-bt-watchdog.service`.

## Компоненты на `.9` (сервер)

### `/usr/local/sbin/usbip-share-bind.sh`

Динамически находит busid устройства по `idVendor=0e8d`/`idProduct=0608` (busid
`5-5` может меняться между загрузками), грузит `usbip_host` и делает `usbip bind`
идемпотентно (ошибка "already bound" игнорируется).

```bash
#!/bin/bash
set -e
VENDOR=0e8d
PRODUCT=0608
modprobe usbip_host
BUSID=""
for d in /sys/bus/usb/devices/*; do
  [ -f "$d/idVendor" ] || continue
  if [ "$(cat "$d/idVendor")" = "$VENDOR" ] && [ "$(cat "$d/idProduct")" = "$PRODUCT" ]; then
    BUSID="$(basename "$d")"
    break
  fi
done
if [ -z "$BUSID" ]; then
  echo "usbip-share: MediaTek BT ($VENDOR:$PRODUCT) not found" >&2
  exit 1
fi
echo "usbip-share: binding busid $BUSID"
usbip bind -b "$BUSID" || true
```

### `/etc/systemd/system/usbip-share.service`

```ini
[Unit]
Description=usbip host: serve MediaTek Bluetooth (0e8d:0608)
After=network-online.target systemd-modules-load.service
Wants=network-online.target

[Service]
Type=simple
ExecStartPre=/usr/local/sbin/usbip-share-bind.sh
ExecStart=/usr/bin/usbipd
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
```

- `ExecStartPre` биндит устройство до старта демона.
- `usbipd` без `-D` работает в foreground, поэтому systemd корректно отслеживает и
  перезапускает его.
- Модуль `usbip_host` также грузится на загрузку через
  `/etc/modules-load.d/usbip-host.conf` (строка `usbip_host`).

Включение/запуск:

```sh
systemctl enable --now usbip-share.service
```

## Компоненты на `.253` (клиент)

### `/usr/local/sbin/usbip-bt-watchdog.sh`

Главный вопрос — **как определить, что проброшенный контроллер «умер»**. Важные
экспериментальные выводы (все «очевидные» HCI-пробники оказались непригодны):

- После обрыва TCP-ссылки `.253` не «самовыздоравливает»: в `/sys` и у `vhci_hcd`
  остаётся stale-запись, а `hci0` продолжает числиться `UP RUNNING`, пока
  `usbipd`-сервер не вернётся. Это ловушка «мёртвого адаптера».
- `hciconfig`/`bluetoothctl show`/`btmgmt` **читают кэш ядра** и возвращают данные даже
  на мёртвой ссылке — не годятся.
- `hcitool -i hci0 cmd 04 0001` (read local version) возвращает `HCI Event 0x0e` с
  кэшированными данными и на живой, и на мёртвой ссылке (нестабильно).
- `hcitool -i hci0 cc <addr>` / `inq` действительно «висят» на мёртвом контроллере,
  но и на живом (к несуществующему адресу) таймаутятся `exit 124` — дискриминатора
  по таймауту нет.
- Рост `vhci_hcd: urb->status -104` (ECONNRESET) в `dmesg` — точный признак обрыва,
  **но** одиночная HCI-команда его не генерирует (нужен постоянный поток URB, какой
  даёт только активный CE308-пулер), поэтому как самостоятельный пробник он ненадёжен.

Из этого следует надёжная и простая модель: **детектировать доступность сервера по
TCP** (`192.168.13.9:3240`) и выполнять **принудительный detach+attach**, когда сервер
был недоступен с последнего успешного attach. Свежий re-attach против *доступного*
сервера всегда даёт рабочий hci0.

Состояния watchdog (цикл `CHECK_SEC=30`):

1. `is_server_up` == НЕТ → наращиваем счётчик `server_down`, ничего не трогаем
   (detach без пользы). Линк восстановится, когда сервер вернётся.
2. `is_server_up` == ДА и `server_down` ≥ `SERVER_DOWN_BEFORE_FAIL` (2) → сервер только
   что вернулся, ссылка stale → **forced re-attach** (detach+attach, дождаться hci0).
3. `is_server_up` == ДА, порт не приаттачен → attach.
4. `is_server_up` == ДА, порт приаттачен и сервер не уходил → **не трогаем** — чтобы не
   сбрасывать работающий контроллер каждые 30 с (это разорвало бы активную BLE-сессию
   CE308).

`do_attach` сначала делает `usbip detach -p 00` (убирает stale вхождение), затем в цикле
`usbip attach`, ждёт появления `hci0` (`wait_hci`, до 30 с) и поднимает `hciconfig hci0 up`.

### `/etc/systemd/system/usbip-bt-watchdog.service`

```ini
[Unit]
Description=usbip Bluetooth link watchdog (attach 0e8d:0608 from 192.168.13.9)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/sbin/usbip-bt-watchdog.sh
Restart=on-failure
RestartSec=10

[Install]
WantedBy=multi-user.target
```

Включение/запуск:

```sh
systemctl enable --now usbip-bt-watchdog.service
```

Логирование — через `logger -t usbip-bt-watchdog` (журнал
`journalctl -u usbip-bt-watchdog.service`). При штатной работе (линк жив) сообщений нет;
сообщения появляются при недоступности сервера и re-attach.

### Обязательный модуль `vhci-hcd` (клиент)

`usbip attach` на `.253` невозможен без загруженного `vhci_hcd`. После перезагрузки
`.253` модуль может не подняться автоматически (в юните watchdog его изначально не
было): `usbip port` возвращает `open vhci_driver (is vhci_hcd loaded?)`, attach не
проходит и `hci0` не появляется — снаружи это выглядит как «исчез Bluetooth-адаптер»
вместе с данными CE308. Симптомы в журнале клиента: `usbip-bt-watchdog: port not
attached while server up; attaching` каждые ~90 с, при этом `ls /sys/class/bluetooth/`
пуст.

Лечение и защита от повтора:
- загрузка при старте: `/etc/modules-load.d/vhci-hcd.conf` со строкой `vhci-hcd`;
- скрипт `usbip-bt-watchdog.sh` теперь сам делает `modprobe vhci-hcd` перед
  `usbip attach` (в `do_attach`);
- ручная проверка/лечение: `modprobe vhci-hcd && usbip attach -r 192.168.13.9 -b 5-5`.

Историческая заметка: именно отсутствие `vhci-hcd` после перезагрузки `.253`
(2026-09-24) дало ложный вывод «сломался пулер CE308 / пропал адаптер» — на деле был
не загружен модуль vhci на клиенте.

## Параметры

| Переменная (клиент) | Значение | Смысл |
|---|---|---|
| `SERVER` | `192.168.13.9` | узел-сервер usbip |
| `PORTNUM` | `3240` | TCP-порт usbipd |
| `BUSID` | `5-5` | busid устройства на сервере |
| `PORT` | `00` | номер vhci-порта на клиенте |
| `CHECK_SEC` | `30` | период цикла |
| `SERVER_DOWN_BEFORE_FAIL` | `2` | сколько подряд «недоступен», чтобы считать ссылку stale |

## Стабильность и поведение при недоступности `.9`

- `.9` (этот ПК) может быть временно недоступен. Пока сервер вниз, watchdog просто
  ждёт (не детачит), а при возврате сервера **сам переподключает** контроллер
  (`server back; link stale -> forced re-attach`). Задержка восстановления —
  `CHECK_SEC * SERVER_DOWN_BEFORE_FAIL + время detach+attach` ≈ 1–2 мин.
- Перезагрузки переживаются:
  - `.9`: `usbip-share.service` (enabled) + `usbip-host.conf` (modules-load)
    переподнимают демон и бинд.
  - `.253`: `usbip-bt-watchdog.service` (enabled) при старте приаттачивает контроллер.
- CE308-пулер обращается к BlueZ через `hci0`. Watchdog **никогда** не перезапускает
  `bluetoothd` и не трогает контроллер при исправной ссылке, поэтому BLE-сессия
  CE308 не прерывается.

## Диагностика

```sh
# сервер (на .9)
systemctl status usbip-share.service
ss -tnlp | grep 3240
usbip list -l                                   # устройство привязано (busid 5-5)
/usr/local/sbin/usbip-share-bind.sh             # повторный бинд

# клиент (на .253)
systemctl status usbip-bt-watchdog.service
journalctl -u usbip-bt-watchdog.service -f       # сообщения re-attach / unreachable
modprobe vhci-hcd                                # если usbip port ругается «is vhci_hcd loaded?»
usbip port                                       # Port 00 In Use …
hciconfig hci0 | head -3                         # BD C8:94:02:C0:FA:DA, UP RUNNING
bluetoothctl list                                # Controller … gsrv [default]
bluetoothctl scan on                             # проверка BLE-радио (найдёт соседние устройства)
```

## Проверено

- Проброс работает: на `.253` появляется `hci0` с BD `C8:94:02:C0:FA:DA`, BlueZ
  регистрирует контроллер `[default]`, BLE-скан находит соседние устройства.
- CE308-пулер через проброшенный адаптер читает и сохраняет данные:
  `ce308: подключено к AA:BB:CC:DD:EE:FF`, `снимок энергии сохранён: …`; в Redis
  `sunreceiver:ce308:current` содержит актуальный снимок
  (`ce308_active_power`, пофазные U/I/P, реактивные мощности).
- Авто-восстановление: при остановке юнита на `.9` watchdog логирует
  `unreachable (1)`, `unreachable (2)`, после возврата сервера —
  `server back; link stale -> forced re-attach` и `re-attached OK`; `hci0` поднимается
  с работоспособным BD-адресом.
- Здоровая ссылка не сбрасывается: при живом сервере watchdog не пишет в журнал и не
  трогает контроллер (за 2+ цикла hci0 тот же, счётчик `-104` стабилен).