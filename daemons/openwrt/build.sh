#!/bin/sh
# build.sh — кросс-сборка C-демонов sunReceiver под OpenWrt (ath79, big-endian
# MIPS 24Kc, musl) и раскладка в оверлей сборки прошивки TL-WR1043ND.
#
# Тулчейн берётся из дерева OpenWrt (~/src/tools/openwrt/staging_dir).
# Артефакты кладутся в files/ этого дерева (TOPDIR/files), откуда попадают в образ.
#
# usage: VERSION=0.1.0 daemons/openwrt/build.sh [OPENWRT_TOPDIR]
set -eu

REPO="$(cd "$(dirname "$0")/../.." && pwd)"
OPENWRT_TOPDIR="${1:-$HOME/src/tools/openwrt}"
STAGING="${OPENWRT_STAGING_DIR:-$OPENWRT_TOPDIR/staging_dir}"
VERSION="${VERSION:-dev}"

TC="$(echo "$STAGING"/toolchain-mips_24kc_gcc-*_musl)"
CC="$TC/bin/mips-openwrt-linux-gcc"
STRIP="$TC/bin/mips-openwrt-linux-strip"
[ -x "$CC" ] || { echo "ERROR: не найден тулчейн: $CC" >&2; exit 1; }
export STAGING_DIR="$STAGING"

FILES="$OPENWRT_TOPDIR/files"
# Динамическая линковка с musl: бинарники вшиваются в образ, собранный тем же
# тулчейном, поэтому libc всегда совпадает. Существенно меньше статических.
CFLAGS="-O2 -std=gnu99 -Wall -Wextra"

echo "==> сборка bmslistener ($VERSION)"
"$CC" $CFLAGS -DVERSION="\"$VERSION\"" \
	-o "$REPO/dist/openwrt-bmslistener" "$REPO/daemons/bmslistener/bmslistener.c"

echo "==> сборка mapgateway ($VERSION)"
"$CC" $CFLAGS -DVERSION="\"$VERSION\"" \
	-o "$REPO/dist/openwrt-mapgateway" "$REPO/daemons/mapgateway/mapgateway.c"

echo "==> сборка read_bms (CGI)"
"$CC" $CFLAGS -o "$REPO/dist/openwrt-read_bms" \
	"$REPO/daemons/bmslistener/web/read_bms.c"

echo "==> раскладка в $FILES"
"$STRIP" "$REPO/dist/openwrt-bmslistener" "$REPO/dist/openwrt-mapgateway" "$REPO/dist/openwrt-read_bms"
mkdir -p "$FILES/usr/sbin" "$FILES/etc/init.d" "$FILES/etc/config" \
	"$FILES/etc/uci-defaults" "$FILES/www"
install -m 0755 "$REPO/dist/openwrt-bmslistener" "$FILES/usr/sbin/bmslistener"
install -m 0755 "$REPO/dist/openwrt-mapgateway" "$FILES/usr/sbin/mapgateway"
install -m 0755 "$REPO/dist/openwrt-read_bms"    "$FILES/usr/sbin/read_bms"
install -m 0755 "$REPO/daemons/bmslistener/bmslistener.init" "$FILES/etc/init.d/bmslistener"
install -m 0755 "$REPO/daemons/mapgateway/mapgateway.init"   "$FILES/etc/init.d/mapgateway"
: > "$FILES/www/read_bms.php"

echo "==> готово"
ls -l "$FILES/usr/sbin/bmslistener" "$FILES/usr/sbin/mapgateway" "$FILES/usr/sbin/read_bms"
