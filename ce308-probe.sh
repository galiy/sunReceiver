#!/usr/bin/env bash
# Диагностический пробник BLE-счётчика Энергомера СЕ308/СЕ208 через ЛОКАЛЬНЫЙ
# Bluetooth-адаптер (изолированно от прод-сервиса sunReceiver).
#
# Переиспользует боевой код подключения/чтения CE308 (build tag ce308probe),
# поэтому результат отражает ровно то, что делает прод-пулер.
#
# Использование:
#   ./ce308-probe.sh                 # MAC/PIN из sunReceiver.json (раздел ce308)
#   CE308_MAC=6C:.. CE308_PIN=123456 ./ce308-probe.sh
#   CE308_PROBE_ROUNDS=10 ./ce308-probe.sh
set -euo pipefail
cd "$(dirname "$0")"

echo "== локальные Bluetooth-адаптеры =="
hciconfig -a 2>/dev/null | grep -E '^hci|BD Address|Name:' || echo "(hciconfig: нет контроллеров)"
bluetoothctl list 2>/dev/null || true
echo

exec go test -tags ce308probe -run '^TestCE308Probe$' -v -count=1 \
  -timeout "${CE308_PROBE_TIMEOUT:-300s}" .
