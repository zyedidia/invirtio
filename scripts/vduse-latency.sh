#!/bin/sh
# Split the round trip of a request to a UML VDUSE device into its stages,
# and histogram each in nanoseconds. Run as root, with one request in flight
# at a time, for example fio at queue depth 1.
#
# usage: vduse-latency.sh <UML binary> <UML pid> [seconds]
#
# The stages, from the host driver's kick to its next one:
#   @kick    host kick (vduse_vdpa_kick_vq) to the guest's kick handler;
#   @guest   kick handler to the guest's VDUSE_VQ_INJECT_IRQ ioctl, which
#            covers taking, serving and completing the request;
#   @wakeup  that ioctl to the start of VDUSE's injection work, which runs
#            in a kworker;
#   @inject  injection work to the host driver's vring_interrupt;
#   @host    vring_interrupt to the next kick, which covers the host's
#            completion, the application and its next submission.
# @round is the whole round trip. @kick is split by whether the SIGIO helper
# signalled the guest in between (@kick_by[1]) or not (@kick_by[0]).
#
# One uprobe, on the kick handler, adds the cost of a trap to each round trip.
# The other stages use kprobes and tracepoints. Without a duration, the
# script runs until it is interrupted.

set -eu

if [ $# -lt 2 ]; then
    sed -n '4,22s/^# \{0,1\}//p' "$0" >&2
    exit 2
fi

BIN=$(realpath "$1")
PID=$2
END=""
[ $# -ge 3 ] && END="interval:s:$3 { exit(); }"

# _IOW(0x81, 0x17, __u32)
INJECT=0x40048117

exec bpftrace -e "
kprobe:vduse_vdpa_kick_vq {
	if (@t_cb) {
		@host = hist(nsecs - @t_cb);
		@t_cb = 0;
	}
	if (@t_kick0) {
		@round = hist(nsecs - @t_kick0);
	}
	@t_kick0 = nsecs;
	@t_kick = nsecs;
	@signalled = 0;
	@count[\"host kick\"] = count();
}
tracepoint:syscalls:sys_enter_tgkill /args.sig == 29 && pid == $PID/ {
	if (@t_kick) {
		@signalled = 1;
	}
}
uprobe:$BIN:uml_vduse_kick_interrupt {
	if (@t_kick) {
		@kick = hist(nsecs - @t_kick);
		@kick_by[@signalled] = hist(nsecs - @t_kick);
		@t_kick = 0;
		@t_handler = nsecs;
	}
	@count[\"guest kick handler\"] = count();
}
tracepoint:syscalls:sys_enter_ioctl /pid == $PID && args.cmd == $INJECT/ {
	if (@t_handler) {
		@guest = hist(nsecs - @t_handler);
		@t_handler = 0;
	}
	@t_ioctl = nsecs;
	@count[\"guest inject\"] = count();
}
kprobe:vduse_vq_irq_inject {
	if (@t_ioctl) {
		@wakeup = hist(nsecs - @t_ioctl);
		@t_ioctl = 0;
		@t_work = nsecs;
	}
	@count[\"host injection work\"] = count();
}
kprobe:vring_interrupt {
	if (@t_work) {
		@inject = hist(nsecs - @t_work);
		@t_work = 0;
		@t_cb = nsecs;
	}
	@count[\"host vring_interrupt\"] = count();
}
$END
END {
	clear(@t_kick0); clear(@t_kick); clear(@t_handler); clear(@t_ioctl);
	clear(@t_work); clear(@t_cb); clear(@signalled);
}"
