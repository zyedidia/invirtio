#!/usr/bin/env python3
# Test the virtio-net backend of docs/VIRTIO_NET.md on the X710, whose two
# ports are cabled to each other. Run as root on the linux-host kernel,
# with iperf3 and ethtool installed.
#
# usage: vduse-net-test.py [--bench] <guest build dir> <initramfs> <output dir>
#
# Port 0 goes to the guest through VFIO, and the guest exports it to the
# host as a virtio-net device. Port 1 stays on the host's i40e, in a network
# namespace, as the far end. The script checks the host interface's address,
# MTU, speed and offloads, pings both ways over IPv4 and from the host over
# IPv6, runs iperf3 in both directions with one and four streams at MTU 9000
# and 1500, checks that the offloads are used, and that taking port 1 down
# and up reaches the host interface's carrier. It then removes and attaches the device again,
# kills the guest during an iperf3 run and relaunches it, and checks that the
# run finishes. It checks that the guest's drop counters are zero and that
# neither kernel logs a warning. With --bench, which needs netperf, it also
# measures TCP throughput, the rate of 64-byte UDP datagrams, netperf's
# TCP_RR and UDP_RR, and ping latency idle and under load, each with the CPU
# use of the host, the guest's UML process and the guest's own CPU. It runs
# the same jobs with both ports on the host's i40e, prints both, and writes
# them to bench.json in the output directory. The guest's console goes to
# guest.log in the output directory.

import glob
import json
import os
import re
import shutil
import signal
import subprocess
import sys
import time

NAME = "vnet-test"
GUEST_PORT = "0000:05:00.0"
PEER_PORT = "0000:05:00.1"
PEER_NS = "vnet-peer"
NATIVE_NS = "vnet-native"
HOST_ADDR = "10.71.0.1"
PEER_ADDR = "10.71.0.2"
JUMBO_MTU = 9000
# The guest's drop and error counters, which stay at zero. Rx_no_buffer and
# rx_dropped are left out, since they grow whenever the host is slow to
# refill its buffers or the guest is short of memory.
GUEST_COUNTERS = ["tx_invalid", "tx_dropped", "rx_oversized",
                  "rx_unsupported", "q0_bad_requests", "q1_bad_requests",
                  "q0_inject_errors", "q1_inject_errors"]
# Messages of the transport, its UML host and the backend that report a
# failure.
GUEST_ERRORS = ("is broken", "dropping a bad request", "cannot ", "is gone")
# Bits of /sys/bus/virtio/devices/*/features, which lists bit 0 first.
FEATURES = {"CSUM": 0, "GUEST_CSUM": 1, "MTU": 3, "MAC": 5,
            "GUEST_TSO4": 7, "GUEST_TSO6": 8, "GUEST_ECN": 9,
            "HOST_TSO4": 11, "HOST_TSO6": 12, "HOST_ECN": 13,
            "MRG_RXBUF": 15, "STATUS": 16, "SPEED_DUPLEX": 63}
SCRIPTS = os.path.dirname(os.path.abspath(__file__))

failures = []


def check(cond, what):
    print(("ok: " if cond else "FAIL: ") + what)
    if not cond:
        failures.append(what)
    return cond


def run(*args, check=False, timeout=None):
    """Run a command. One that outlives the timeout is killed, and its
    result has return code None."""
    try:
        return subprocess.run(args, capture_output=True, text=True,
                              check=check, timeout=timeout)
    except subprocess.TimeoutExpired as e:
        return subprocess.CompletedProcess(args, None, e.stdout or "",
                                           e.stderr or "")


def peer(*args, check=False):
    return run("ip", "netns", "exec", PEER_NS, *args, check=check)


def read(path):
    try:
        with open(path) as f:
            return f.read().strip()
    except OSError:
        return ""


def write(path, text):
    with open(path, "w") as f:
        f.write(text)


def wait(cond, timeout, step=0.2):
    end = time.time() + timeout
    while time.time() < end:
        if cond():
            return True
        time.sleep(step)
    return cond()


def port_netdev(addr):
    nets = glob.glob(f"/sys/bus/pci/devices/{addr}/net/*")
    return os.path.basename(nets[0]) if nets else None


def driver_of(addr):
    return os.path.basename(os.path.realpath(
        f"/sys/bus/pci/devices/{addr}/driver"))


class Guest:
    def __init__(self, build, initrd, out, log):
        self.path = os.path.join(out, "guest.log")
        self.logf = open(self.path, "a")
        self.start = os.path.getsize(self.path)
        self.marks = 0
        cmd = [os.path.join(build, "linux"), f"umid=vnet-{os.getpid()}",
               "mem=1G", f"initrd={initrd}",
               f"vfio_uml.device={GUEST_PORT}",
               f"vduse_guest.device=net,{NAME},netdev=eth0,"
               f"mtu={JUMBO_MTU},log={log}", "init_shell"]
        self.proc = subprocess.Popen(cmd, stdin=subprocess.PIPE,
                                     stdout=self.logf,
                                     stderr=subprocess.STDOUT)

    def log(self):
        with open(self.path, errors="replace") as f:
            f.seek(self.start)
            return f.read()

    def wait_for(self, text, timeout):
        return wait(lambda: text in self.log() or self.proc.poll() is not None,
                    timeout) and text in self.log()

    def output(self, line, timeout=10):
        """Run a shell command and return what it printed."""
        self.marks += 1
        start, end = f"mark-{self.marks}-start", f"mark-{self.marks}-end"
        # The console echoes the input, which holds the markers only split.
        self.proc.stdin.write(f"echo {start[:-1]}''{start[-1]}; {line}; "
                              f"echo {end[:-1]}''{end[-1]}\n".encode())
        self.proc.stdin.flush()
        if not self.wait_for(end, timeout):
            return ""
        log = self.log()
        return log[log.find(start) + len(start):log.find(end)]

    def counters(self):
        """Return the device's counters, with each queue's prefixed by
        q<index>_."""
        counters, prefix = {}, ""
        for line in self.output("cat /proc/vduse_guest").splitlines():
            m = re.match(r"^\s+queue (\d+)\s*$", line)
            if m:
                prefix = f"q{m.group(1)}_"
                continue
            m = re.match(r"^\s+(\w+)\s+(-?\d+)\s*$", line)
            if m:
                counters[prefix + m.group(1)] = int(m.group(2))
        return counters

    def kill(self):
        if self.proc.poll() is None:
            self.proc.send_signal(signal.SIGKILL)
            self.proc.wait()


def cpumask(mask):
    """Format a CPU mask as sysfs takes it, in comma-separated groups of 32
    bits."""
    groups = []
    while True:
        groups.append(f"{mask & 0xffffffff:08x}")
        mask >>= 32
        if not mask:
            break
    return ",".join(reversed(groups)) + "\n"


def online_affinity():
    """VDUSE loops in the host kernel when it injects an interrupt on a
    queue whose affinity holds no online CPU, so such a queue gets every
    online CPU."""
    online = 0
    for r in read("/sys/devices/system/cpu/online").split(","):
        lo, _, hi = r.partition("-")
        for c in range(int(lo), int(hi or lo) + 1):
            online |= 1 << c
    for f in glob.glob(f"/sys/class/vduse/{NAME}/vq*/irq_cb_affinity"):
        if int(read(f).replace(",", ""), 16) & online == 0:
            write(f, cpumask(online))


def attach():
    """Add the device to the vDPA bus and return its host interface."""
    run("vdpa", "dev", "add", "name", NAME, "mgmtdev", "vduse", check=True)
    driver = f"/sys/bus/vdpa/devices/{NAME}/driver"
    if os.path.basename(os.path.realpath(driver)) != "virtio_vdpa":
        if os.path.exists(driver):
            write(f"{driver}/unbind", NAME)
        write("/sys/bus/vdpa/drivers/virtio_vdpa/bind", NAME)
    pattern = f"/sys/bus/vdpa/devices/{NAME}/virtio*/net/*"
    if not wait(lambda: glob.glob(pattern), 10):
        return None
    net = os.path.basename(glob.glob(pattern)[0])
    run("nmcli", "device", "set", net, "managed", "no")
    online_affinity()
    return net


def configure(net, mtu):
    run("ip", "link", "set", net, "mtu", str(mtu), check=True)
    run("ip", "addr", "replace", f"{HOST_ADDR}/24", "dev", net, check=True)
    run("ip", "link", "set", net, "up", check=True)


def carrier(net):
    return read(f"/sys/class/net/{net}/carrier") == "1"


def features(net):
    bits = read(f"/sys/class/net/{net}/device/features")
    return {name for name, bit in FEATURES.items()
            if bit < len(bits) and bits[bit] == "1"}


def link_local(ns, net):
    out = run("ip", "-n", ns, "-6", "-o", "addr", "show", "dev", net,
              "scope", "link").stdout
    m = re.search(r"inet6 (fe80::[0-9a-f:]+)/", out)
    return m.group(1) if m else None


def iperf(reverse=False, streams=1, seconds=10, ns=None):
    """Return the received rate in Gb/s of an iperf3 run against the peer,
    or None if it failed."""
    cmd = ["iperf3", "-c", PEER_ADDR, "-J", "-t", str(seconds),
           "-P", str(streams)]
    if reverse:
        cmd.append("-R")
    if ns:
        cmd = ["ip", "netns", "exec", ns] + cmd
    r = run(*cmd, timeout=seconds + 20)
    if r.returncode is None:
        print(f"iperf3 did not finish in {seconds + 20} seconds")
        return None
    try:
        end = json.loads(r.stdout)["end"]
        return end["sum_received"]["bits_per_second"] / 1e9
    except (ValueError, KeyError):
        print(r.stdout[-2000:] + r.stderr)
        return None


def ping_rtt(addr, count, ns=None):
    """Return the average round trip in microseconds, or None."""
    cmd = ["ping", "-q", "-c", str(count), "-W", "2", "-i", "0.2", addr]
    if ns:
        cmd = ["ip", "netns", "exec", ns] + cmd
    r = run(*cmd)
    m = re.search(r"= [\d.]+/([\d.]+)/", r.stdout)
    if r.returncode != 0 or not m:
        return None
    return float(m.group(1)) * 1000


def kernel_log():
    return run("dmesg").stdout.splitlines()


def log_time(line):
    m = re.match(r"\[\s*(\d+\.\d+)\]", line)
    return float(m.group(1)) if m else 0.0


def kernel_log_start():
    """Return where the host's kernel log ends now, as the time of its last
    line and its number of lines."""
    lines = kernel_log()
    return max((log_time(l) for l in lines), default=0.0), len(lines)


def kernel_log_since(start):
    """Return the host's kernel log after a point from kernel_log_start().
    The ring buffer can drop old lines, so the time is used when the log has
    timestamps, and the line count only when it has none."""
    time, count = start
    lines = kernel_log()
    if any(log_time(l) for l in lines):
        return [l for l in lines if log_time(l) > time]
    return lines[count:]


def warnings(lines, errors=()):
    return [l for l in lines
            if "WARNING:" in l or "BUG:" in l or "circular locking" in l or
            (("vduse-guest" in l or "vduse-uml" in l) and
             any(e in l for e in errors))]


def test_device(net, port0_mac):
    check(read(f"/sys/class/net/{net}/address") == port0_mac,
          f"{net} has port 0's address {port0_mac}")
    check(read(f"/sys/class/net/{net}/mtu") == str(JUMBO_MTU),
          f"{net} has the guest's MTU {JUMBO_MTU}")
    missing = set(FEATURES) - features(net)
    check(not missing, f"every feature is negotiated: missing "
          f"{sorted(missing)}")
    configure(net, JUMBO_MTU)
    if not check(wait(lambda: carrier(net), 30), f"{net} has carrier"):
        return False
    check(read(f"/sys/class/net/{net}/speed") == "10000" and
          read(f"/sys/class/net/{net}/duplex") == "full",
          f"{net} reports 10000 Mb/s full duplex: "
          f"{read(f'/sys/class/net/{net}/speed')} "
          f"{read(f'/sys/class/net/{net}/duplex')}")
    return True


def diagnose(guest, net, peer_net):
    """Print the guest's state after traffic stopped, then the counters of
    the host interface, the guest's eth0 and the peer port around a ping,
    which shows where frames are lost. The interrupt counts are read twice,
    so that a vector that no longer fires shows."""
    for line in ("cat /proc/vduse_guest", "cat /proc/interrupts",
                 "sleep 2; cat /proc/interrupts", "cat /proc/vfio_uml",
                 "dmesg | tail -40"):
        print(f"guest: {line}\n{guest.output(line, timeout=20)}")
    for when in ("before", "after"):
        if when == "after":
            r = run("ping", "-c", "3", "-W", "1", PEER_ADDR)
            print(f"ping: {r.stdout.strip()}")
        print(f"--- {when} the ping")
        print(run("ip", "-s", "-s", "link", "show", net).stdout)
        print(peer("ip", "-s", "-s", "link", "show", peer_net).stdout)
        print("guest eth0: " + guest.output(
            "grep . /sys/class/net/eth0/statistics/* | "
            "sed 's|.*/||' | grep -v ':0$'"))
        print("guest interrupts: " + guest.output(
            "grep -E 'i40e|vduse' /proc/interrupts"))
        print("guest backend: " + guest.output(
            "grep -E 'rx_|tx_|kicks|requests|injected|suppressed|idx|event|kick_pending' "
            "/proc/vduse_guest"))
        r = run("ethtool", "-S", net)
        print(f"host ethtool -S {net}:\n" +
              (r.stdout if r.returncode == 0 else "ethtool is not installed"))
    print(run("ip", "neigh", "show", "dev", net).stdout)
    print(peer("ip", "neigh", "show", "dev", peer_net).stdout)
    print("bounce_size: " + read(f"/sys/class/vduse/{NAME}/bounce_size"))
    print("host kernel log:\n" + "\n".join(kernel_log()[-40:]))


def test_traffic(guest, net, peer_net):
    before = guest.counters()
    check(ping_rtt(PEER_ADDR, 5) is not None, "the host pings the peer")
    check(ping_rtt(HOST_ADDR, 5, ns=PEER_NS) is not None,
          "the peer pings the host")
    ll = link_local(PEER_NS, peer_net)
    check(ll is not None and
          ping_rtt(f"{ll}%{net}", 5) is not None,
          "the host pings the peer's IPv6 link-local address")

    for mtu in (JUMBO_MTU, 1500):
        configure(net, mtu)
        peer("ip", "link", "set", peer_net, "mtu", str(mtu), check=True)
        for reverse in (False, True):
            for streams in (1, 4):
                rate = iperf(reverse, streams)
                if not check(rate is not None and rate > 0.5,
                             f"iperf3 at MTU {mtu}, "
                             f"{'peer to host' if reverse else 'host to peer'}, "
                             f"{streams} streams: "
                             f"{rate and round(rate, 2)} Gb/s"):
                    diagnose(guest, net, peer_net)
                    return False
    configure(net, JUMBO_MTU)
    peer("ip", "link", "set", peer_net, "mtu", str(JUMBO_MTU), check=True)

    after = guest.counters()
    for name in ("tx_gso", "rx_gso"):
        check(after.get(name, 0) > before.get(name, 0),
              f"the guest's {name} grows: {after.get(name)}")
    return True


def test_link(net, peer_net):
    peer("ip", "link", "set", peer_net, "down", check=True)
    check(wait(lambda: not carrier(net), 15),
          f"{net} loses carrier when the peer goes down")
    peer("ip", "link", "set", peer_net, "up", check=True)
    check(wait(lambda: carrier(net), 30),
          f"{net} gets carrier back when the peer comes up")


def test_reattach(net):
    run("vdpa", "dev", "del", NAME, check=True)
    net = attach()
    if not check(net is not None, "the device attaches again"):
        return None
    configure(net, JUMBO_MTU)
    check(wait(lambda: carrier(net), 30) and
          ping_rtt(PEER_ADDR, 3) is not None,
          "traffic passes after attaching again")
    return net


def test_kill(guest, build, initrd, out, log):
    """Kill the guest during an iperf3 run and relaunch it. The host's
    interface stays, and TCP carries on once the new guest's link is up."""
    client = subprocess.Popen(
        ["iperf3", "-c", PEER_ADDR, "-J", "-t", "40"],
        stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    time.sleep(5)
    guest.kill()
    guest = Guest(build, initrd, out, log)
    check(guest.wait_for("reconnecting to the net device", 60),
          "the relaunched guest reconnects")
    try:
        stdout, _ = client.communicate(timeout=120)
        result = json.loads(stdout)
        # The last second shows that traffic came back after the kill.
        ok = ("error" not in result and
              result["intervals"][-1]["sum"]["bytes"] > 0)
    except (subprocess.TimeoutExpired, ValueError, KeyError, IndexError):
        client.kill()
        ok = False
    check(ok, "an iperf3 run across the kill finishes with traffic "
          "in its last second")
    check(ping_rtt(PEER_ADDR, 3) is not None,
          "traffic passes through the relaunched guest")
    return guest


CLK_TCK = os.sysconf("SC_CLK_TCK")


def stat_cpu(line):
    """Return the busy and total time in a cpu line of /proc/stat, or None
    if the line is not one."""
    v = [int(x) for x in line.split()[1:8] if x.isdigit()]
    if len(v) != 7:
        return None
    return v[0] + v[1] + v[2] + v[5] + v[6], sum(v)


def process_cpu(pid):
    """Return the CPU seconds of a host process and all of its threads."""
    with open(f"/proc/{pid}/stat") as f:
        fields = f.read().rsplit(")", 1)[1].split()
    return (int(fields[11]) + int(fields[12])) / CLK_TCK


class CpuMeter:
    """CPU use over a job: the host's busy CPUs, the share of them that the
    guest's UML process takes, and how busy the guest's own CPU is."""

    def __init__(self, guest):
        self.guest = guest
        self.wall = time.time()
        with open("/proc/stat") as f:
            self.host = stat_cpu(f.readline())
        if guest:
            self.uml = process_cpu(guest.proc.pid)
            self.inner = stat_cpu(guest.output("head -1 /proc/stat"))

    def stop(self):
        wall = time.time() - self.wall
        with open("/proc/stat") as f:
            busy, total = stat_cpu(f.readline())
        r = {"host_cpus": round((busy - self.host[0]) /
                                max(total - self.host[1], 1) *
                                os.sysconf("SC_NPROCESSORS_ONLN"), 2)}
        if self.guest:
            r["uml_cpus"] = round((process_cpu(self.guest.proc.pid) -
                                   self.uml) / wall, 2)
            inner = stat_cpu(self.guest.output("head -1 /proc/stat"))
            if inner and self.inner:
                r["guest_busy"] = round((inner[0] - self.inner[0]) /
                                        max(inner[1] - self.inner[1], 1), 2)
        return r


def in_ns(ns, cmd):
    return ["ip", "netns", "exec", ns] + cmd if ns else cmd


def iperf_udp(reverse, seconds=10, ns=None):
    """Return the packets per second that the sender sent and the receiver
    got in an iperf3 run of 64-byte UDP datagrams at an unlimited rate."""
    cmd = ["iperf3", "-c", PEER_ADDR, "-J", "-u", "-b", "0", "-l", "64",
           "-t", str(seconds)]
    if reverse:
        cmd.append("-R")
    r = run(*in_ns(ns, cmd), timeout=seconds + 20)
    try:
        end = json.loads(r.stdout)["end"]
        total = end.get("sum") or end["sum_received"]
        sent = total["packets"]
        got = sent - total["lost_packets"]
        return {"sent_pps": round(sent / total["seconds"]),
                "received_pps": round(got / total["seconds"]),
                "lost_percent": round(total["lost_percent"], 2)}
    except (ValueError, KeyError, TypeError, ZeroDivisionError):
        print(r.stdout[-2000:] + r.stderr)
        return None


def netperf_rr(test, seconds=10, ns=None):
    """Return the transactions per second and the mean and 99th percentile
    round trip in microseconds of a netperf test with 1-byte messages."""
    cmd = ["netperf", "-H", PEER_ADDR, "-t", test, "-l", str(seconds),
           "-P", "0", "--", "-r", "1,1",
           "-o", "THROUGHPUT,MEAN_LATENCY,P99_LATENCY"]
    r = run(*in_ns(ns, cmd), timeout=seconds + 20)
    try:
        tps, mean, p99 = r.stdout.strip().splitlines()[-1].split(",")
        return {"tps": round(float(tps)), "mean_us": round(float(mean), 1),
                "p99_us": round(float(p99), 1)}
    except (ValueError, IndexError):
        print(r.stdout[-2000:] + r.stderr)
        return None


def bench_jobs(guest=None, ns=None):
    """Run every job, through the guest when it is given and otherwise
    natively in the namespace, with the CPU use of each."""
    def measured(fn):
        meter = CpuMeter(guest)
        result = fn()
        return {"result": result, **meter.stop()}

    results = {}
    for reverse in (False, True):
        way = "rx" if reverse else "tx"
        for streams in (1, 4):
            results[f"tcp_{way}_{streams}_gbps"] = measured(
                lambda: iperf(reverse, streams, seconds=20, ns=ns))
        results[f"udp64_{way}"] = measured(lambda: iperf_udp(reverse, ns=ns))
    for test in ("TCP_RR", "UDP_RR"):
        results[test.lower()] = measured(lambda: netperf_rr(test, ns=ns))
    results["ping_idle_us"] = ping_rtt(PEER_ADDR, 50, ns=ns)
    load = subprocess.Popen(in_ns(ns, ["iperf3", "-c", PEER_ADDR, "-t", "15"]),
                            stdout=subprocess.DEVNULL,
                            stderr=subprocess.DEVNULL)
    time.sleep(2)
    results["ping_load_us"] = ping_rtt(PEER_ADDR, 50, ns=ns)
    load.wait()
    return results


def write_bench(out, results):
    with open(os.path.join(out, "bench.json"), "w") as f:
        json.dump(results, f, indent=2)


def bench(guest, out):
    results = {"guest": bench_jobs(guest=guest)}
    results["guest"]["counters"] = guest.counters()
    write_bench(out, results)
    return results


def print_bench(results):
    """Print each job's result and CPU use through the guest and natively."""
    def describe(v):
        if not isinstance(v, dict) or "result" not in v:
            return f"{v and round(v, 1)}"
        r = v["result"]
        text = (", ".join(f"{k}={x}" for k, x in r.items())
                if isinstance(r, dict) else f"{r and round(r, 2)}")
        cpu = ", ".join(f"{k}={x}" for k, x in v.items() if k != "result")
        return f"{text} ({cpu})"

    for job, v in results.get("guest", {}).items():
        if job == "counters":
            continue
        print(job)
        print(f"  guest:  {describe(v)}")
        print(f"  native: {describe(results.get('native', {}).get(job))}")


def bench_native(out, results):
    """Both ports on the host's i40e, with port 0 in a namespace of its
    own. Run after the guest is gone and port 0 is back on i40e."""
    port0 = port_netdev(GUEST_PORT)
    if not port0:
        print("bench: port 0 has no interface on the host")
        return
    run("ip", "netns", "add", NATIVE_NS)
    try:
        run("nmcli", "device", "set", port0, "managed", "no")
        run("ip", "link", "set", port0, "netns", NATIVE_NS, check=True)
        run("ip", "-n", NATIVE_NS, "link", "set", port0, "mtu",
            str(JUMBO_MTU), check=True)
        run("ip", "-n", NATIVE_NS, "addr", "add", f"{HOST_ADDR}/24", "dev",
            port0, check=True)
        run("ip", "-n", NATIVE_NS, "link", "set", port0, "up", check=True)
        wait(lambda: run("ip", "netns", "exec", NATIVE_NS, "cat",
                         f"/sys/class/net/{port0}/carrier").stdout.strip()
             == "1", 30)
        results["native"] = bench_jobs(ns=NATIVE_NS)
    finally:
        run("ip", "-n", NATIVE_NS, "link", "set", port0, "netns", "1")
        run("ip", "netns", "del", NATIVE_NS)
    write_bench(out, results)
    print_bench(results)


def link_down_on_close(net, ns=None):
    """Return whether i40e takes the link down when the interface closes,
    which it does not by default, or None without ethtool."""
    cmd = ["ethtool", "--show-priv-flags", net]
    r = run(*(["ip", "netns", "exec", ns] + cmd if ns else cmd))
    m = re.search(r"^link-down-on-close\s*:\s*(on|off)", r.stdout, re.M)
    return m.group(1) == "on" if m else None


def set_link_down_on_close(net, on, ns=None):
    cmd = ["ethtool", "--set-priv-flags", net, "link-down-on-close",
           "on" if on else "off"]
    run(*(["ip", "netns", "exec", ns] + cmd if ns else cmd), check=True)


def setup_peer():
    """Set up port 1 as the far end. Taking it down must take the link down,
    or the guest's port never loses its link."""
    peer_net = port_netdev(PEER_PORT)
    if not peer_net:
        sys.exit(f"{PEER_PORT} has no interface on the host")
    saved = link_down_on_close(peer_net)
    if saved is None:
        sys.exit("ethtool is needed to set i40e's link-down-on-close flag")
    set_link_down_on_close(peer_net, True)
    run("ip", "netns", "add", PEER_NS)
    run("nmcli", "device", "set", peer_net, "managed", "no")
    run("ip", "link", "set", peer_net, "netns", PEER_NS, check=True)
    peer("ip", "link", "set", peer_net, "mtu", str(JUMBO_MTU), check=True)
    peer("ip", "addr", "add", f"{PEER_ADDR}/24", "dev", peer_net, check=True)
    peer("ip", "link", "set", peer_net, "up", check=True)
    servers = [subprocess.Popen(["ip", "netns", "exec", PEER_NS] + cmd,
                                stdout=subprocess.DEVNULL,
                                stderr=subprocess.DEVNULL)
               for cmd in (["iperf3", "-s"], ["netserver", "-D"])
               if shutil.which(cmd[0])]
    return peer_net, servers, saved


def teardown_peer(peer_net, servers, saved):
    for server in servers:
        server.kill()
        server.wait()
    peer("ip", "link", "set", peer_net, "netns", "1")
    run("ip", "netns", "del", PEER_NS)
    set_link_down_on_close(peer_net, saved)


class Run:
    """The guest that is running and the benchmark's results."""
    guest = None
    results = None


def check_guest(guest, which):
    """Check a guest's drop counters and log, before it is replaced."""
    counters = guest.counters()
    check(all(counters.get(c) == 0 for c in GUEST_COUNTERS),
          f"{which}'s drop counters are zero: " +
          ", ".join(f"{c}={counters.get(c)}" for c in GUEST_COUNTERS))
    check(not warnings(guest.log().splitlines(), GUEST_ERRORS),
          f"{which} logs no warning or error")


def run_tests(state, build, initrd, out, log, port0_mac, peer_net,
              do_bench):
    log_start = kernel_log_start()
    state.guest = Guest(build, initrd, out, log)
    if not check(state.guest.wait_for("created net device", 120),
                 "the guest creates the device"):
        return
    net = attach()
    if not check(net is not None, "the host attaches the device"):
        return
    if not test_device(net, port0_mac):
        return
    if not test_traffic(state.guest, net, peer_net):
        return
    test_link(net, peer_net)
    if do_bench:
        state.results = bench(state.guest, out)
    net = test_reattach(net)
    check_guest(state.guest, "the first guest")
    if net:
        state.guest = test_kill(state.guest, build, initrd, out, log)
        check_guest(state.guest, "the relaunched guest")
    check(not warnings(kernel_log_since(log_start)),
          "the host logs no warning")


def main():
    args = sys.argv[1:]
    do_bench = "--bench" in args
    args = [a for a in args if a != "--bench"]
    if len(args) != 3:
        sys.exit(f"usage: {sys.argv[0]} [--bench] <guest build dir> "
                 "<initramfs> <output dir>")
    build, initrd, out = (os.path.abspath(a) for a in args)
    os.makedirs(out, exist_ok=True)
    guest_log = os.path.join(out, "guest.log")
    if os.path.exists(guest_log):
        os.replace(guest_log, guest_log + ".old")
    log = os.path.join(out, NAME + ".inflight")
    if os.path.exists(log):
        os.unlink(log)

    for mod in ("vduse", "virtio_vdpa", "virtio_net", "vfio-pci", "i40e"):
        run("modprobe", mod, check=True)
    port0 = port_netdev(GUEST_PORT)
    if driver_of(GUEST_PORT) != "i40e" or not port0:
        sys.exit(f"{GUEST_PORT} must be on i40e when the test starts")
    port0_mac = read(f"/sys/class/net/{port0}/address")

    if do_bench and not shutil.which("netperf"):
        sys.exit("--bench needs netperf and netserver")
    peer_net, servers, saved_flag = setup_peer()
    run(os.path.join(SCRIPTS, "vfio-bind.sh"), GUEST_PORT, check=True)
    state = Run()
    try:
        run_tests(state, build, initrd, out, log, port0_mac, peer_net,
                  do_bench)
    finally:
        if state.guest:
            state.guest.kill()
        run("vdpa", "dev", "del", NAME)
        run(sys.executable, "-B", os.path.join(SCRIPTS, "vduse-destroy.py"),
            NAME)
        run(os.path.join(SCRIPTS, "vfio-unbind.sh"), GUEST_PORT)
        wait(lambda: port_netdev(GUEST_PORT) is not None, 10)
        if state.results:
            bench_native(out, state.results)
        teardown_peer(peer_net, servers, saved_flag)

    print(f"{len(failures)} failures" if failures else "PASS")
    sys.exit(1 if failures else 0)


if __name__ == "__main__":
    main()
