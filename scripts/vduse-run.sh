#!/usr/bin/env bash
# Run a UML guest that serves a VDUSE device, attach the device to the host
# once the guest has created it, and remove and destroy the device when the
# guest exits, however it exits. Run as root.
#
# usage: vduse-run.sh <device name> <guest command>...
#
# The guest's console stays on the terminal and is also written to
# $VDUSE_RUN_LOG, by default /tmp/vduse-run-<name>.log. A device that an
# earlier guest left behind is removed first, since a wlan device cannot be
# taken over. When nothing serves the device any more, removing it waits for
# VDUSE's message timeout, so the timeout is lowered first.

set -u

if [ $# -lt 2 ]; then
    sed -n '5,12s/^# \{0,1\}//p' "$0" >&2
    exit 2
fi
[ "$(id -u)" -eq 0 ] || { echo "${0##*/}: must run as root" >&2; exit 1; }

NAME=$1
shift
ROOT=$(realpath "$(dirname "$0")/..")
LOG=${VDUSE_RUN_LOG:-/tmp/vduse-run-$NAME.log}

lower_timeout() {
    echo 1 2>/dev/null > "/sys/class/vduse/$NAME/msg_timeout"
}

cleanup() {
    if [ -e "/sys/bus/vdpa/devices/$NAME" ]; then
        lower_timeout
        vdpa dev del "$NAME"
    fi
    python3 "$ROOT/scripts/vduse-destroy.py" "$NAME" 2>/dev/null
}

# The watcher stops once the guest has exited. A lowered timeout keeps an add
# that it started from waiting long. Signals are ignored from here on, so
# that the device is always removed.
finish() {
    trap '' INT TERM HUP
    [ -n "$TTY" ] && stty "$TTY"
    rm -f "$RUNNING"
    lower_timeout
    [ -n "$WATCH" ] && wait "$WATCH" 2>/dev/null
    cleanup
}

modprobe -a vduse vdpa virtio_vdpa
cleanup
: > "$LOG"
# A guest that is killed leaves the terminal raw.
TTY=
[ -t 0 ] && TTY=$(stty -g)
GUEST=
WATCH=
RUNNING=$(mktemp)
trap finish EXIT
trap 'if [ -n "$GUEST" ]; then kill -TERM "$GUEST"; else exit 1; fi' \
    INT TERM HUP

# The guest names the device in its console once it has created it.
(
    trap '' HUP
    while [ -e "$RUNNING" ]; do
        if grep -qF "$NAME: created" "$LOG"; then
            vdpa dev add name "$NAME" mgmtdev vduse
            exit
        fi
        sleep 0.1
    done
) &
WATCH=$!

# The guest runs in the background, so that the script can pass it the
# signals it gets while it waits. UML powers off in order on SIGTERM. Tee
# outlives the terminal's signals, so that the guest's shutdown is logged.
"$@" <&0 > >(trap '' INT HUP; exec tee -a "$LOG") 2>&1 &
GUEST=$!
while kill -0 "$GUEST" 2>/dev/null; do
    wait "$GUEST"
done
wait "$GUEST"
