#!/usr/bin/env bash
# Detach a platform device from its host driver and bind it to
# vfio-platform.
#
# Reverse with vfio-platform-unbind.sh.
#
# SPDX-License-Identifier: GPL-2.0-or-later

set -euo pipefail

PROG=${0##*/}

usage() {
    cat >&2 <<EOF
usage: $PROG [--no-reset] <platform-device>

Bind a platform device, e.g. an ACPI xHCI controller such as NVDA8000:02,
to vfio-platform so a userspace driver can claim it. See
/sys/bus/platform/devices for the names.

vfio-platform refuses devices it cannot reset; for ACPI devices that means
one with an _RST method. --no-reset loads vfio-platform with
reset_required=0 so a device without _RST can be bound (the module is
reloaded if it is loaded with the other setting and nothing uses it).

Refuses to act on a USB host controller with devices attached, so the
controller your keyboard or dock hangs off cannot be taken away by
accident.
EOF
    exit 2
}

NO_RESET=0
if [ "${1:-}" = "--no-reset" ]; then
    NO_RESET=1
    shift
fi

[ $# -eq 1 ] || usage
NAME=$1

DEV=/sys/bus/platform/devices/$NAME

[ -d "$DEV" ] || { echo "$PROG: no such platform device: $NAME" >&2; exit 1; }

driver_of() {
    local d
    d=$(readlink -f "$1/driver" 2>/dev/null) || return 0
    [ -n "$d" ] && basename "$d"
}

# ---- refuse if anything behind the device is in use -------------------

in_use=""

# USB devices hang off the root hubs (usbN) directly under the controller.
# Devices are the N-... directories with a devnum; that skips interfaces
# (N-...:c.i) and hub port nodes (N-...-portP).
for hub in "$DEV"/usb[0-9]*; do
    [ -d "$hub" ] || continue
    bus=${hub##*/usb}
    while read -r d; do
        n=${d##*/}
        [ -e "$d/devnum" ] || continue
        prod=$(cat "$d/product" 2>/dev/null || echo "?")
        in_use+="  USB device $n ($prod) is attached"$'\n'
    done < <(find "$hub" -maxdepth 4 -type d -name "$bus-*" 2>/dev/null)
done

if [ -n "$in_use" ]; then
    echo "$PROG: refusing to bind $NAME, it is in use:" >&2
    printf '%s' "$in_use" >&2
    echo "$PROG: unplug those first" >&2
    exit 1
fi

[ "$(id -u)" -eq 0 ] || { echo "$PROG: must run as root" >&2; exit 1; }

# ---- report the IOMMU group ------------------------------------------

if [ ! -e "$DEV/iommu_group" ]; then
    echo "$PROG: $NAME has no IOMMU group; is an iommu enabled?" >&2
    exit 1
fi

GROUP=$(basename "$(readlink -f "$DEV/iommu_group")")
echo "$PROG: $NAME is in IOMMU group $GROUP"

for peer in /sys/kernel/iommu_groups/"$GROUP"/devices/*; do
    pname=$(basename "$peer")
    [ "$pname" = "$NAME" ] && continue
    echo "  group peer $pname (driver=$(driver_of "$peer")) will" \
         "need to be bound too" >&2
done

# ---- load vfio-platform with the requested reset policy ---------------

CURRENT=$(driver_of "$DEV")

if [ "${CURRENT:-}" = "vfio-platform" ]; then
    echo "$PROG: $NAME is already bound to vfio-platform"
    exit 0
fi

WANT=$([ $NO_RESET -eq 1 ] && echo N || echo Y)
PARAM=/sys/module/vfio_platform/parameters/reset_required

# reset_required is read-only once loaded, so a mismatch means reloading.
if [ -e "$PARAM" ] && [ "$(cat "$PARAM")" != "$WANT" ]; then
    if compgen -G "/sys/bus/platform/drivers/vfio-platform/*:*" >/dev/null; then
        echo "$PROG: vfio-platform is loaded with reset_required=$(cat "$PARAM")" \
             "and has devices bound; unbind them first" >&2
        exit 1
    fi
    echo "$PROG: reloading vfio-platform with reset_required=$WANT"
    modprobe -r vfio-platform
fi

# Kernels built with IOMMUFD_VFIO_CONTAINER serve /dev/vfio/vfio from
# iommufd and have no type1 module.
modprobe -q vfio_iommu_type1 || true
modprobe vfio-platform reset_required=$([ $NO_RESET -eq 1 ] && echo 0 || echo 1)

# ---- bind -------------------------------------------------------------

# driver_override first, so drivers_probe cannot hand it back to the
# native driver in the window after unbind.
echo vfio-platform > "$DEV/driver_override"

if [ -n "${CURRENT:-}" ]; then
    echo "$PROG: unbinding from $CURRENT"
    echo "$NAME" > "$DEV/driver/unbind"
fi

echo "$NAME" > /sys/bus/platform/drivers_probe || true

NEW=$(driver_of "$DEV")
if [ "${NEW:-}" != "vfio-platform" ]; then
    echo "$PROG: bind failed, driver is now '${NEW:-none}'" >&2
    dmesg | grep -i vfio | tail -n 3 >&2 || true
    if [ $NO_RESET -eq 0 ]; then
        echo "$PROG: if the device has no reset function, retry with" \
             "--no-reset" >&2
    fi
    echo "$PROG: vfio-platform-unbind.sh $NAME returns it to the host" >&2
    exit 1
fi

echo "$PROG: $NAME bound to vfio-platform"
ls -l "/dev/vfio/$GROUP"
