#!/bin/sh
# agm-usbip-watchdog — внешний сторож удалённого CH340 (RS485, PZEM-017),
# проброшенного по usbip. Аналог agm-bt-watchdog, но для USB-serial:
# демон sunReceiver работает без root и не может выполнять usbip attach,
# поэтому порт поднимает этот скрипт (root, отдельный systemd-юнит).
#
# Логика:
#   - здоровье: в системе есть СОБСТВЕННЫЙ usbip-импорт CH340 (lsusb видит
#     VID:PID) и существует tty, привязанный в sysfs именно к VID:PID CH340;
#   - при пропаже/зависании: detach застарелых импортов ЭТОГО хоста и
#     attach заново. busid НЕ хранится (на удалённом хосте может меняться):
#     он определяется по `usbip list -r HOST` (VID:PID CH340);
#   - если экспортировано несколько CH340 — это ошибка конфигурации: ничего не
#     подключаем, громко пишем в syslog.
#   - cooldown между восстановлениями; один экземпляр (flock).
#
# Параметры (env из unit или /etc/agm-usbip-watchdog.conf):
#   AGM_USBIP_HOST    — адрес usbip-сервера (обязателен)
#   AGM_USBIP_PORT    — порт usbip-сервера (по умолчанию 3240)
#   AGM_USBIP_VIDPID  — VID:PID моста CH340 (по умолчанию 1a86:7523)
#   AGM_USBIP_INTERVAL / AGM_USBIP_UNHEALTHY / AGM_USBIP_COOLDOWN
set -u

TAG=agm-usbip-watchdog
[ -r /etc/agm-usbip-watchdog.conf ] && . /etc/agm-usbip-watchdog.conf

HOST=${AGM_USBIP_HOST:-}
PORT=${AGM_USBIP_PORT:-3240}
VIDPID=${AGM_USBIP_VIDPID:-1a86:7523}
INTERVAL=${AGM_USBIP_INTERVAL:-30}
UNHEALTHY=${AGM_USBIP_UNHEALTHY:-2}
COOLDOWN=${AGM_USBIP_COOLDOWN:-300}
LAST_FILE=/run/agm-usbip-watchdog.last
LOCK=/run/agm-usbip-watchdog.lock

log() {
	logger -t "$TAG" "$*" 2>/dev/null || true
	echo "$TAG: $*"
}

have() { command -v "$1" >/dev/null 2>&1; }

if [ -z "$HOST" ]; then
	log "ОШИБКА: не задан AGM_USBIP_HOST — сторож не запущен"
	exit 1
fi

exec 9>"$LOCK" || exit 0
flock -n 9 || exit 0

for t in usbip lsusb flock logger; do
	have "$t" || log "ВНИМАНИЕ: нет утилиты $t"
done

# healthy — true, если в системе есть импортированный CH340 и его tty.
# tty привязываем к VID:PID через sysfs: чужой USB-serial не должен давать
# ложное «здоров» (главный кейс — CH340 импортирован, а tty мёртв/не создан).
healthy() {
	lsusb -d "$VIDPID" 2>/dev/null | grep -q . || return 1
	for d in /sys/bus/usb-serial/devices/*; do
		[ -e "$d" ] || continue
		base=$(basename "$d")
		[ -e "/dev/$base" ] || continue
		idv=$(cat "/sys/class/tty/$base/device/../../idVendor" 2>/dev/null || true)
		idp=$(cat "/sys/class/tty/$base/device/../../idProduct" 2>/dev/null || true)
		[ "${idv}:${idp}" = "$VIDPID" ] && return 0
	done
	return 1
}

cooldown_ok() {
	[ -f "$LAST_FILE" ] || return 0
	last=$(cat "$LAST_FILE" 2>/dev/null || echo 0)
	now=$(date +%s)
	[ $((now - last)) -ge "$COOLDOWN" ]
}

# remote_busids — список busid экспортированных CH340 на HOST (по VID:PID).
# Печатает по одному busid на строку.
remote_busids() {
	# Реальный вывод `usbip list -r HOST`: строка хоста начинается с " - HOST",
	# строки устройств — с отступа: "      1-1.2: QinHeng ... (1a86:7523)".
	usbip list -r "$HOST" 2>/dev/null |
		sed -n "s/^[[:space:]]*\([0-9][0-9A-Za-z.-]*\):.*($VIDPID).*/\1/p"
}

# detach_local — отцепить застарелые импорты этого HOST:PORT (не трогаем чужие).
detach_local() {
	if ! have usbip; then
		return 0
	fi
	usbip port 2>/dev/null | awk -v host="$HOST" -v port="$PORT" '
		/^Port [0-9]+:/ { p=$2; sub(":", "", p) }
		index($0, "usbip://" host ":" port "/") > 0 { print p }
	' | while read -r p; do
		[ -n "$p" ] || continue
		log "detach застарелого импорта: порт $p"
		usbip detach -p "$p" >/dev/null 2>&1 || true
	done
}

recover() {
	if ! cooldown_ok; then
		log "восстановление пропущено: cooldown ${COOLDOWN}s"
		return 0
	fi
	if ! have usbip; then
		log "ОШИБКА: нет утилиты usbip — восстановление невозможно"
		return 1
	fi
	# usbip-utils использует фиксированный порт 3240 для list/attach;
	# AGM_USBIP_PORT применяется только к сопоставлению импортов при detach.
	if [ "$PORT" != "3240" ]; then
		log "ВНИМАНИЕ: usbip list/attach используют порт 3240; AGM_USBIP_PORT=$PORT влияет только на detach"
	fi

	busids=$(remote_busids)
	count=$(printf '%s\n' "$busids" | grep -c . || true)
	if [ "$count" -eq 0 ]; then
		log "ОШИБКА: на $HOST не экспортировано устройство $VIDPID (CH340)"
		date +%s >"$LAST_FILE"
		return 1
	fi
	if [ "$count" -gt 1 ]; then
		log "ОШИБКА: на $HOST экспортировано несколько CH340 ($count) — не подключаю ни один (ошибка конфигурации)"
		date +%s >"$LAST_FILE"
		return 1
	fi
	busid=$(printf '%s\n' "$busids" | head -n 1)

	detach_local
	log "usbip attach -r $HOST -b $busid (CH340 $VIDPID)"
	if ! usbip attach -r "$HOST" -b "$busid" >/dev/null 2>&1; then
		log "ОШИБКА: usbip attach -r $HOST -b $busid не удался"
		date +%s >"$LAST_FILE"
		return 1
	fi

	i=0
	while [ "$i" -lt 10 ]; do
		sleep 2
		if healthy; then
			log "порт CH340 восстановлен (busid $busid)"
			date +%s >"$LAST_FILE"
			return 0
		fi
		i=$((i + 1))
	done
	log "ОШИБКА: после attach порт CH340 не появился"
	date +%s >"$LAST_FILE"
	return 1
}

bad=0
last_beat=0
log "старт: usbip $HOST:$PORT, CH340 $VIDPID, интервал ${INTERVAL}s, cooldown ${COOLDOWN}s"
while :; do
	if healthy; then
		bad=0
		now=$(date +%s)
		if [ $((now - last_beat)) -ge 600 ]; then
			last_beat=$now
			log "порт CH340 в норме"
		fi
	else
		bad=$((bad + 1))
		log "порт CH340 нездоров (проверка $bad из $UNHEALTHY)"
		if [ "$bad" -ge "$UNHEALTHY" ]; then
			recover || true
			bad=0
		fi
	fi
	sleep "$INTERVAL"
done
