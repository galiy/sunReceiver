# deploy/ — внешние сторожевые скрипты

Здесь лежат root-скрипты/systemd-юниты, которые **не входят** в бинарник
sunReceiver и ставятся на прод отдельно (демон работает без root и не может
управлять usbip).

## agm-usbip-watchdog

Сторож удалённого CH340 (RS485, PZEM-017), проброшенного по usbip (аналог
`agm-bt-watchdog` из прежнего отдельного проекта).

Зачем: CH340 физически находится на удалённом хосте (Малина) и экспортируется по
usbip. `usbip attach` — root-операция; `sunreceiver.service` работает от
непривилегированного пользователя с `PrivateDevices=false`, но без capabilities.
Поэтому порт поднимает этот скрипт, а демон только ищет, проверяет активной пробой
и удерживает открытым.

Поведение:

- здоровье: `lsusb` видит мост CH340 (`1a86:7523`) и есть usb-serial tty, чей
  sysfs `idVendor:idProduct` равен VID:PID CH340 (чужой ttyUSB здоровьем не
  считается);
- `busid` **не хранится**: определяется по `usbip list -r HOST`, отбор по VID:PID;
- если CH340 на удалённом хосте несколько — ошибка конфигурации: ничего не
  подключается, громкий лог в syslog;
- перед attach отцепляются застарелые импорты того же `HOST:PORT`;
- cooldown между восстановлениями, один экземпляр (flock), лог в syslog
  (`logger -t agm-usbip-watchdog`);
- параметры — только из окружения юнита или `/etc/agm-usbip-watchdog.conf`
  (адрес usbip, порт, VID:PID, интервалы); `sunReceiver.json` watchdog не читает.

Установка (прод, root):

```sh
install -m 0755 agm-usbip-watchdog.sh /usr/local/sbin/agm-usbip-watchdog.sh
install -m 0644 agm-usbip-watchdog.service /etc/systemd/system/agm-usbip-watchdog.service
# подставить реальный адрес usbip-сервера:
sed -i 's/AGM_USBIP_HOST=.*/AGM_USBIP_HOST=<usbip-host>/' /etc/systemd/system/agm-usbip-watchdog.service
systemctl daemon-reload
systemctl enable --now agm-usbip-watchdog.service
```

Требуется пакет `usbip` (usbip-utils) и `usbutils` (lsusb) на проде, а на
удалённом хосте — экспорт устройства (`usbipd`) и доступность порта (по умолчанию
3240).

## Юнит sunReceiver

Для доступа к `/dev/ttyUSB*` из песочницы правка `sunreceiver.service`:

```
PrivateDevices=false
SupplementaryGroups=dialout
```

Остальной hardening (`ProtectSystem=strict`, пустой `CapabilityBoundingSet` и т.д.)
сохраняется.
