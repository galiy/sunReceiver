# Web-интерфейс ПАК «Малина» (192.168.13.60) — PHP-обёртки и API

> Проверено по состоянию на 2026-09-07. Источник — чтение ФС хоста по SSH
> (root, см. `malina.json` → `ssh`), каталог `/settings/html` (веб-корень,
> symlink `/var/www/html -> /settings/html`).

## Устройство веб-корня и дата-кадр

- ХОСТ: Raspberry Pi, Raspbian 8 (jessie), hostname `malina.localhost`.
  ФС `/` — **read-only** (ext4, mmcblk0p2); данные живут на rw-разделе
  `/settings` (mmcblk0p3). Веб-интерфейс — PHP 5, сервер — Microsoft-IIS/6.0
  на памяти (вероятно nginx-обвязка), Basic-auth (`admin`/пароль из `malina.json`).
- Веб-корень: `/settings/html`. Все данные, меняющиеся в реальном времени,
  демон `mapd` непрерывно складывает в **System V shared memory**, откуда их
  читают PHP-обработчики.

### Сегменты разделяемой памяти (ipcs -m), используемые PHP

| shmop key (dec) | Параметры | Читает |
|---|---|---|
| 1994 (0x7CA) | 1024 б | (bms/прочее) |
| 1995 (0x7CB) | **8192 б** | `read_sec.php` — RAM всех MPPT (по 2048 б на узел) |
| 1996 (0x7CC) | **2048 б** | `read_memory.php`, `get_map_e.php` — RAM МАП (байт-ячейки 0x000..0x7FF) |
| 1998 (0x7CE) | 1024 б | (резерв/прочее) |
| 2001 (0x7D1) | 1024 б | (прочее) |
| 2002 (0x7D2) | 1024 б | (прочее) |
| **2015 (0x7DF)** | **2048 б** | `read_json.php?device=map` — JSON-блок МАП |
| **2016 (0x7E0)** | **4096 б** | `read_json.php?device=mppt` — 4 слота MPPT, по 1024 б |
| **2017 (0x7E1)** | **2048 б** | `read_json.php?device=bat` — 2 слота АКБ |
| 4000/4001/4003 | 1024 б | (облако/прочее) |
| 5000 (0x1388) | 1024 б | `get_eeprom_state.php` — признак записи в EEPROM |

Каждый текстовый блок в shm заканчивается маркером `#EOF`.

### Утилиты, вызываемые через sudo (hist-данные)

- `/usr/sbin/db_read <d|m> <map|charger|mppt|bms> [mppt_number] <field> [date]`
  — исторические данные из БД. Формат: `ok<путь_к_файлу>` → затем содержимое файла
  (PHP отдаёт его и удаляет).
- `/usr/sbin/get_devices.sh` — список подключённых устройств (в т.ч. число MPPT).
- `/usr/sbin/cpu_ram_uptime.sh`, `get_lan_connection.sh`, `get_wlan_connection.sh`,
  `get_mppt_errors.sh <n>`, `/bin/systemctl start add_tty` — системные данные.

---

## PHP, реализующие API-доступ к ПАК «Малина»

Помимо используемых `read_json.php`, `read_memory.php`, `write_eeprom.php`, есть ещё
множество PHP-эндпоинтов (все лежат в `/settings/html`). Ниже — полный перечень
с назначением. Жирным выделены те, что отдают машинные (JSON) данные, пригодные
для интеграции.

### Живые текущие параметры (shared memory)

- **`read_json.php?device=map|mppt|bat`** — сводный JSON текущих параметров МАП/MPPT/АКБ.
- **`read_memory.php?offset=&count=`** — байт-ячейки RAM МАП (shm 1996),
  offset в десятичных байтах, json с адресами. Пример: `offset=1414=0x586`.
- **`read_sec.php?id=&offset=&count=`** — RAM конкретного MPPT (shm 1995,
  `id` 1..N, по 2048 б/узел).
- **`get_data.php`** — исторические точки полей через `db_read`
  (POST: `device`, `time`=day|month, `field`, `number`, `date`).
- **`get_map_e.php?counter=1..5`** — счётчики энергии МАП из `/settings/logs/map_energy.json`
  (1=сеть, 2=заряд от МАП, 3=заряд, 4=продажа, 5=batmon.log).
- **`get_mppt_e.php?mppt=0..3`** — энергия MPPT за сутки из `/settings/logs/mppt<N>_energy.json`.
- **`get_cpu_mem.php`** — CPU/RAM/uptime.
- **`get_nodes.php`** — список узлов (json `/settings/web-data/nodes.list`),
  для `master_node` (мультиустройство).
- **`get_eeprom_state.php`** — есть ли несогласованные изменения EEPROM.
- **`get_db_status.php?action=backup|restore`** — статус резервной копии/восстановления.

### События и ошибки (журналы)

- **`events_get.php`** — события (json `/settings/web-data/events.list`).
- `map_errors.php`, `mppt_errors.php?mppt=` — ошибки МАП/MPPT (HTML-таблица).
- `errors_counter.php`, `map_errors_history.php`, `map_counters_history.php`,
  `mppt_errors_history.php`, `mppt_counters_history.php` — истории (HTML).
- `check_daemons.php`, `check_update.php`, `service_control.php` — служебное.

### Управление и настройка (HTTP POST/действия)

- **`write_eeprom.php`** — запись ячеек EEPROM/RAM через очередь сообщений
  (SysV msg → демон `mapd`, ключи 2100/2101). Формат POST `data` =
  `[[адрес, значение], ...]`.
- `write_sec.php` — запись в RAM MPPT; `write_shm.php` — запись в shm.
- `change_pass.php`, `password.php` — смена пароля/доступа.
- `eth_set.php`, `wifi_set.php`, `network.php`, `hostname_set.php`, `time_set.php`,
  `timezone.php` — сеть/система.
- `email.php`, `email_set.php`, `email_test.php`, `cloud.php`, `set_cloud_settings.php`,
  `cloud_switch.php`, `get_cloud_enable.php`, `get_cloud_status.php`,
  `set/get_emoncms_settings.php`, `emoncms_switch.php`, `get_emoncms_enable/status.php` —
  облако E-mail/emonCMS.
- `batmon_set.php`, `batmon_reset.php`, `batmon_history.php` — BMS.
- `load_menu.php`, `mac_set.php`, `mac_info.php`, `tables.php`, `sysinfo.php`,
  `live_graphs.php`, `history.php` — UI.
- `devices.php`, `find_devices.php`, `master_node.php`, `master_node_set.php`,
  `master_node_save.php`, `master_node_load.php`, `request_node.php`, `save_ab_set.php`,
  `load_ab_set.php`, `wind_mppts_set.php` — работа с устройствами.
- `reboot.php`, `reboot_do.php`, `shutdown_do.php`, `update.php`, `update_main.php`,
  `update_start.php`, `db_backup.php`, `db_restore.php`, `usb_backup.php` — обслуживание.
- `get_lan_state.php`, `get_wlan_state.php`, `read_sec.php` др. — статусы соединений.

### UI-страницы
`index.php`, `password.php`, `sysinfo.php`, `Mac`, `tables.php`, `live_graphs.php`,
`history.php`, `menu.json`/`submenu.json` (локаль `RU`/`EN`), css/js, `MICROART-MIB.txt`
(SNMP-описание — отдельный способ опроса УМАП через SNMP).

---

## Примечания для интеграции

- Для чтения в реальном времени проще всего брать `read_json.php?device=mppt|map|bat`
  (готовые JSON) — это то же, что уже используется в `docs/read_json.md`.
- Для байт-точных данных RAM МАП — `read_memory.php` (shm 1996), RAM MPPT — `read_sec.php`
  (shm 1995): это аналог Modbus-гейта (байт-ячейки, см. `docs/map/`), но уже свёрнутый в память.
- История (энергия, поля) читается через `get_data.php`/`get_map_e.php`/`get_mppt_e.php`
  из `/settings/logs/*.json` и БД через `db_read`.
- Системные/инфраструктурные данные (список узлов, CPU, статус сети, события,
  состояние EEPROM) — отдельные маленькие PHP.
- Скрытый конфиг доступа — `malina.json` (не в git): Basic-auth для HTTP и
  `ssh` для FС. Веб-роот на rw-разделе `/settings/html`.