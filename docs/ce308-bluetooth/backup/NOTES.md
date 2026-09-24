# Backup: проброс Bluetooth-адаптера .9 → .253 по usbip (ОТКЛЮЧЕНО)

Дата снятия бэкапа и отключения: **2026-09-24**.

## Статус

Проброс **отключён и удалён с обоих хостов**. Причина — на `.253` установлен
штатный физический USB Bluetooth-адаптер (Realtek `0bda:8771`, `hci0`,
`RTK_BT_5.0`), поэтому виртуальный vhci-контроллер из MediaTek `.9` больше не
нужен. Полное описание схемы — [`../README.md`](../README.md).

## Что лежит в этой папке (снимки реальных файлов с хостов)

Хост `.9` (sasha-nout) — usbip-сервер:

| Файл бэкапа | Оригинал на `.9` | Назначение |
|---|---|---|
| `dot9-usbip-share.service` | `/etc/systemd/system/usbip-share.service` | systemd-юнит `usbipd` |
| `dot9-usbip-share-bind.sh` | `/usr/local/sbin/usbip-share-bind.sh` | привязка `0e8d:0608` к `usbip-host` |
| `dot9-modules-load-usbip-host.conf` | `/etc/modules-load.d/usbip-host.conf` | загрузка `usbip_host` при старте |

Хост `.253` (gsrv) — usbip-клиент:

| Файл бэкапа | Оригинал на `.253` | Назначение |
|---|---|---|
| `dot253-usbip-bt-watchdog.service` | `/etc/systemd/system/usbip-bt-watchdog.service` | systemd-юнит watchdog |
| `dot253-usbip-bt-watchdog.sh` | `/usr/local/sbin/usbip-bt-watchdog.sh` | TCP-детект сервера + detach/attach |
| `dot253-modules-load-vhci-hcd.conf` | `/etc/modules-load.d/vhci-hcd.conf` | загрузка `vhci-hcd` при старте |

## Параметры (на момент отключения)

- Сервер usbip: `192.168.13.9:3240`, устройства `0e8d:0608`, busid `5-5`.
- Клиент: vhci-порт `00`, период watchdog `30 с`.
- На `.253` контроллер поднимался как `hci0` (BD `C8:94:02:C0:FA:DA`), после
  установки физического адаптера — как `hci1`.

## Что именно удалено с хостов

На `.9`:

```sh
systemctl disable --now usbip-share.service
usbip unbind -b 5-5   # снять привязку устройства
rm -f /etc/systemd/system/usbip-share.service
rm -f /usr/local/sbin/usbip-share-bind.sh
rm -f /etc/modules-load.d/usbip-host.conf
systemctl daemon-reload
rmmod usbip_host 2>/dev/null || true
```

На `.253`:

```sh
systemctl disable --now usbip-bt-watchdog.service
usbip detach -p 00
rm -f /etc/systemd/system/usbip-bt-watchdog.service
rm -f /usr/local/sbin/usbip-bt-watchdog.sh
rm -f /etc/modules-load.d/vhci-hcd.conf
systemctl daemon-reload
rmmod vhci-hcd 2>/dev/null || true
```

Пакеты `usbip`/`linux-tools` **не удалялись** — это стандартные утилиты ядра, не
относящиеся к пробросу; при желании можно удалить отдельно.

## Восстановление (если понадобится)

1. Скопировать файлы обратно на хосты из этой папки (с сохранением прав:
   скрипты `0755`, конфиги `0644`).
2. `systemctl daemon-reload`.
3. На `.9`: `systemctl enable --now usbip-share.service`.
4. На `.253`: `systemctl enable --now usbip-bt-watchdog.service`.
5. Для обратной загрузки модулей при старте — вернуть соответствующие
   `modules-load.d/*.conf`.
