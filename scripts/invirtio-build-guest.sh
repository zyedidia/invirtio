#!/usr/bin/env bash
# Build a UML guest and its initramfs from the configuration fragments of a
# target. The targets are:
#   invirtio      the driver guest that invirtio install takes, with the
#                 drivers and VDUSE backends of the WiFi and Ethernet
#                 guests, and the WiFi firmware. invirtio picks the backend
#                 for each device when it starts the device's guest.
#   blk-test      the guest that invirtio-test test-blk runs under the
#                 launcher, which serves a ramdisk.
#   blk-test-ubd  the guest that invirtio-test test-blk runs without the
#                 sandbox, which serves a ubd disk backed by a host file.
#   blk-bench     the guest that invirtio-test bench-blk runs, with and
#                 without the sandbox, which serves its own null_blk.
#   net-test      the guest that invirtio-test test-net runs, which exports
#                 one end of a veth pair, or a port of the X710 or I226-V.
#   net-bench     the guest that invirtio-test bench-net runs, which
#                 exports a port of the X710.
#   wlan-test     the guest that invirtio-test test-wlan runs, which exports
#                 a mac80211_hwsim radio and runs access points on three
#                 others.
#
# usage: invirtio-build-guest.sh [-t <target>] [<build dir>]
#
# The default target is invirtio, and the default build directory is
# linux-uml/build-<target>. The initramfs goes to
# <build dir>/initramfs/initramfs.cpio, where invirtio install and
# invirtio-test look.

set -euo pipefail

# The tools that the test guests take are in sbin, which a user's PATH can
# leave out.
export PATH=$PATH:/usr/sbin:/sbin
ROOT=$(realpath "$(dirname "$0")/..")
SRC=$ROOT/linux-uml/linux
CONFIGS=$ROOT/configs

TARGET=invirtio
while getopts t:h opt; do
    case $opt in
    t) TARGET=$OPTARG ;;
    *) sed -n '2,29s/^# \{0,1\}//p' "$0" >&2; exit 2 ;;
    esac
done
shift $((OPTIND - 1))

case $TARGET in
invirtio)
    FRAGMENTS=(driver-guest wlan-guest net-guest)
    INITRAMFS=(-d mt7921e -d cfg80211 -i "$ROOT/scripts/invirtio-guest-init")
    ;;
blk-test)
    FRAGMENTS=(driver-guest blk-test)
    INITRAMFS=()
    ;;
blk-test-ubd)
    FRAGMENTS=(driver-guest blk-test blk-test-ubd)
    INITRAMFS=()
    ;;
blk-bench)
    FRAGMENTS=(driver-guest blk-bench)
    INITRAMFS=()
    ;;
net-test)
    FRAGMENTS=(driver-guest net-guest net-test lockdep)
    INITRAMFS=(-i "$ROOT/scripts/net-test-init" -b "$(command -v iperf3)"
               -b "$(command -v ip)")
    ;;
net-bench)
    FRAGMENTS=(driver-guest net-guest)
    INITRAMFS=()
    ;;
wlan-test)
    FRAGMENTS=(driver-guest wlan-guest wlan-hwsim lockdep)
    INITRAMFS=(-i "$ROOT/scripts/wlan-hwsim-init" -d cfg80211
               -b "$(command -v hostapd)" -b "$(command -v iperf3)"
               -b "$(command -v iw)")
    ;;
*)
    echo "${0##*/}: unknown target $TARGET" >&2
    exit 2
    ;;
esac

OUT=$(realpath -m "${1:-$ROOT/linux-uml/build-$TARGET}")
PATHS=()
for f in "${FRAGMENTS[@]}"; do
    PATHS+=("$CONFIGS/$f.config")
done

make -C "$SRC" ARCH=um O="$OUT" x86_64_defconfig
(cd "$SRC" && ARCH=um scripts/kconfig/merge_config.sh -m -O "$OUT" \
    "$OUT/.config" "${PATHS[@]}")
make -C "$SRC" ARCH=um O="$OUT" olddefconfig
make -C "$SRC" ARCH=um O="$OUT" -j"$(nproc)"
"$ROOT/scripts/mkinitramfs.sh" "${INITRAMFS[@]}" "$OUT" "$OUT/initramfs"
