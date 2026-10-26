#!/bin/sh
# Test a UML guest's VDUSE block device for correctness and across the
# guest's lifecycle (V2 and V8 in docs/VDUSE.md). Run as root.
#
# usage: vduse-test.sh <output dir> <initramfs> <UML build dir> [<test>...]
#
# The guest serves a 256 MiB ubd disk backed by a file in the output
# directory, so that its data survives relaunches. The tests run in this
# order, all of them by default:
#   io       fio with verify=crc32c over the whole disk, then mkfs.ext4, a
#            mount, a copy and a checksum after the page cache is dropped;
#   rebind   unbinding virtio_blk and binding it again, then vdpa dev del
#            and add, with the data compared after each;
#   stop     SIGTERM after vdpa dev del, which leaves no VDUSE device;
#   kill     kill -9 while fio writes and verifies at 2000 IOPS, and a
#            relaunch with the same name and log, after which fio finishes
#            without errors;
#   term     SIGTERM while the device is on the vDPA bus, and a relaunch
#            that takes it over;
#   unused   kill -9 before the host attaches the device, and a relaunch
#            that destroys it and creates it again;
#   timeout  a message timeout while no guest runs, after which a relaunched
#            guest reports that the device is broken.
#
# Each guest's console goes to guest-<n>.log in the output directory. The
# script fails if a guest logs a kernel warning, so a build with lockdep
# also checks the locking.

set -u

if [ $# -lt 3 ]; then
    sed -n '4,26s/^# \{0,1\}//p' "$0" >&2
    exit 2
fi

ROOT=$(realpath "$(dirname "$0")/..")
mkdir -p "$1"
OUT=$(realpath "$1")
INITRD=$(realpath "$2")
BUILD=$(realpath "$3")
shift 3
TESTS=${*:-io rebind stop kill term unused timeout}

NAME=vtest
UMID=vduse-test
DISK=$OUT/disk.img
LOG=$OUT/$NAME.inflight
DATA=$OUT/data
N=0
PID=

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

cleanup() {
    vdpa dev del $NAME 2>/dev/null
    [ -n "$PID" ] && kill -9 "$PID" 2>/dev/null
    sleep 1
    python3 "$ROOT/scripts/vduse-destroy.py" $NAME 2>/dev/null
    umount "$OUT/mnt" 2>/dev/null
}
trap cleanup EXIT

wait_for() {
    i=0
    while ! eval "$1"; do
        i=$((i + 1))
        [ $i -gt 120 ] && return 1
        sleep 0.5
    done
}

# Start a guest and wait until it has created or reconnected to the device.
start() {
    N=$((N + 1))
    GLOG=$OUT/guest-$N.log
    "$BUILD/linux" umid=$UMID mem=512M initrd="$INITRD" ubd0="$DISK" \
        vduse_guest.device=blk,$NAME,disk=/dev/ubda,log="$LOG" \
        </dev/null >"$GLOG" 2>&1 &
    PID=$!
    wait_for "grep -q '$NAME: \(created\|reconnecting\|the device stopped\)' $GLOG" ||
        fail "guest $N did not reach the device"
}

# Stop the guest with a signal and wait for it to exit.
stop() {
    kill "-$1" "$PID"
    wait "$PID" 2>/dev/null
    PID=
}

# Print the host block device of the attached device.
dev() {
    echo "/dev/$(ls /sys/bus/vdpa/devices/$NAME/virtio*/block | head -1)"
}

attach() {
    vdpa dev add name $NAME mgmtdev vduse || fail "vdpa dev add"
    wait_for "ls /sys/bus/vdpa/devices/$NAME/virtio*/block >/dev/null 2>&1" ||
        fail "no block device"
    wait_for "[ -b $(dev) ]" || fail "no $(dev)"
    udevadm settle
}

# Write the reference data to the start of the disk, or compare it.
write_data() {
    dd if="$DATA" of="$(dev)" bs=1M oflag=direct status=none ||
        fail "writing the data"
}

check_data() {
    dd if="$(dev)" bs=1M count=64 iflag=direct status=none | cmp -s - "$DATA" ||
        fail "the data differs after $1"
}

expect_log() {
    grep -q "$1" "$GLOG" || fail "guest $N did not log '$1'"
}

t_io() {
    start
    attach
    fio --name=verify --filename="$(dev)" --rw=randwrite --bs=4k \
        --iodepth=16 --ioengine=io_uring --direct=1 --verify=crc32c \
        --verify_fatal=1 --verify_state_save=0 --output="$OUT/io-fio.log" ||
        fail "fio verify"
    mkfs.ext4 -F -q "$(dev)" || fail "mkfs.ext4"
    mkdir -p "$OUT/mnt"
    mount "$(dev)" "$OUT/mnt" || fail "mount"
    cp "$DATA" "$OUT/mnt/data" && sync || fail "copy"
    umount "$OUT/mnt"
    echo 3 > /proc/sys/vm/drop_caches
    mount "$(dev)" "$OUT/mnt" || fail "mount again"
    cmp -s "$DATA" "$OUT/mnt/data" || fail "the copy differs"
    umount "$OUT/mnt"
}

t_rebind() {
    [ -n "$PID" ] || { start; attach; }
    write_data
    v=$(basename /sys/bus/vdpa/devices/$NAME/virtio*)
    echo "$v" > /sys/bus/virtio/drivers/virtio_blk/unbind ||
        fail "unbind"
    echo "$v" > /sys/bus/virtio/drivers/virtio_blk/bind || fail "bind"
    wait_for "ls /sys/bus/vdpa/devices/$NAME/virtio*/block >/dev/null 2>&1" ||
        fail "no block device after bind"
    udevadm settle
    check_data "binding virtio_blk again"
    vdpa dev del $NAME || fail "vdpa dev del"
    attach
    check_data "vdpa dev del and add"
}

t_stop() {
    [ -n "$PID" ] || { start; attach; }
    vdpa dev del $NAME || fail "vdpa dev del"
    stop TERM
    expect_log "$NAME: destroyed the device"
    [ -e /dev/vduse/$NAME ] && fail "the device is left after SIGTERM"
    [ -e "$LOG" ] && fail "the log is left after SIGTERM"
}

t_kill() {
    [ -n "$PID" ] || { start; attach; }
    fio --name=kill --filename="$(dev)" --rw=randwrite --bs=4k --size=64M \
        --iodepth=16 --ioengine=io_uring --direct=1 --rate_iops=2000 \
        --verify=crc32c --verify_fatal=1 --verify_state_save=0 \
        --output="$OUT/kill-fio.log" &
    FIO=$!
    sleep 3
    stop 9
    start
    expect_log "$NAME: reconnecting to the blk device"
    wait $FIO || fail "fio across kill -9"
    grep "requests to replay" "$GLOG"
}

t_term() {
    [ -n "$PID" ] || { start; attach; }
    write_data
    stop TERM
    expect_log "stays on the host's vDPA bus"
    start
    expect_log "$NAME: reconnecting to the blk device"
    check_data "SIGTERM and a relaunch"
}

t_unused() {
    [ -n "$PID" ] || start
    vdpa dev del $NAME 2>/dev/null
    stop 9
    start
    expect_log "$NAME: created blk device"
    attach
    check_data "a relaunch before attaching"
}

t_timeout() {
    [ -n "$PID" ] || { start; attach; }
    echo 2 > /sys/class/vduse/$NAME/msg_timeout
    stop 9
    v=$(basename /sys/bus/vdpa/devices/$NAME/virtio*)
    echo "$v" > /sys/bus/virtio/drivers/virtio_blk/unbind
    start
    expect_log "the device stopped working after a message timed out"
    stop 9
    vdpa dev del $NAME || fail "vdpa dev del of the broken device"
}

modprobe -a vduse vdpa virtio_vdpa virtio_blk || fail modprobe
ulimit -l unlimited
python3 "$ROOT/scripts/vduse-destroy.py" $NAME 2>/dev/null
rm -f "$LOG"
truncate -s 256M "$DISK"
head -c 64M /dev/urandom > "$DATA"

for t in $TESTS; do
    echo "== $t"
    "t_$t"
done

[ -n "$PID" ] && { vdpa dev del $NAME 2>/dev/null; stop TERM; }
if grep -l "WARNING:\|BUG:\|possible circular\|possible recursive" "$OUT"/guest-*.log; then
    fail "a guest logged a kernel warning"
fi
echo "PASS: $TESTS"
