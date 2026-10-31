#!/usr/bin/env bash
# Build the driver guest that invirtio install takes: one UML with the
# drivers and VDUSE backends of the WiFi and Ethernet guests, and its
# initramfs with the WiFi firmware. invirtio picks the backend for each
# device when it starts the device's guest.
#
# usage: invirtio-build-guest.sh [<build dir>]
#
# The default build directory is linux-uml/build-invirtio. The initramfs goes
# to <build dir>/initramfs/initramfs.cpio, where invirtio install looks.

set -euo pipefail

ROOT=$(realpath "$(dirname "$0")/..")
SRC=$ROOT/linux-uml/linux
OUT=$(realpath -m "${1:-$ROOT/linux-uml/build-invirtio}")

make -C "$SRC" ARCH=um O="$OUT" x86_64_defconfig
(cd "$SRC" && ARCH=um scripts/kconfig/merge_config.sh -m -O "$OUT" \
    "$OUT/.config" "$ROOT/configs/driver-guest.config" \
    "$ROOT/configs/wlan-guest.config" "$ROOT/configs/net-guest.config")
make -C "$SRC" ARCH=um O="$OUT" olddefconfig
make -C "$SRC" ARCH=um O="$OUT" -j"$(nproc)"
"$ROOT/scripts/mkinitramfs.sh" -d mt7921e -d cfg80211 \
    -i "$ROOT/scripts/invirtio-guest-init" "$OUT" "$OUT/initramfs"
