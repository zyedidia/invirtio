#!/usr/bin/env python3
# Measure a WiFi interface's throughput and latency against an iperf3 server
# that is reached through the access point, as docs/VIRTIO_WIFI.md stage 6
# describes. Run as root.
#
# usage: wlan-bench.py [options] <interface> <wpa_supplicant.conf> <server>
#                      <output dir>
#
# The interface is the host's mt7921e interface for the native mode, or the
# virtio_wlan interface for the sandboxed one, so that both modes are measured
# the same way. The script takes the interface from NetworkManager and the
# system wpa_supplicant, and moves its wiphy into a network namespace of its
# own, so that traffic to a server on this host crosses the radio instead of
# being delivered locally. There it joins the network of the configuration
# file with wpa_supplicant, which keeps the passphrase off the command line,
# and gets a lease with busybox udhcpc. The server address is one of this
# host's addresses on the access point's uplink, or any other host behind the
# access point. The script runs an iperf3 server on it unless --no-server is
# given.
#
# It then runs iperf3 TCP to the server (upload) and from it (download), each
# --reps times for --time seconds, and pings the server while idle and during
# one more download. It reports the host's busy CPU time over each iperf3 run,
# in cores, and with --pid the CPU time of that process too, the UML guest in
# the sandboxed mode, which the host's figure includes. It writes results.json
# and the logs to the output directory and prints a summary. On every path it
# moves the wiphy back, stops its wpa_supplicant and server, and gives the
# interface back to NetworkManager.

import argparse
import json
import os
import re
import statistics
import subprocess
import sys
import time

NETNS = "wlan-bench"
PORT = 5299
ENV = {**os.environ, "LC_ALL": "C"}
# The lease adds the address and a default route inside the namespace only.
UDHCPC_SCRIPT = """#!/bin/sh
case "$1" in
bound|renew)
	ip addr flush dev "$interface"
	ip addr add "$ip/$mask" dev "$interface"
	[ -n "$router" ] && ip route replace default via "${router%% *}"
	;;
esac
exit 0
"""
WPAS = ["busctl", "--auto-start=no", "call", "fi.w1.wpa_supplicant1",
        "/fi/w1/wpa_supplicant1", "fi.w1.wpa_supplicant1"]


def run(*args, check=False, timeout=None):
    return subprocess.run(args, capture_output=True, text=True, check=check,
                          env=ENV, timeout=timeout)


def ns(*args, **kwargs):
    return run("ip", "netns", "exec", NETNS, *args, **kwargs)


def phy_of(net):
    with open(f"/sys/class/net/{net}/phy80211/name") as f:
        return f.read().strip()


def release(net):
    """Take the interface from NetworkManager and the system wpa_supplicant,
    which would otherwise end connections that it did not make."""
    for _ in range(50):
        run("nmcli", "device", "set", net, "managed", "no")
        r = run("nmcli", "-g", "GENERAL.STATE", "device", "show", net)
        if "unmanaged" in r.stdout or "not running" in r.stderr:
            break
        time.sleep(0.1)
    m = re.match(r'o "(\S+)"', run(*WPAS, "GetInterface", "s", net).stdout)
    if m:
        run(*WPAS, "RemoveInterface", "o", m.group(1))


def cpu_time(pid):
    """Return a process's user and system time in seconds."""
    with open(f"/proc/{pid}/stat") as f:
        fields = f.read().rsplit(")", 1)[1].split()
    return (int(fields[11]) + int(fields[12])) / os.sysconf("SC_CLK_TCK")


def host_busy():
    """Return the host's total busy CPU time in seconds."""
    with open("/proc/stat") as f:
        v = [int(x) for x in f.readline().split()[1:]]
    return (sum(v) - v[3] - v[4]) / os.sysconf("SC_CLK_TCK")


def iperf(server, seconds, reverse, pid):
    args = ["timeout", str(seconds + 30), "iperf3", "-c", server, "-p",
            str(PORT), "-t", str(seconds), "-J"] + (["-R"] if reverse else [])
    # The server stops listening for a moment between tests.
    for _ in range(5):
        before = (cpu_time(pid) if pid else None, host_busy(), time.time())
        r = ns(*args)
        after = (cpu_time(pid) if pid else None, host_busy(), time.time())
        if "Connection refused" not in r.stdout:
            break
        time.sleep(1)
    try:
        end = json.loads(r.stdout)["end"]
        mbps = end["sum_received"]["bits_per_second"] / 1e6
        retrans = end["sum_sent"].get("retransmits")
    except (ValueError, KeyError):
        return {"error": (r.stdout + r.stderr)[-500:]}
    wall = after[2] - before[2]
    result = {"mbps": mbps, "retransmits": retrans,
              "host_cpu": (after[1] - before[1]) / wall}
    if pid:
        result["guest_cpu"] = (after[0] - before[0]) / wall
    return result


def ping(server, count):
    r = ns("ping", "-c", str(count), "-i", "0.2", "-W", "2", server)
    m = re.search(r"= ([\d.]+)/([\d.]+)/([\d.]+)/([\d.]+) ms", r.stdout)
    loss = re.search(r"([\d.]+)% packet loss", r.stdout)
    return {"min": float(m.group(1)), "avg": float(m.group(2)),
            "max": float(m.group(3)), "mdev": float(m.group(4)),
            "loss": float(loss.group(1)) if loss else None} if m else \
        {"error": (r.stdout + r.stderr)[-500:]}


def summary(label, results):
    print(f"\n{label}: {results['link'].get('freq', '?')} MHz, "
          f"{results['link'].get('tx bitrate', '?')}")
    for what in ("upload", "download"):
        runs = [x for x in results[what] if "mbps" in x]
        if not runs:
            print(f"  {what:8} failed")
            continue
        mbps = [x["mbps"] for x in runs]
        line = (f"  {what:8} median {statistics.median(mbps):7.1f} Mb/s "
                f"(min {min(mbps):.1f}, max {max(mbps):.1f}), host CPU "
                f"{statistics.median(x['host_cpu'] for x in runs):.2f} cores")
        if "guest_cpu" in runs[0]:
            line += (", guest "
                     f"{statistics.median(x['guest_cpu'] for x in runs):.2f}")
        print(line)
    for what in ("ping_idle", "ping_loaded"):
        p = results[what]
        if "avg" in p:
            print(f"  {what:11} avg {p['avg']:.2f} ms, max {p['max']:.2f} ms,"
                  f" mdev {p['mdev']:.2f} ms, loss {p['loss']}%")
        else:
            print(f"  {what:11} failed")


def main():
    ap = argparse.ArgumentParser(usage="%(prog)s [options] <interface> "
                                 "<wpa_supplicant.conf> <server> <output dir>")
    ap.add_argument("net")
    ap.add_argument("conf")
    ap.add_argument("server")
    ap.add_argument("out")
    ap.add_argument("--label", default="")
    ap.add_argument("--reps", type=int, default=3)
    ap.add_argument("--time", type=int, default=10)
    ap.add_argument("--pid", type=int)
    ap.add_argument("--no-server", action="store_true")
    a = ap.parse_args()
    os.makedirs(a.out, exist_ok=True)
    out = os.path.abspath(a.out)
    phy = phy_of(a.net)

    server = wpa = None
    run("ip", "netns", "del", NETNS)
    try:
        release(a.net)
        run("ip", "netns", "add", NETNS, check=True)
        run("iw", "phy", phy, "set", "netns", "name", NETNS, check=True)
        ns("ip", "link", "set", "lo", "up")
        ns("ip", "link", "set", a.net, "up", check=True)
        if not a.no_server:
            server = subprocess.Popen(["iperf3", "-s", "-B", a.server, "-p",
                                       str(PORT)], stdout=subprocess.DEVNULL,
                                      stderr=subprocess.DEVNULL, env=ENV)

        ctrl = os.path.join(out, "wpa")
        conf = os.path.join(out, "wpa_supplicant.conf")
        with open(a.conf) as f, open(conf, "w") as g:
            g.write(f"ctrl_interface={ctrl}\n" + re.sub(
                r"(?m)^ctrl_interface=.*\n", "", f.read()))
        os.chmod(conf, 0o600)
        with open(os.path.join(out, "wpa_supplicant.log"), "w") as logf:
            wpa = subprocess.Popen(["ip", "netns", "exec", NETNS,
                                    "wpa_supplicant", "-i", a.net, "-D",
                                    "nl80211", "-c", conf], stdout=logf,
                                   stderr=subprocess.STDOUT, env=ENV)
        end = time.time() + 30
        while time.time() < end:
            st = ns("wpa_cli", "-p", ctrl, "-i", a.net, "status").stdout
            if "wpa_state=COMPLETED" in st:
                break
            time.sleep(0.2)
        else:
            sys.exit("wpa_supplicant did not join the network, see "
                     f"{out}/wpa_supplicant.log")

        script = os.path.join(out, "udhcpc.sh")
        with open(script, "w") as f:
            f.write(UDHCPC_SCRIPT)
        os.chmod(script, 0o755)
        r = ns("busybox", "udhcpc", "-i", a.net, "-f", "-n", "-q", "-t", "5",
               "-T", "2", "-s", script)
        if r.returncode:
            sys.exit(f"no DHCP lease: {r.stdout}{r.stderr}")

        link = {}
        for line in ns("iw", "dev", a.net, "link").stdout.splitlines():
            k, _, v = line.strip().partition(": ")
            if v:
                link[k] = v
        results = {"label": a.label, "interface": a.net, "phy": phy,
                   "server": a.server, "link": link,
                   "ssid": link.get("SSID"),
                   "ping_idle": ping(a.server, 50), "upload": [],
                   "download": []}
        for _ in range(a.reps):
            results["upload"].append(iperf(a.server, a.time, False, a.pid))
            results["download"].append(iperf(a.server, a.time, True,
                                             a.pid))
        # Latency under load: ping during one more download.
        load = subprocess.Popen(["ip", "netns", "exec", NETNS, "iperf3",
                                 "-c", a.server, "-p", str(PORT), "-t",
                                 str(a.time), "-R"],
                                stdout=subprocess.DEVNULL,
                                stderr=subprocess.DEVNULL, env=ENV)
        time.sleep(1)
        results["ping_loaded"] = ping(a.server,
                                      max(1, (a.time - 2) * 5))
        load.wait(timeout=a.time + 30)

        with open(os.path.join(out, "results.json"), "w") as f:
            json.dump(results, f, indent=2)
        summary(a.label or a.net, results)
    finally:
        if wpa:
            wpa.terminate()
            try:
                wpa.wait(timeout=10)
            except subprocess.TimeoutExpired:
                wpa.kill()
                wpa.wait()
        if server:
            server.terminate()
            server.wait()
        ns("iw", "phy", phy, "set", "netns", "1")
        run("ip", "netns", "del", NETNS)
        run("nmcli", "device", "set", a.net, "managed", "yes")


if __name__ == "__main__":
    main()
