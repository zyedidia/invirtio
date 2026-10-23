#!/usr/bin/env bash
# Steer the host interrupts of a VFIO device to chosen host CPUs.

set -euo pipefail

PROG=${0##*/}

usage() {
    cat >&2 <<EOF
usage: $PROG [--once] <pci-address> <cpu list>

Set the affinity of the host interrupts that vfio-pci requested for the
device, which are named vfio-msi[N](<pci-address>) or vfio-msix[N](...) in
/proc/interrupts, to the given host CPUs, for example 4 or 4-5.

The interrupts only exist while the guest has MSI or MSI-X enabled, and
they are requested again when the guest switches modes or reloads its
driver. So by default $PROG keeps running, sets the affinity of each
interrupt when it appears, and exits when it is interrupted. Start it before
or after the guest. With --once, it sets the affinity of the interrupts that
exist now and exits, failing if there are none.

Pin the guest CPU that handles the interrupts (cpu_pin=) to the same host
CPU, so that the host interrupt wakes the CPU that the guest runs on.
EOF
    exit 2
}

ONCE=0
if [ "${1:-}" = "--once" ]; then
    ONCE=1
    shift
fi

[ $# -eq 2 ] || usage
ADDR=$1
CPUS=$2

if ! [[ $ADDR =~ ^[0-9a-fA-F]{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-7]$ ]]; then
    echo "$PROG: '$ADDR' is not a PCI address like 0000:01:00.0" >&2
    exit 1
fi
# The kernel prints PCI addresses in lowercase.
ADDR=${ADDR,,}

if ! [[ $CPUS =~ ^[0-9]+(-[0-9]+)?(,[0-9]+(-[0-9]+)?)*$ ]]; then
    echo "$PROG: '$CPUS' is not a CPU list like 4 or 4-5" >&2
    exit 1
fi

[ -w /proc/irq/default_smp_affinity ] || {
    echo "$PROG: setting IRQ affinity needs root" >&2
    exit 1
}

# Print the IRQ numbers of the device's VFIO interrupts.
device_irqs() {
    awk -v addr="($ADDR)" '
        $NF ~ /^vfio-msix?\[/ && index($NF, addr) { sub(":", "", $1); print $1 }
    ' /proc/interrupts
}

# The CPU list as the kernel prints it, for example 4-5 for 4,5. Set after
# the first write.
TARGET=$CPUS

# Set the affinity of every device interrupt that does not have it yet, and
# write how many interrupts the device has to fd 3.
apply() {
    local irq cur n=0

    # An interrupt can be freed at any point, so failed reads skip it.
    for irq in $(device_irqs); do
        n=$((n + 1))
        cur=$(cat "/proc/irq/$irq/smp_affinity_list" 2>/dev/null) || continue
        [ "$cur" = "$TARGET" ] && continue
        if echo "$CPUS" > "/proc/irq/$irq/smp_affinity_list" 2>/dev/null; then
            cur=$(cat "/proc/irq/$irq/smp_affinity_list" 2>/dev/null) ||
                continue
            TARGET=$cur
            echo "$PROG: IRQ $irq -> CPU $TARGET" >&2
        else
            echo "$PROG: failed to set the affinity of IRQ $irq" >&2
        fi
    done
    echo "$n" >&3
}

if [ "$ONCE" -eq 1 ]; then
    [ "$(apply 3>&1)" -gt 0 ] || {
        echo "$PROG: $ADDR has no VFIO interrupts" >&2
        exit 1
    }
    exit 0
fi

echo "$PROG: watching the VFIO interrupts of $ADDR, stop with Ctrl-C" >&2
while true; do
    apply 3> /dev/null
    sleep 0.2
done
