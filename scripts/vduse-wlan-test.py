#!/usr/bin/env python3
# Test the host's virtio_wlan driver against the UML mac80211_hwsim guest of
# docs/VIRTIO_WIFI.md (stages 1 to 4). Run as root on the linux-host kernel.
#
# usage: vduse-wlan-test.py <guest build dir> <initramfs> <output dir>
#
# The script loads virtio_wlan, sets the host's country to DE for the 6 GHz
# band, boots the guest, and attaches its device to virtio_vdpa. It checks that
# the host's wiphy has the guest's bands, channels, capabilities and ciphers,
# and that bringing the host's interface up and down does the same to the
# guest's wlan0. It then scans and checks that every network of the guest is
# found on its band, connects to the open networks on 2.4 and 5 GHz, checks the
# station, gets a DHCP lease, pings the access points' bridge and runs iperf3
# against it in both directions, and disconnects. It then joins the WPA2
# networks, with and without MFP, with wpa_supplicant, checks that the pairwise
# and group keys are renewed, passes the same traffic and disconnects, has
# NetworkManager join three of them and pings through them, checks that a
# missing network fails, and checks that the driver's counters are zero. It
# removes the device while connected and attaches it again, kills the guest,
# and checks that a command times out and that the device can still be removed.
# It also checks that neither kernel logs a warning, and restores the host's
# country. The guest's console goes to guest.log in the output directory.
# NetworkManager can start the system wpa_supplicant, if it is not running,
# when the interface appears or when the test gives it the interface, and the
# test leaves it running.

import glob
import json
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
            "connect_timeouts", "bad_rx_frames", "tx_timeouts",
            "rx_post_errors", "eapol_timeouts"]
GUEST_COUNTERS = ["tx_invalid", "tx_spoofed", "tx_dropped", "rx_oversized",
                  "rx_bad_buffers"]
SERVER = "192.168.100.1"
PASSPHRASE = "invirtio-hwsim"
# The networks that wpa_supplicant joins, with its MFP setting for each. The
# WPA2-PSK networks renew their pairwise key every 6 seconds and their group
# key every 4.
WPA_NETWORKS = [("hwsim-2g-wpa2", 0), ("hwsim-2g-wpa2-mfp", 2),
                ("hwsim-5g-wpa2", 0), ("hwsim-5g-wpa2-mfp", 2),
                ("hwsim-2g-wpa3-transition", 1)]
REKEYED = {"hwsim-2g-wpa2", "hwsim-5g-wpa2"}
# The networks that NetworkManager joins, and the name of its profile.
NM_NETWORKS = [("hwsim-2g-wpa2", 0), ("hwsim-5g-wpa2", 0),
               ("hwsim-5g-wpa2-mfp", 2)]
NM_CONNECTION = "vwlan-test"
# The lease adds only the address and its subnet's route, and no default
# route.
UDHCPC_SCRIPT = """#!/bin/sh
[ "$1" = bound ] && ip addr add "$ip/$mask" dev "$interface"
exit 0
"""

failures = []


def check(cond, what):
    print(("ok: " if cond else "FAIL: ") + what)
    if not cond:
        failures.append(what)
    return cond


# The output of nmcli and the other tools is matched in English.
ENV = {**os.environ, "LC_ALL": "C"}


def run(*args, check=False):
    return subprocess.run(args, capture_output=True, text=True, check=check,
                          env=ENV)


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


# The test's own calls never start the system wpa_supplicant.
WPAS = ["busctl", "--auto-start=no", "call", "fi.w1.wpa_supplicant1",
        "/fi/w1/wpa_supplicant1", "fi.w1.wpa_supplicant1"]


def release_supplicant(net):
    """The system wpa_supplicant keeps an interface that NetworkManager gave
    it after the interface is removed, and takes over a new one with the same
    name, so it is told to drop it. NetworkManager can still be handing it
    the interface, so it must stay without it for a second. Return whether
    it did."""
    since = time.time()
    end = since + 10
    while time.time() < end:
        r = run(*WPAS, "GetInterface", "s", net)
        m = re.match(r'o "(\S+)"', r.stdout)
        if m:
            run(*WPAS, "RemoveInterface", "o", m.group(1))
            since = time.time()
        elif time.time() - since >= 1:
            return True
        time.sleep(0.2)
    return False


def unmanage(net):
    """NetworkManager's wpa_supplicant ends connections that it did not make,
    so the interface must be unmanaged before the test uses it. NetworkManager
    can take a moment to know the interface. Return whether it is
    unmanaged."""
    for _ in range(50):
        run("nmcli", "device", "set", net, "managed", "no")
        r = run("nmcli", "-g", "GENERAL.STATE", "device", "show", net)
        if "unmanaged" in r.stdout or "not running" in r.stderr:
            return release_supplicant(net)
        time.sleep(0.1)
    return False


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
            check(unmanage(net), f"NetworkManager leaves {net} alone")
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


def settled_counter(phy, name):
    """Return a counter once it has not changed for a second. The guest
    reports a disconnection after it has answered the command that asked
    for it."""
    value = counter(phy, name)
    since = time.time()
    end = since + 10
    while time.time() - since < 1 and time.time() < end:
        time.sleep(0.2)
        now = counter(phy, name)
        if now != value:
            value, since = now, time.time()
    return value


def counters(phy):
    return {name: counter(phy, name) for name in COUNTERS}


def band_of(freq):
    return "2g" if freq < 3000 else "5g" if freq < 5935 else "6g"


def parse_scan(text):
    found = {}
    bss = None
    for line in text.splitlines():
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


def scan(net):
    """Return the SSID and frequency of each BSS that a scan finds. A scan
    of the system wpa_supplicant, which can start before the device is
    unmanaged, makes the first attempts fail with EBUSY or end without
    results, so a scan that finds nothing is tried again."""
    empty = 0
    for _ in range(20):
        r = run("timeout", "40", "iw", "dev", net, "scan")
        found = parse_scan(r.stdout)
        if found:
            return found
        if "(-16)" not in r.stderr:
            empty += 1
            if empty == 3:
                break
        time.sleep(1)
    print(f"iw scan returned {r.returncode}: {r.stderr.strip()} "
          f"{r.stdout.strip()[:500]}")
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
        self.proc = subprocess.Popen(["iw", "event"], stdout=subprocess.PIPE,
                                     env=ENV)
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

    def count(self, pattern):
        """Return how many events so far match, without consuming them."""
        try:
            while True:
                data = os.read(self.fd, 65536)
                if not data:
                    break
                self.text += data.decode(errors="replace")
        except BlockingIOError:
            pass
        return len(re.findall(pattern, self.text))

    def close(self):
        self.proc.kill()
        self.proc.wait()


def test_connect(events, net, ssid):
    run("iw", "dev", net, "connect", ssid, check=True)
    m = events.wait_for(rf"{net} \(phy #\d+\): (connected to|failed to "
                        rf"connect|timed out)[^\n]*", 40)
    return m.group(0) if m else "no connection event"


def carrier(net):
    with open(f"/sys/class/net/{net}/carrier") as f:
        return f.read().strip() == "1"


def test_traffic(guest, net, out):
    """Get a lease from the guest's DHCP server, then ping its access points'
    bridge and run iperf3 against it both ways."""
    check(carrier(net), f"{net} has a carrier while connected")
    script = os.path.join(out, "udhcpc.sh")
    with open(script, "w") as f:
        f.write(UDHCPC_SCRIPT)
    os.chmod(script, 0o755)
    r = run("busybox", "udhcpc", "-i", net, "-f", "-n", "-q", "-t", "5",
            "-T", "2", "-s", script)
    addr = run("ip", "-4", "-br", "addr", "show", net).stdout.split()[2:]
    if not check(r.returncode == 0 and addr,
                 f"{net} gets a DHCP lease: {' '.join(addr)}"):
        print(r.stdout + r.stderr)
        return
    try:
        r = run("ping", "-c", "5", "-i", "0.2", "-W", "2", "-I", net, SERVER)
        m = re.search(r"(\d+) received", r.stdout)
        check(r.returncode == 0 and m and m.group(1) == "5",
              f"ping {SERVER}: {m.group(0) if m else r.stdout + r.stderr}")
        for args, what in (((), "to"), (("-R",), "from")):
            # The server stops listening for a moment between tests.
            for _ in range(5):
                r = run("timeout", "30", "iperf3", "-c", SERVER, "--bind-dev",
                        net, "-t", "3", "-J", *args)
                if "Connection refused" not in r.stdout:
                    break
                time.sleep(1)
            try:
                bps = json.loads(r.stdout)["end"]["sum_received"][
                    "bits_per_second"]
            except (ValueError, KeyError):
                bps = 0
            if not check(r.returncode == 0 and bps > 0,
                         f"iperf3 {what} the guest: {bps / 1e6:.0f} Mb/s"):
                print(r.stdout[-2000:] + r.stderr)
                print(guest.output("pidof iperf3; cat /tmp/iperf3.log"))
    finally:
        run("ip", "-4", "addr", "flush", "dev", net)


def wpa_state(net, ctrl):
    r = run("wpa_cli", "-p", ctrl, "-i", net, "status")
    m = re.search(r"^wpa_state=(\w+)", r.stdout, re.M)
    return m.group(1) if m else ""


def test_wpa(guest, net, out, ssid, mfp):
    """Join a network with wpa_supplicant and pass traffic. On a network that
    renews its keys, wait for both to be renewed first."""
    ctrl = os.path.join(out, "wpa")
    conf = os.path.join(out, "wpa_supplicant.conf")
    log = os.path.join(out, f"wpa-{ssid}.log")
    # A network with required MFP also offers the SHA-256 PSK.
    key_mgmt = "WPA-PSK WPA-PSK-SHA256" if mfp == 2 else "WPA-PSK"
    with open(conf, "w") as f:
        f.write(f"ctrl_interface={ctrl}\n"
                f"network={{\n\tssid=\"{ssid}\"\n\tpsk=\"{PASSPHRASE}\"\n"
                f"\tkey_mgmt={key_mgmt}\n\tieee80211w={mfp}\n}}\n")
    with open(log, "w") as logf:
        proc = subprocess.Popen(["wpa_supplicant", "-i", net, "-D", "nl80211",
                                 "-c", conf], stdout=logf,
                                stderr=subprocess.STDOUT, env=ENV)
    joined = False
    try:
        end = time.time() + 30
        while not joined and time.time() < end:
            joined = wpa_state(net, ctrl) == "COMPLETED"
            time.sleep(0.2)
        if not check(joined, f"wpa_supplicant joins {ssid}"):
            with open(log) as f:
                print(f.read()[-3000:])
            return
        if ssid in REKEYED:
            time.sleep(8)
            with open(log) as f:
                text = f.read()
            # A rekey changes the state for a moment.
            end = time.time() + 5
            while (wpa_state(net, ctrl) != "COMPLETED" and
                   time.time() < end):
                time.sleep(0.2)
            check(text.count("WPA: Key negotiation completed") >= 2 and
                  "WPA: Group rekeying completed" in text and
                  wpa_state(net, ctrl) == "COMPLETED",
                  f"{ssid} renews its pairwise and group keys")
            negotiated = text.count("WPA: Key negotiation completed")
        test_traffic(guest, net, out)
        # A message 4 that the access point does not accept still logs a
        # completed negotiation, and ends in a deauthentication once the
        # access point has given up resending message 3.
        if ssid in REKEYED:
            with open(log) as f:
                text = f.read()
            check(text.count("WPA: Key negotiation completed") > negotiated,
                  f"{ssid} renews its pairwise key during the traffic")
            time.sleep(5)
        with open(log) as f:
            text = f.read()
        check(text.count("CTRL-EVENT-CONNECTED") == 1 and
              "CTRL-EVENT-DISCONNECTED" not in text,
              f"{ssid} stays connected throughout")
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait()
    end = time.time() + 10
    while ("Not connected" not in run("iw", "dev", net, "link").stdout and
           time.time() < end):
        time.sleep(0.2)
    check(not carrier(net) and
          "Not connected" in run("iw", "dev", net, "link").stdout,
          f"stopping wpa_supplicant disconnects from {ssid}")


def test_nm(net, ssid, mfp):
    """Have NetworkManager join a network through a profile of its own and
    ping through it, then leave the interface unmanaged again. The profile
    adds no default route and no DNS servers, and is not saved to disk."""
    uuid = None
    try:
        run("nmcli", "device", "set", net, "managed", "yes")
        end = time.time() + 10
        while time.time() < end:
            state = run("nmcli", "-g", "GENERAL.STATE", "device", "show",
                        net).stdout
            if "unavailable" not in state and "unmanaged" not in state:
                break
            time.sleep(0.2)
        # NetworkManager only joins a network that its own scan has found.
        end = time.time() + 30
        while time.time() < end:
            run("nmcli", "device", "wifi", "rescan", "ifname", net, "ssid",
                ssid)
            ssids = run("nmcli", "-g", "SSID", "device", "wifi", "list",
                        "ifname", net).stdout.split()
            if ssid in ssids:
                break
            time.sleep(2)
        pmf = {0: "disable", 1: "optional", 2: "required"}[mfp]
        # A killed run can leave a profile of the same name in memory, whose
        # file is under /run.
        for line in run("nmcli", "-t", "-f", "NAME,UUID,FILENAME",
                        "connection", "show").stdout.splitlines():
            name, old, filename = (line.split(":", 2) + ["", ""])[:3]
            if name == NM_CONNECTION and filename.startswith("/run/"):
                run("nmcli", "connection", "delete", "uuid", old)
        r = run("nmcli", "connection", "add", "save", "no", "type", "wifi",
            "ifname", net, "con-name", NM_CONNECTION, "ssid", ssid,
            "wifi-sec.key-mgmt", "wpa-psk", "wifi-sec.psk", PASSPHRASE,
            "wifi-sec.pmf", pmf, "connection.autoconnect", "no",
            "ipv4.method", "auto", "ipv4.never-default", "yes",
            "ipv4.ignore-auto-dns", "yes", "ipv6.method", "disabled")
        m = re.search(r"\(([0-9a-f-]{36})\)", r.stdout)
        uuid = m.group(1) if m else NM_CONNECTION
        start = time.time()
        r = run("nmcli", "--wait", "45", "connection", "up", "uuid", uuid)
        addr = run("ip", "-4", "-br", "addr", "show", net).stdout.split()[2:]
        if not check(r.returncode == 0 and addr,
                     f"NetworkManager joins {ssid}: {' '.join(addr)}"):
            print(r.stdout + r.stderr)
            print(run("nmcli", "-f", "SSID,BSSID,FREQ,SECURITY", "device",
                      "wifi", "list", "ifname", net).stdout)
            print(run("journalctl", "-u", "NetworkManager", "-o", "cat",
                      "--since", f"@{int(start) - 35}").stdout[-4000:])
            return
        r = run("ping", "-c", "5", "-i", "0.2", "-W", "2", "-I", net, SERVER)
        m = re.search(r"(\d+) received", r.stdout)
        check(r.returncode == 0 and m and m.group(1) == "5",
              f"ping {SERVER} through NetworkManager's connection: "
              f"{m.group(0) if m else r.stdout + r.stderr}")
    finally:
        if uuid:
            run("nmcli", "connection", "down", "uuid", uuid)
            run("nmcli", "connection", "delete", "uuid", uuid)
        check(unmanage(net), f"NetworkManager leaves {net} alone again")
        run("ip", "link", "set", net, "up")


def test_scan_connect(guest, phy, net, out):
    """Scan all three bands, then connect to open and WPA2 networks, pass
    traffic, and leave the last connection up. Return the number of
    disconnections that the host asked for."""
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
            if not check(result.startswith(f"{net}") and
                         "connected to" in result and f"SSID: {ssid}" in link,
                         f"connecting to {ssid}: {result}"):
                m = events.wait_for(rf"{net} \(phy #\d+\): disconnected[^\n]*",
                                    1)
                print(m.group(0) if m else "no disconnection event")
            sta = run("iw", "dev", net, "station", "dump")
            if not check("signal:" in sta.stdout,
                         "station dump shows the access point's signal"):
                print(sta.stdout + sta.stderr + link)
            test_traffic(guest, net, out)
            run("iw", "dev", net, "disconnect", check=True)
            disconnects += 1
            m = events.wait_for(rf"{net} \(phy #\d+\): disconnected", 10)
            check(m is not None and not carrier(net) and
                  "Not connected" in run("iw", "dev", net, "link").stdout,
                  f"disconnecting from {ssid} drops the carrier")
        # wpa_supplicant and NetworkManager decide how often they
        # disconnect, so the disconnections that the host asked for are
        # counted from their events.
        before = settled_counter(phy, "stale_events")
        events.close()
        events = Events()
        for ssid, mfp in WPA_NETWORKS:
            test_wpa(guest, net, out, ssid, mfp)
        check((counter(phy, "control_port_frames") or 0) > 0 and
              (counter(phy, "control_port_rx_frames") or 0) > 0,
              "wpa_supplicant sends and receives EAPOL frames through the "
              "control port")
        for ssid, mfp in NM_NETWORKS:
            test_nm(net, ssid, mfp)
        after = settled_counter(phy, "stale_events")
        local = events.count(rf"{net} \(phy #\d+\): disconnected "
                             rf"\(local request\)")
        check(before is not None and after is not None and
              after - before == local,
              f"the guest reports the {local} disconnections that "
              f"wpa_supplicant and NetworkManager asked for, and no other")
        disconnects += local
        # The events of wpa_supplicant's connections are not the test's.
        events.close()
        events = Events()
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
    disconnects = test_scan_connect(guest, phy, net,
                                    os.path.dirname(guest.path))
    values = counters(phy)
    check(values.pop("stale_events") == disconnects and
          all(v == 0 for v in values.values()),
          f"the driver's counters are zero, with {disconnects} stale "
          f"disconnections: {counters(phy)}")
    stats = dict(re.findall(r"^\s+(\w+)\s+(\d+)\s*$",
                            guest.output("cat /proc/vduse_guest"), re.M))
    check(all(stats.get(name) == "0" for name in GUEST_COUNTERS),
          "the backend's drop counters are zero: " +
          ", ".join(f"{k} {v}" for k, v in stats.items()
                    if k.startswith(("tx_", "rx_"))))
    # The hwsim guest sends a frame before dev_queue_xmit() returns, so only
    # this counter shows that the backend completes control port frames from
    # their destructor.
    check(int(stats.get("tx_chained", "0")) > 0,
          "the backend completes control port frames once they are sent")

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
