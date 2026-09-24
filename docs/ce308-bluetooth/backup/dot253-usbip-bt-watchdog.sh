#!/bin/bash
# usbip Bluetooth link watchdog for .253 (gsrv).
# Keeps the MediaTek BT controller (0e8d:0608) attached from 192.168.13.9 over usbip.
#
# Why this design: after the usbip server (.9) goes away, the vhci client on
# .253 keeps a stale /sys entry and hci0 stays "UP RUNNING" in the kernel cache,
# while every real URB hits ECONNRESET. hciconfig/bluetoothctl read that cache,
# and a single HCI command either also reads the cache or hangs on the dead
# controller - so they cannot tell a healthy link from a severed one (all HCI
# liveness probes were tried and rejected; see docs/<this-doc>.md).
#
# The only robust, self-contained signal is server reachability over TCP on the
# usbip port, combined with the fact that a fresh detach+attach against a
# *reachable* server reliably rebuilds a working hci0.
#
# State machine:
#   - every cycle, check TCP reachability of SERVER:3240
#   - if unreachable: we are "waiting for server". Do NOT detach (that would
#     drop the (already dead) port without benefit and can leave the vhci slot
#     busy). The link recovers automatically once the server is back.
#   - if reachable:
#       * if the port is attached AND we never saw the server go away since the
#         last successful attach -> healthy, leave it (do not disturb CE308).
#         A cheap re-attach test would reset the running controller every cycle,
#         which we avoid.
#       * if the server WAS down since our last good attach, or the port is not
#         attached -> do a forced detach+attach to rebuild a live hci0.
#
# Because CE308 polling talks to BlueZ through this controller, we never restart
# bluetoothd and never touch the controller unless the link is genuinely stale.

set -u

SERVER=192.168.13.9
PORTNUM=3240
BUSID=5-5
PORT=00
CHECK_SEC=30
SERVER_DOWN_BEFORE_FAIL=2   # consecutive unreachable checks before "down" counts

is_server_up() {
  timeout 3 bash -c "exec 3<>/dev/tcp/$SERVER/$PORTNUM" 2>/dev/null
}

is_attached() {
  usbip port 2>/dev/null | grep -q "Port $PORT"
}

wait_hci() {
  local i
  for i in $(seq 1 30); do
    hciconfig hci0 >/dev/null 2>&1 && return 0
    sleep 1
  done
  return 1
}

do_attach() {
  # vhci-hcd после перезагрузки может быть не загружен — иначе attach невозможен
  modprobe vhci-hcd 2>/dev/null || true
  usbip detach -p "$PORT" 2>/dev/null || true
  sleep 1
  local i
  for i in $(seq 1 20); do
    if usbip attach -r "$SERVER" -b "$BUSID" 2>/dev/null; then
      if wait_hci; then
        hciconfig hci0 up >/dev/null 2>&1 || true
        return 0
      fi
    fi
    sleep 3
  done
  return 1
}

log() {
  logger -t usbip-bt-watchdog "$*"
  echo "usbip-bt-watchdog: $*"
}

main() {
  # "down" = reachable TCP but our link is stale (server was unreachable since
  # the last attach and is now back). This is the trigger to re-attach.
  local server_down=0

  while true; do
    if is_server_up; then
      if [ "$server_down" -ge "$SERVER_DOWN_BEFORE_FAIL" ]; then
        log "server back; link stale (was down $server_down checks) -> forced re-attach"
        if do_attach; then
          log "re-attached OK"
        else
          log "re-attach failed; will retry next cycle"
        fi
        server_down=0
      else
        if ! is_attached; then
          log "port not attached while server up; attaching"
          if do_attach; then
            log "attached OK"
          fi
        fi
        # healthy: leave it
      fi
      sleep "$CHECK_SEC"
    else
      server_down=$((server_down + 1))
      log "server $SERVER:$PORTNUM unreachable (${server_down})"
      sleep "$CHECK_SEC"
    fi
  done
}

# Run only when executed directly (not when sourced for testing).
if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  main
fi
