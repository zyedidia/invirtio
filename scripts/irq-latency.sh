#!/bin/sh
# Histogram the time from the host's VFIO interrupt handler to the UML guest's
# handler, split by how the interrupt reached the guest. Stop with Ctrl-C.
#
# usage: irq-latency.sh [path to the UML binary]
#
# Latency is measured from the last host interrupt before each guest handler
# call:
#   @lat[1]  the CPU left the idle wait on the IRQ epoll set;
#   @lat[2]  the SIGIO helper thread signalled the CPU (tgkill with SIGIO);
#   @lat[0]  neither, so the CPU was already handling interrupts.
#
# @wake[1] and @wake[2] histogram the time from the host interrupt to that
# wakeup. The counts show how often each probe fired at all, including events
# that are not tied to a measured interrupt.
#
# The idle path is detected on the return from sigio_idle_exit, since x86
# uprobes cannot be placed on mark_sigio_pending, whose first instruction has
# a lock prefix. Only one start time is kept, so this assumes one active
# vector.

set -eu

BIN=$(realpath "${1:-$(dirname "$0")/../linux-uml/build-a1/linux}")

exec bpftrace -e "
kprobe:vfio_msihandler {
	@count[\"host interrupt\"] = count();
	if (@start) {
		@count[\"host interrupt before guest handler ran\"] = count();
	}
	@start = nsecs;
	@path = 0;
}
uprobe:$BIN:sigio_idle_enter {
	@count[\"idle wait on IRQ epoll set\"] = count();
}
uretprobe:$BIN:sigio_idle_exit {
	@count[\"idle wait exit\"] = count();
	if (@start) {
		@path = 1;
		@wake[1] = hist(nsecs - @start);
	}
}
tracepoint:syscalls:sys_enter_tgkill /args.sig == 29/ {
	@count[\"helper tgkill\"] = count();
	if (@start) {
		@path = 2;
		@wake[2] = hist(nsecs - @start);
	}
}
uprobe:$BIN:uml_vfio_interrupt {
	@count[\"guest handler\"] = count();
	if (@start) {
		@lat[@path] = hist(nsecs - @start);
		@start = 0;
	}
}
END { clear(@start); clear(@path); }"
