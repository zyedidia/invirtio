#!/bin/sh
# Benchmark VirtIO block devices exported through VDUSE with fio, against
# null_blk on the host. Run as root.
#
# usage: vduse-bench.sh <output dir> <initramfs> <target>...
#
# Each target is one of:
#   native             null_blk on the host;
#   qsd                qemu-storage-daemon exporting a null-co node with its
#                      vduse-blk export;
#   uml:<name>:<path>[:<options>]
#                      a UML guest from the build directory <path>, exporting
#                      its own null_blk through vduse_uml. <name> names the
#                      results, and <options> are added to the device's,
#                      for example bounce=guest.
#
# Every target gets one 256-entry queue, a 1 GiB disk with 512-byte blocks
# and the none scheduler. fio runs on host CPU FIO_CPU (default 2), and the
# device process on DEV_CPU (default 4). VDUSE injects each queue's
# interrupts from a kworker on IRQ_CPU (default 3). With IRQ_CPU empty,
# VDUSE's default applies, which moves to the next CPU on every injection.
# Each of the seven jobs runs REPS times (default 3) for RUNTIME seconds
# (default 5) after a 1-second ramp.
#
# The output directory gets a directory per target, with fio's JSON for each
# run, and for qsd and UML the process's user and system CPU ticks before and
# after the run (<job>-<rep>.ticks). UML targets also get /proc/vduse_uml
# before and after each run. vduse-bench-summary.py prints the results.

set -u

if [ $# -lt 3 ]; then
    sed -n '4,28s/^# \{0,1\}//p' "$0" >&2
    exit 2
fi

ROOT=$(realpath "$(dirname "$0")/..")
OUT=$1
INITRD=$(realpath "$2")
shift 2
UMID=vduse-bench
MC="$ROOT/build/uml-mconsole $UMID"
FIO_CPU=${FIO_CPU:-2}
DEV_CPU=${DEV_CPU:-4}
IRQ_CPU=${IRQ_CPU-3}
REPS=${REPS:-3}
RUNTIME=${RUNTIME:-5}

mkdir -p "$OUT"
OUT=$(realpath "$OUT")

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

cleanup() {
    vdpa dev del vqsd 2>/dev/null
    vdpa dev del vuml 2>/dev/null
    [ -n "${QSD:-}" ] && kill "$QSD" 2>/dev/null
    [ -n "${PID:-}" ] && kill -9 "$PID" 2>/dev/null
    sleep 1
    python3 "$ROOT/scripts/vduse-destroy.py" vqsd 2>/dev/null
    python3 "$ROOT/scripts/vduse-destroy.py" vuml 2>/dev/null
    modprobe -r null_blk 2>/dev/null
}
trap cleanup EXIT

wait_for() {
    i=0
    while ! eval "$1"; do
        i=$((i + 1))
        [ $i -gt 60 ] && return 1
        sleep 0.5
    done
}

# Attach a VDUSE device, pin its queues' interrupt injection to IRQ_CPU,
# and print its host block device.
attach() {
    vdpa dev add name "$1" mgmtdev vduse || fail "vdpa dev add $1"
    wait_for "ls /sys/bus/vdpa/devices/$1/virtio*/block >/dev/null 2>&1" ||
        fail "no block device for $1"
    d=/dev/$(ls /sys/bus/vdpa/devices/$1/virtio*/block | head -1)
    wait_for "[ -b $d ]" || fail "no $d"
    for f in /sys/class/vduse/$1/vq*/irq_cb_affinity; do
        [ -n "$IRQ_CPU" ] && printf '%x\n' $((1 << IRQ_CPU)) > "$f"
        echo "$f $(cat "$f")" >> "$OUT/irq_cb_affinity"
    done
    echo "$d"
}

# Print the user and system CPU ticks of a process.
cputicks() {
    awk '{ print $14, $15 }' /proc/$1/stat
}

# Run every job against a device. A UML target saves /proc/vduse_uml, and a
# target with a device process saves that process's CPU time.
run_jobs() {
    target=$1
    dev=$2
    counters=$3
    pid=$4
    dir=$OUT/$target
    mkdir -p "$dir"
    echo none > /sys/block/${dev#/dev/}/queue/scheduler

    for rep in $(seq $REPS); do
        for job in \
            "lat-qd1 randread 4k 1" \
            "iops-qd32 randread 4k 32" \
            "iops-qd128 randread 4k 128" \
            "seqread-128k read 128k 8" \
            "seqread-1m read 1m 8" \
            "seqwrite-128k write 128k 8" \
            "seqwrite-1m write 1m 8"; do
            set -- $job
            run=$1-$rep
            [ "$counters" = yes ] && $MC proc vduse_uml > "$dir/$run.before"
            [ -n "$pid" ] && t0=$(cputicks $pid)
            taskset -c $FIO_CPU fio --name="$1" --filename="$dev" \
                --rw="$2" --bs="$3" --iodepth="$4" \
                --direct=1 --ioengine=io_uring --time_based \
                --runtime=$RUNTIME --ramp_time=1 --norandommap \
                --randrepeat=0 --output-format=json \
                --output="$dir/$run.json" >/dev/null ||
                fail "fio $target $run"
            [ -n "$pid" ] && echo "$t0 $(cputicks $pid)" > "$dir/$run.ticks"
            [ "$counters" = yes ] && $MC proc vduse_uml > "$dir/$run.after"
        done
    done
}

run_native() {
    modprobe -r null_blk 2>/dev/null
    modprobe null_blk nr_devices=1 gb=1 hw_queue_depth=256 submit_queues=1 \
        memory_backed=0 || fail "null_blk"
    wait_for "[ -b /dev/nullb0 ]" || fail "no /dev/nullb0"
    run_jobs native /dev/nullb0 no ""
    modprobe -r null_blk
}

run_qsd() {
    taskset -c $DEV_CPU qemu-storage-daemon \
        --blockdev driver=null-co,node-name=null0,size=1073741824,read-zeroes=off \
        --export type=vduse-blk,id=exp0,node-name=null0,name=vqsd,writable=on,num-queues=1,queue-size=256 \
        > "$OUT/qsd.log" 2>&1 &
    QSD=$!
    wait_for "[ -e /dev/vduse/vqsd ]" || fail "no /dev/vduse/vqsd"
    run_jobs qsd "$(attach vqsd)" no $QSD
    vdpa dev del vqsd
    kill "$QSD"
    wait "$QSD" 2>/dev/null
    QSD=
}

run_uml() {
    # VDUSE counts bounce=guest's guest pages against RLIMIT_MEMLOCK.
    ulimit -l unlimited
    "$2/linux" umid=$UMID mem=512M cpu_pin=$DEV_CPU initrd="$INITRD" \
        null_blk.nr_devices=1 null_blk.gb=1 null_blk.hw_queue_depth=256 \
        null_blk.submit_queues=1 null_blk.memory_backed=0 \
        vduse_uml.device=blk,vuml,disk=/dev/nullb0,log="$OUT/$1.inflight"${3:+,$3} \
        </dev/null >"$OUT/$1.guest.log" 2>&1 &
    PID=$!
    wait_for "[ -e /dev/vduse/vuml ]" || fail "no /dev/vduse/vuml"
    dev=$(attach vuml)
    grep -E "^CONFIG_(SMP|LOCKDEP|PROVE_LOCKING)=" "$2/.config" \
        > "$OUT/$1.config"
    run_jobs "$1" "$dev" yes $PID
    vdpa dev del vuml
    kill -9 $PID
    wait $PID 2>/dev/null
    PID=
    sleep 1
    python3 "$ROOT/scripts/vduse-destroy.py" vuml
    rm -f "$OUT/$1.inflight"
}

{
    uname -a
    echo "governor $(cat /sys/devices/system/cpu/cpu$DEV_CPU/cpufreq/scaling_governor)"
    echo "boost $(cat /sys/devices/system/cpu/cpufreq/boost 2>/dev/null)"
    echo "clk_tck $(getconf CLK_TCK)"
    echo "fio_cpu $FIO_CPU dev_cpu $DEV_CPU irq_cpu ${IRQ_CPU:-default} reps $REPS runtime $RUNTIME"
    fio --version
    qemu-storage-daemon --version | head -1
    git -C "$ROOT/linux-uml/linux" log -1 --format='%h %s'
    git -C "$ROOT/linux-uml/linux" diff --quiet HEAD ||
        echo "uncommitted changes in linux-uml/linux"
} > "$OUT/host"

modprobe -a vduse vdpa virtio_vdpa virtio_blk || fail modprobe
python3 "$ROOT/scripts/vduse-destroy.py" vqsd 2>/dev/null
python3 "$ROOT/scripts/vduse-destroy.py" vuml 2>/dev/null

: > "$OUT/targets"
for t in "$@"; do
    case $t in
    native)
        run_native
        echo native >> "$OUT/targets"
        ;;
    qsd)
        run_qsd
        echo qsd >> "$OUT/targets"
        ;;
    uml:*:*)
        rest=${t#uml:}
        name=${rest%%:*}
        rest=${rest#*:}
        path=${rest%%:*}
        opts=
        [ "$rest" != "$path" ] && opts=${rest#*:}
        run_uml "$name" "$(realpath "$path")" "$opts"
        echo "$name" >> "$OUT/targets"
        ;;
    *)
        fail "unknown target $t"
        ;;
    esac
done
