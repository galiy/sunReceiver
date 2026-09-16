# bmslistener — демон-слушатель ANT BMS (дополнение ПАК «Малина»)

Пассивный C-демон **для ПАК «Малина»** (Raspberry Pi, arm): слушает USB-serial
адаптеры, на которых ANT BMS вещает 140-байтные live-кадры (UART 19200 8N1), и
публикует коллекцию активных адаптеров в System V shared memory (ключ **2018**,
32 КБ) в формате `{"updated":<epoch>,"devices":[{...},...]}` + терминатор `#EOF` —
та же конвенция, что у существующих на Малине демонов `mapd`/`mpptd`/`dbed`.

Это **дополнение к уже работающим на Малине сервисам**: bmslistener ставится рядом с
`mapd` (МАП), `mpptd` (MPPT), `dbed`/`mserver` и веб-интерфейсом, используя ту же
шину/shared-memory экосистему. Данные читает `web/read_bms.php` (см.
[`antbms.md`](../antbms.md)), а в sunReceiver — `bms_poller.go` (модуль ANT BMS).

Исходники: [`bmslistener/`](../../bmslistener/) → `bmslistener.c`, `Makefile`,
`bmslistener.service`, `web/read_bms.php` (web-api-эндпоинт).

Конечная цель демона — **не только** публикация параметров в shm, а **выдача данных
через web-api**: `web/read_bms.php` читает shm 2018 и отдаёт готовый JSON (аналог
`read_json.php` для других устройств). Именно его опрашивает `bms_poller.go`
(через `mppt.bms_path` в конфиге).

## Что делает

- Пассивно читает live-кадры ANT BMS с USB-serial адаптеров (по аналогии с `mapd`/
  `mpptd` для других устройств).
- Публикует в shm (ключ 2018) JSON-коллекцию устройств с `#EOF`-терминатором.
- Формат shm — общий для экосистемы ПАК «Малина», поэтому читается `read_bms.php`
  без модификации остального веб-интерфейса.
- `Restart=on-failure`, лимит частоты стартов — чтобы при crash-лупе не дёргать
  USB-serial (PL2303) каждые 3 с.

## Установка (на ПАК «Малина»)

Выполняется **на Малине** (arm, Raspberry Pi), рядом с существующими сервисами.
Требуется `gcc` (компилятор C, стандарт gnu99). Скопировать каталог `bmslistener/` на
Малину и в нём:

```sh
make          # собрать бинарник bmslistener (arm)
sudo make install
```

`make install` выполняет:

1. `install -m 0755 bmslistener /usr/sbin/bmslistener` — кладёт бинарник в `/usr/sbin/`.
2. `install -m 0644 bmslistener.service /etc/systemd/system/bmslistener.service` — юнит.
3. Устанавливает веб-скрипт `web/read_bms.php` → `/settings/html/read_bms.php`
   (владелец `www-data:www-data`, права 0644).
4. `systemctl daemon-reload && systemctl enable bmslistener.service` — активация.

Веб-скрипт ставится на **rw-раздел** `/settings/html` (веб-корень Малины, см.
[`malina-web-api.md`](../malina-web-api.md)); `/` там read-only. Путь веб-корня
переопределяется переменной `WEBROOT` (по умолчанию `/settings/html`).

Если нужно обновить только web-api без пересборки демона и без перезапуска сервиса:

```sh
sudo make install-web
```

Откат установки: `sudo make uninstall` (бинарник, юнит и веб-скрипт удаляются).

Затем запустить и проверить:

```sh
sudo systemctl start bmslistener.service
systemctl status bmslistener.service   # active (running)
```

Веб-скрипт `read_bms.php` (web-api) кладётся в веб-корень автоматически при
`make install` (см. выше).

### Юнит `bmslistener.service`

- `Type=simple`, `ExecStart=/usr/sbin/bmslistener`, `User=root`, `Nice=-5`.
- `Restart=on-failure`, `RestartSec=3`; лимит частоты старта `StartLimitIntervalSec=60`
  / `StartLimitBurst=5`.
- `WantedBy=multi-user.target`.

## Права и факты

- shm — System V, ключ **2018**, 32 КБ.
- Формат кадра и распиновка ANT BMS — в [`antbms.md`](../antbms.md).
- Веб-доступ (`read_bms.php`) — та же Basic-авторизация, что и на остальном
  веб-интерфейсе ПАК «Малина».
- Замечание по совместимости systemd: `StartLimit*Sec` на systemd ≥229 читается
  только из `[Unit]` (в `[Service]` игнорируется с warning); на 215 (Малина) имя
  `*_Sec` не распознаётся → безвредный игнор, работает глобальный лимит 5/10 с.

## Связка в системе

`bmslistener` (shm 2018, на Малине) → `read_bms.php` (отдаёт JSON) → `bms_poller.go`
(модуль ANT BMS в sunReceiver) → 5-мин усреднённые точки в Redis + PG (см.
[`storage.md`](storage.md)).