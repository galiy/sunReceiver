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
