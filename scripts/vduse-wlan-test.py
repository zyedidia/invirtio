#!/usr/bin/env python3
# Test the host's virtio_wlan driver against the UML mac80211_hwsim guest of
# docs/VIRTIO_WIFI.md (stages 1 and 2, and an open network). Run as root
# on the linux-host kernel.
#
# usage: vduse-wlan-test.py <guest build dir> <initramfs> <output dir>
#
# The script loads virtio_wlan, sets the host's country to DE for the 6 GHz
# band, boots the guest, and attaches its device to virtio_vdpa. It checks that the host's wiphy has
# the guest's bands, channels, capabilities and ciphers, and that bringing
# the host's interface up and down does the same to the guest's wlan0. It
# then scans and checks that every network of the guest is found on its band,
# connects to the open networks on 2.4 and 5 GHz, checks the station and
# disconnects, checks that a missing network fails, and checks that the
# driver's counters are zero. It removes the device while connected and
# attaches it again, kills the guest, and checks that a command times out and
# that the device can still be removed. It also checks that neither kernel
# logs a warning, and restores the host's country. The guest's console goes
# to guest.log in the output directory.

import glob
import os
import re
import signal
import subprocess
import sys
import time

NAME = "vwlan-test"
COUNTRY = "DE"
NETWORKS = {f"hwsim-{band}-{kind}" for band in ("2g", "5g")
            for kind in ("open", "wpa2", "wpa2-mfp", "wpa3-transition")}
NETWORKS.add("hwsim-6g-wpa3")
COUNTERS = ["cmd_timeouts", "bad_responses", "bad_events", "stale_events",
            "bad_bss", "bad_connect_results", "bad_rates", "scan_timeouts",
            "connect_timeouts"]

failures = []


def check(cond, what):
    print(("ok: " if cond else "FAIL: ") + what)
    if not cond:
        failures.append(what)
    return cond


def run(*args, check=False):
    return subprocess.run(args, capture_output=True, text=True, check=check)


class Guest:
    def __init__(self, build, initrd, out):
        self.path = os.path.join(out, "guest.log")
        self.logf = open(self.path, "w")
        self.marks = 0
        cmd = [os.path.join(build, "linux"), f"umid=vwlan-{os.getpid()}",
               "mem=768M", f"initrd={initrd}", "mac80211_hwsim.radios=4",
               "cfg80211.ieee80211_regdom=DE",
               f"vduse_guest.device=wlan,{NAME},netdev=wlan0,"
               f"log={os.path.join(out, NAME + '.inflight')}", "init_shell"]
        self.proc = subprocess.Popen(cmd, stdin=subprocess.PIPE,
                                     stdout=self.logf,
                                     stderr=subprocess.STDOUT)

    def log(self):
        with open(self.path, errors="replace") as f:
            return f.read()

    def wait_for(self, text, timeout):
        end = time.time() + timeout
        while time.time() < end:
            if text in self.log():
                return True
            if self.proc.poll() is not None:
                return text in self.log()
            time.sleep(0.2)
        return False

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

    def is_up(self):
        out = self.output("cat /sys/class/net/wlan0/flags")
        m = re.search(r"0x[0-9a-f]+", out)
        return bool(m and int(m.group(0), 16) & 1)

    def kill(self):
        if self.proc.poll() is None:
            self.proc.send_signal(signal.SIGKILL)
            self.proc.wait()


def parse_phy(text):
    """Return the frequencies and capabilities of each band in iw phy info."""
    bands = {}
    band = None
    for line in text.splitlines():
        m = re.match(r"\s*Band (\d+):", line)
        if m:
            band = bands.setdefault(int(m.group(1)), {
                "freqs": set(), "ht": None, "vht": None, "he": False})
            continue
        if re.match(r"\t[^\t]", line):
            band = None
        if band is None:
            continue
        m = re.search(r"\* (\d+)(?:\.\d+)? MHz \[\d+\]", line)
        if m:
            band["freqs"].add(int(m.group(1)))
        m = re.match(r"\s*Capabilities: (0x[0-9a-f]+)", line)
        if m and band["ht"] is None:
            band["ht"] = m.group(1)
        m = re.search(r"VHT Capabilities \((0x[0-9a-f]+)\)", line)
        if m:
            band["vht"] = m.group(1)
        if re.search(r"(?<!E)HE Iftypes:.*\bmanaged\b", line):
            band["he"] = True
    ciphers = set()
    m = re.search(r"Supported Ciphers:\n((?:\s+\* .*\n)+)", text)
    if m:
        ciphers = {l.strip("\t *") for l in m.group(1).splitlines()}
    return bands, ciphers


def write(path, text):
    with open(path, "w") as f:
        f.write(text)


def attach():
    """Add the device to the vDPA bus and return its wiphy and netdev."""
    run("vdpa", "dev", "add", "name", NAME, "mgmtdev", "vduse", check=True)
    sysdev = f"/sys/bus/vdpa/devices/{NAME}"
    driver = os.path.join(sysdev, "driver")
    if os.path.basename(os.path.realpath(driver)) != "virtio_vdpa":
        if os.path.exists(driver):
            write(f"{driver}/unbind", NAME)
        write("/sys/bus/vdpa/drivers/virtio_vdpa/bind", NAME)
    for _ in range(100):
        phys = glob.glob(f"/sys/bus/vdpa/devices/{NAME}/virtio*/ieee80211/*")
        nets = glob.glob(f"/sys/bus/vdpa/devices/{NAME}/virtio*/net/*")
        if phys and nets:
            phy, net = os.path.basename(phys[0]), os.path.basename(nets[0])
            run("nmcli", "device", "set", net, "managed", "no")
            # A new wiphy starts blocked while the host's WiFi is switched off.
            for soft in glob.glob(f"/sys/class/ieee80211/{phy}/rfkill*/soft"):
                write(soft, "0")
            run("ip", "link", "set", net, "down")
            return phy, net
        time.sleep(0.1)
    return None, None


def counter(phy, name):
    try:
        with open(f"/sys/kernel/debug/ieee80211/{phy}/{name}") as f:
            return int(f.read())
    except OSError:
        return None


def counters(phy):
    return {name: counter(phy, name) for name in COUNTERS}


def band_of(freq):
    return "2g" if freq < 3000 else "5g" if freq < 5935 else "6g"


def scan(net):
    """Return the SSID and frequency of each BSS that a scan finds. A scan
    that NetworkManager started before the device became unmanaged makes
    the first attempts fail with EBUSY."""
    for _ in range(20):
        r = run("timeout", "40", "iw", "dev", net, "scan")
        if "(-16)" not in r.stderr:
            break
        time.sleep(1)
    if r.returncode != 0:
        print(f"iw scan failed with {r.returncode}: {r.stderr.strip()}")
    found = {}
    bss = None
    for line in r.stdout.splitlines():
        m = re.match(r"BSS ([0-9a-f:]{17})", line)
        if m:
            bss = found.setdefault(m.group(1), {})
        m = re.match(r"\s+freq: (\d+)", line)
        if m and bss is not None:
            bss["freq"] = int(m.group(1))
        m = re.match(r"\s+SSID: (.*)", line)
        if m and bss is not None:
            bss["ssid"] = m.group(1)
    return found


def global_regdom():
    out = run("iw", "reg", "get").stdout
    return out[:out.find("\n\n")] if out.startswith("global") else ""


def get_country():
    """Return the global country, if it is one that can be set again."""
    m = re.match(r"global\ncountry ([A-Z]{2}|00):", global_regdom())
    return m.group(1) if m else None


def allow_6ghz():
    """Set the country, and wait until the global domain permits 6 GHz. An
    intersection with another hint shows as country 98."""
    run("iw", "reg", "set", COUNTRY)
    for _ in range(50):
        if "(5945 - " in global_regdom():
            return True
        time.sleep(0.1)
    return False


class Events:
    """The output of iw event for the duration of a test."""

    def __init__(self):
        self.proc = subprocess.Popen(["iw", "event"], stdout=subprocess.PIPE)
        self.fd = self.proc.stdout.fileno()
        os.set_blocking(self.fd, False)
        self.text = ""

    def wait_for(self, pattern, timeout):
        end = time.time() + timeout
        while time.time() < end:
            try:
                self.text += os.read(self.fd, 65536).decode(errors="replace")
            except BlockingIOError:
                pass
            m = re.search(pattern, self.text)
            if m:
                self.text = self.text[m.end():]
                return m
            time.sleep(0.1)
        return None

    def close(self):
        self.proc.kill()
        self.proc.wait()


def test_connect(events, net, ssid):
    run("iw", "dev", net, "connect", ssid, check=True)
    m = events.wait_for(rf"{net} \(phy #\d+\): (connected to|failed to "
                        rf"connect|timed out)[^\n]*", 40)
    return m.group(0) if m else "no connection event"


def test_scan_connect(phy, net):
    """Scan all three bands, then connect to open networks and leave the
    last connection up. Return the number of disconnections that the host
    asked for."""
    r = run("ip", "link", "set", net, "up")
    if not check(r.returncode == 0, f"{net} comes up"):
        return 0
    found = scan(net)
    ssids = {b.get("ssid") for b in found.values()}
    check(NETWORKS <= ssids,
          f"a scan finds the guest's networks: missing "
          f"{sorted(NETWORKS - ssids)}")
    check(all(b.get("ssid", "hwsim-").startswith("hwsim-") and
              band_of(b.get("freq", 0)) == b["ssid"].split("-")[1]
              for b in found.values() if b.get("ssid") in NETWORKS),
          "each network is on its band")

    events = Events()
    disconnects = 0
    try:
        for ssid in ("hwsim-2g-open", "hwsim-5g-open"):
            result = test_connect(events, net, ssid)
            link = run("iw", "dev", net, "link").stdout
            check(result.startswith(f"{net}") and "connected to" in result
                  and f"SSID: {ssid}" in link,
                  f"connecting to {ssid}: {result}")
            sta = run("iw", "dev", net, "station", "dump")
            if not check("signal:" in sta.stdout,
                         "station dump shows the access point's signal"):
                print(sta.stdout + sta.stderr + link)
            run("iw", "dev", net, "disconnect", check=True)
            disconnects += 1
            m = events.wait_for(rf"{net} \(phy #\d+\): disconnected", 10)
            check(m is not None and
                  "Not connected" in run("iw", "dev", net, "link").stdout,
                  f"disconnecting from {ssid}")
        result = test_connect(events, net, "hwsim-missing")
        check("failed to connect" in result or "timed out" in result,
              f"connecting to a missing network fails: {result}")
        result = test_connect(events, net, "hwsim-2g-open")
        check("connected to" in result,
              f"connecting to hwsim-2g-open again: {result}")
    finally:
        events.close()
    return disconnects


def kernel_log():
    return run("dmesg").stdout.splitlines()


def warnings(lines):
    return [l for l in lines
            if "WARNING:" in l or "BUG:" in l or "circular locking" in l]


def test_wiphy(guest, phy):
    host_bands, host_ciphers = parse_phy(run("iw", "phy", phy, "info").stdout)
    guest_bands, guest_ciphers = parse_phy(guest.output(
        "iw phy $(cat /sys/class/net/wlan0/phy80211/name) info"))
    check(sorted(host_bands) == [1, 2, 4],
          f"the host's wiphy has the 2.4, 5 and 6 GHz bands: "
          f"{sorted(host_bands)}")
    for n in (1, 2, 4):
        band = guest_bands.get(n, {})
        host = host_bands.get(n, {})
        check(band and host.get("freqs") == band["freqs"],
              f"band {n} has the guest's {len(band.get('freqs', ()))} "
              "channels")
        check(band and host.get("ht") == band["ht"] and
              host.get("vht") == band["vht"] and host.get("he") == band["he"],
              f"band {n} has the guest's HT {band.get('ht')}, VHT "
              f"{band.get('vht')} and HE {band.get('he')}")
    check(host_ciphers == guest_ciphers and host_ciphers,
          f"the host's wiphy has the guest's ciphers: {sorted(host_ciphers)}")


def run_tests(guest, log_start):
    if not check(guest.wait_for(f"{NAME}: created wlan device", 90) and
                 guest.wait_for("wlan-hwsim-init: ready", 90),
                 "the guest creates the virtio-wlan device"):
        return
    phy, net = attach()
    if not check(phy is not None,
                 "virtio_wlan registers a wiphy and an interface"):
        for line in kernel_log()[log_start:]:
            if re.search(r"virtio|vdpa|vduse", line):
                print("host: " + line)
        return
    print(f"{NAME} is {phy} with interface {net}")
    test_wiphy(guest, phy)

    r = run("ip", "link", "set", net, "up")
    check(r.returncode == 0 and guest.is_up(),
          f"bringing {net} up brings the guest's wlan0 up {r.stderr}")
    r = run("ip", "link", "set", net, "down")
    check(r.returncode == 0 and not guest.is_up(),
          f"bringing {net} down brings the guest's wlan0 down")

    # The guest reports each disconnection that the host asked for with the
    # id of a connection that has already ended, which the driver ignores.
    disconnects = test_scan_connect(phy, net)
    values = counters(phy)
    check(values.pop("stale_events") == disconnects and
          all(v == 0 for v in values.values()),
          f"the driver's counters are zero, with {disconnects} stale "
          f"disconnections: {counters(phy)}")

    run("vdpa", "dev", "del", NAME, check=True)
    check(not os.path.exists(f"/sys/class/ieee80211/{phy}") and
          not os.path.exists(f"/sys/class/net/{net}"),
          "removing a connected device removes the wiphy and the interface")
    phy, net = attach()
    if not check(phy is not None, "the device can be attached again"):
        return

    guest.kill()
    start = time.time()
    r = run("ip", "link", "set", net, "up")
    took = time.time() - start
    check(r.returncode != 0 and 4 < took < 10 and
          counter(phy, "cmd_timeouts") == 1,
          f"a command to a dead guest times out after {took:.1f} s")
    start = time.time()
    r = run("ip", "link", "set", net, "up")
    took = time.time() - start
    check(r.returncode != 0 and took < 1 and
          counter(phy, "cmd_timeouts") == 1,
          "a broken device fails commands at once")
    print("removing the device, which takes VDUSE's 30 s message timeout")
    run("vdpa", "dev", "del", NAME)
    check(not os.path.exists(f"/sys/class/ieee80211/{phy}") and
          not os.path.exists(f"/sys/class/net/{net}"),
          "a broken device can be removed")

    found = warnings(guest.log()[guest.log().find(
        "wlan-hwsim-init: ready"):].splitlines())
    found += warnings(kernel_log()[log_start:])
    for w in found:
        print("warning: " + w)
    check(not found, "neither kernel logs a warning")


def main():
    if len(sys.argv) != 4:
        sys.exit(f"usage: {sys.argv[0]} <guest build dir> <initramfs> "
                 "<output dir>")
    build, initrd, out = (os.path.abspath(a) for a in sys.argv[1:])
    os.makedirs(out, exist_ok=True)

    run("modprobe", "cfg80211", check=True)
    run("modprobe", "vduse", check=True)
    run("modprobe", "virtio_vdpa", check=True)
    run("modprobe", "virtio_wlan", check=True)
    log_start = len(kernel_log())

    country = get_country()
    if not allow_6ghz():
        sys.exit(f"setting the host's country to {COUNTRY} does not permit "
                 "6 GHz")
    guest = Guest(build, initrd, out)
    try:
        run_tests(guest, log_start)
    finally:
        if country:
            run("iw", "reg", "set", country)
        guest.kill()
        run("vdpa", "dev", "del", NAME)
        run(sys.executable, "-B",
            os.path.join(os.path.dirname(__file__), "vduse-destroy.py"), NAME)

    print(f"{len(failures)} failures" if failures else "PASS")
    sys.exit(1 if failures else 0)


if __name__ == "__main__":
    main()
