#!/usr/bin/env bash
# Return a platform device from vfio-platform to its native host driver.
#
# Reverse of vfio-platform-bind.sh.

set -euo pipefail

PROG=${0##*/}

usage() {
    cat >&2 <<EOF
usage: $PROG <platform-device>

Clear the vfio-platform override on a platform device and let the kernel
rebind it to whichever driver matches, e.g. handing an xHCI controller
back to xhci-hcd.

Stop whatever is using the device first; a process holding /dev/vfio/<group>
open will keep it claimed.
EOF
    exit 2
}

[ $# -eq 1 ] || usage
NAME=$1

DEV=/sys/bus/platform/devices/$NAME

[ -d "$DEV" ] || { echo "$PROG: no such platform device: $NAME" >&2; exit 1; }
[ "$(id -u)" -eq 0 ] || { echo "$PROG: must run as root" >&2; exit 1; }

driver_of() {
    local d
    d=$(readlink -f "$1/driver" 2>/dev/null) || return 0
    [ -n "$d" ] && basename "$d"
}

CURRENT=$(driver_of "$DEV")

if [ -n "${CURRENT:-}" ] && [ "$CURRENT" != "vfio-platform" ]; then
    echo "$PROG: $NAME is bound to $CURRENT, not vfio-platform; nothing to do"
    exit 0
fi

# Warn if the group node is still open, since the unbind will fail in a
# way that is easy to misread.
if [ -e "$DEV/iommu_group" ]; then
    GROUP=$(basename "$(readlink -f "$DEV/iommu_group")")
    holders=$(
        for fd in /proc/[0-9]*/fd/*; do
            tgt=$(readlink "$fd" 2>/dev/null) || continue
            [ "$tgt" = "/dev/vfio/$GROUP" ] || continue
            pid=${fd#/proc/}; pid=${pid%%/*}
            printf '%s ' "$pid"
        done 2>/dev/null | tr ' ' '\n' | sort -u | tr '\n' ' '
    )
    if [ -n "${holders// /}" ]; then
        echo "$PROG: /dev/vfio/$GROUP is still open by pid(s): $holders" >&2
        echo "$PROG: stop them first" >&2
        exit 1
    fi
fi

# Clearing the override has to come first, or drivers_probe just picks
# vfio-platform straight back up.
echo > "$DEV/driver_override"

if [ -n "${CURRENT:-}" ]; then
    echo "$PROG: unbinding from $CURRENT"
    echo "$NAME" > "$DEV/driver/unbind"
fi

echo "$NAME" > /sys/bus/platform/drivers_probe || true

NEW=$(driver_of "$DEV")

if [ -z "${NEW:-}" ]; then
    echo "$PROG: $NAME is now unbound; no native driver matched it" >&2
    exit 1
fi

echo "$PROG: $NAME bound to $NEW"
