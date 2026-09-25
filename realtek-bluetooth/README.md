# Realtek Bluetooth (RTL8761BU / USB 0bda:a728)

Материалы из открытых источников для диагностики USB-BT-адаптера **Realtek
Bluetooth 5.4 Radio (0bda:a728 = RTL8761BU)** на прод-сервере `192.0.2.253`.
Адаптер перестал инициализироваться: ядро даёт `command tx timeout` на команде
конфигурации прошивки и `RTL: Read reg16 failed (-110)`, контроллер остаётся
`DOWN` с адресом `00:00:00:00:00:00`.

## Идентификация чипа

- `lsusb`: `0bda:a728 Realtek Semiconductor Corp. Bluetooth 5.4 Radio`
- Ядро (dmesg): `RTL: examining hci_ver=0a hci_rev=000b lmp_ver=0a lmp_subver=8761`
- `btusb.c` (mainline): `/* Realtek 8761BU Bluetooth devices */ { USB_DEVICE(0x0bda, 0xa728), driver_info = BTUSB_REALTEK | ... }`
- Прошивки: `rtl_bt/rtl8761bu_fw.bin` + `rtl_bt/rtl8761bu_config.bin`
- Загруженная версия прошивки (dmesg): `fw version 0xdfc6d922`

## Содержимое папки

| Файл | Источник | Назначение |
|--|--|--|
| `rtl8761bu_fw.bin` | linux-firmware (main) | прошивка RTL8761BU, версия 0xdfc6d922 |
| `rtl8761bu_config.bin` | linux-firmware (main) | конфиг RTL8761BU |
| `btrtl.c` | linux.git (torvalds/master) | драйвер загрузчика прошивки Realtek (kernel `drivers/bluetooth/btrtl.c`) |
| `btusb.c` | linux.git (torvalds/master) | USB-драйвер BT (kernel `drivers/bluetooth/btusb.c`) |

## SHA-256

```
1d7a9597349ad89344fa16c1913d3e39e9a12e966e417ca16871bc79bbe59edb  rtl8761bu_fw.bin
6c28a3f07c6a30ed208c4b64862a23f02b7d93543ea980edd24df16bab45095f  rtl8761bu_config.bin
```

## Источники

- Прошивки: `https://git.kernel.org/pub/scm/linux/kernel/git/firmware/linux-firmware.git/plain/rtl_bt/`
- Драйвер: `https://git.kernel.org/pub/scm/linux/kernel/git/torvalds/linux.git/plain/drivers/bluetooth/`

## Важное замечание

Скачанная из открытого архива прошивка `rtl8761bu_fw.bin` (44484 байт, версия
0xdfc6d922) **идентична** установленной на сервере (`/lib/firmware/rtl_bt/`).
То есть таймаут `RTL: Read reg16 failed (-110)` / `command tx timeout` **не
является следствием отсутствия или устаревшей прошивки** — это сбой на уровне
«прошивка ↔ контроллер / USB-питание». Перезагрузка драйвера
(`modprobe -r btusb && modprobe btusb`) контроллер не восстанавливает.

## Наблюдение (2026-09-23) — отличие «мягкого» зависания от аппаратного

Наблюдение из эксплуатации двух разных сбоев:

- **Мягкий сбой** (радиопрошивка, `le-connection-abort-by-local`):
  перезагрузка драйвера `btusb` **помогает**, поток данных CE308
  восстанавливается за 20–40 с. Это штатный случай — его закрывает внешний
  watchdog (`/opt/sunreceiver/watchdog.sh`) перезагрузкой драйвера **без**
  рестарта сервиса.
- **Аппаратный отказ**: контроллер фактически не отвечает на HCI-команды
  (`hciconfig`: `Can't get device info` / `Can't read local name: Connection
  timed out`), хотя прошивка загружается (`fw version 0xdfc6d922`). Здесь
  перезагрузка драйвера **не помогает**: при каждом `modprobe -r btusb`;
  `modprobe btusb` контроллер получает **новый номер** `hci0→hci3→hci5→hci6`
  (старые не отпускаются), но продолжает молчать. Ни перезагрузка драйвера, ни
  рестарт сервиса это не лечат — нужна **пересадка USB-адаптера** (другой порт /
  хаб с питанием) либо **перезагрузка хоста**.

Вывод: чтобы отличить мягкий случай от аппаратного, достаточно `hciconfig`:
если контроллер в списке есть, но `hciconfig <hci> name` отдаёт
`Can't get device info` / `Can't read local name` — это аппаратный отказ, а не
сбой, который лечится драйвером.

## Рекомендуемые действия на сервере (аппаратная часть)

- переподключить USB-адаптер (пересадка на другой порт / USB-хаб с питанием);
- проверить стабильность USB-порта и питание (Realtek BT 5.4 чувствителен к USB);
- при повторяемом сбое — перезагрузка хоста или замена адаптера.

После восстановления контроллер должен появиться как `hci0` с корректным
BD-адресом, и пулер CE308 (программно проверяющий питание адаптера) начнёт
писать показания.