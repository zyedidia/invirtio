#!/usr/bin/env python3
# Test the UML guest's virtio-wlan backend (G2 and G3 in docs/VIRTIO_WIFI.md)
# without the host's virtio_wlan driver. Run as root, with the vduse module of
# linux-host/modules loaded.
#
# usage: vduse-wlan-backend-test.py <guest build dir> <initramfs> <output dir>
#
# The guest is the mac80211_hwsim test guest of G5, whose wlan0 the backend
# exports. The script attaches the device to vhost-vdpa and drives its
# command and event queues from user space, as the host's driver would. It
# checks GET_WIPHY, OPEN and CLOSE, a scan of every band, connections by
# BSSID and by SSID, a connection that fails, station queries, keys,
# disconnections, malformed commands and the backend's counters. It then
# kills the guest and checks that a new guest refuses the device that the
# first one left in use. The guest's console goes to guest-<n>.log in the
# output directory.

import ctypes
import fcntl
import glob
import mmap
import os
import select
import signal
import struct
import subprocess
import sys
import time

NAME = "vwlan-test"


def ioc(direction, nr, size):
    return (direction << 30) | (size << 16) | (0xAF << 8) | nr


VHOST_GET_FEATURES = ioc(2, 0x00, 8)
VHOST_SET_FEATURES = ioc(1, 0x00, 8)
VHOST_SET_OWNER = ioc(0, 0x01, 0)
VHOST_SET_VRING_NUM = ioc(1, 0x10, 8)
VHOST_SET_VRING_ADDR = ioc(1, 0x11, 40)
VHOST_SET_VRING_BASE = ioc(1, 0x12, 8)
VHOST_SET_VRING_KICK = ioc(1, 0x20, 8)
VHOST_SET_VRING_CALL = ioc(1, 0x21, 8)
VHOST_SET_BACKEND_FEATURES = ioc(1, 0x25, 8)
VHOST_GET_BACKEND_FEATURES = ioc(2, 0x26, 8)
VHOST_VDPA_GET_DEVICE_ID = ioc(2, 0x70, 4)
VHOST_VDPA_GET_STATUS = ioc(2, 0x71, 1)
VHOST_VDPA_SET_STATUS = ioc(1, 0x72, 1)
VHOST_VDPA_GET_CONFIG = ioc(2, 0x73, 8)
VHOST_VDPA_SET_VRING_ENABLE = ioc(1, 0x75, 8)
VHOST_VDPA_GET_IOVA_RANGE = ioc(2, 0x78, 16)

VHOST_BACKEND_F_IOTLB_MSG_V2 = 1
VHOST_IOTLB_MSG_V2 = 2
VHOST_IOTLB_UPDATE = 2
VHOST_ACCESS_RW = 3

VIRTIO_F_VERSION_1 = 32
VIRTIO_F_ACCESS_PLATFORM = 33
STATUS_ACK, STATUS_DRIVER, STATUS_DRIVER_OK, STATUS_FEATURES_OK = 1, 2, 4, 8
VRING_DESC_F_NEXT, VRING_DESC_F_WRITE = 1, 2

VQ_CMD, VQ_EVENT = 2, 3
CMD_QUEUE_SIZE, EVENT_QUEUE_SIZE = 32, 128
EVENT_BUFFERS = 64

(CMD_GET_WIPHY, CMD_OPEN, CMD_CLOSE, CMD_SCAN, CMD_ABORT_SCAN, CMD_CONNECT,
 CMD_DISCONNECT, CMD_ADD_KEY, CMD_DEL_KEY, CMD_SET_DEFAULT_KEY,
 CMD_SET_DEFAULT_MGMT_KEY, CMD_AUTHORIZE, CMD_GET_STATION,
 CMD_SET_POWER_MGMT, CMD_SET_MAC) = range(1, 16)
EV_BSS, EV_SCAN_DONE, EV_CONNECT_RESULT, EV_DISCONNECTED = range(1, 5)
S_OK, S_INVALID, S_UNSUPPORTED, S_NETDOWN = 0, 2, 3, 6
AUTH_OPEN, AUTH_AUTOMATIC = 0, 3
CIPHER_CCMP = 0x000FAC04

NETWORKS = {f"hwsim-{band}-{kind}" for band in ("2g", "5g")
            for kind in ("open", "wpa2", "wpa2-mfp", "wpa3-transition")}
NETWORKS.add("hwsim-6g-wpa3")
# The allowlist of include/linux/virtio_wlan.h, with extension IDs as
# (255, id).
ALLOWED = {0, 1, 3, 7, 45, 48, 50, 54, 61, 127, 191, 192, 221, 244,
           (255, 35), (255, 36), (255, 106), (255, 108)}

failures = []


def check(cond, what):
    print(("ok    " if cond else "FAIL  ") + what)
    if not cond:
        failures.append(what)
    return cond


def elements(blob):
    """Return the element IDs of a blob, or None if it is malformed."""
    ids, pos = [], 0
    while pos < len(blob):
        if pos + 2 > len(blob) or pos + 2 + blob[pos + 1] > len(blob):
            return None
        eid, n = blob[pos], blob[pos + 1]
        ids.append((255, blob[pos + 2]) if eid == 255 and n else eid)
        pos += 2 + n
    return ids


def ssid_of(blob):
    pos = 0
    while pos + 2 <= len(blob):
        if blob[pos] == 0:
            return blob[pos + 2:pos + 2 + blob[pos + 1]].decode()
        pos += 2 + blob[pos + 1]
    return None


class Queue:
    """A split virtqueue that this process drives as the device's driver."""

    def __init__(self, dev, index, num):
        self.dev, self.index, self.num = dev, index, num
        self.desc = dev.alloc(16 * num)
        self.avail = dev.alloc(6 + 2 * num)
        self.used = dev.alloc(6 + 8 * num)
        self.kickfd = os.eventfd(0, os.EFD_NONBLOCK)
        self.callfd = os.eventfd(0, os.EFD_NONBLOCK)
        self.free = list(range(num))
        self.chains = {}
        self.avail_idx = self.last_used = 0

        fd = dev.fd
        fcntl.ioctl(fd, VHOST_SET_VRING_NUM, struct.pack("II", index, num))
        fcntl.ioctl(fd, VHOST_SET_VRING_ADDR,
                    struct.pack("IIQQQQ", index, 0, dev.iova(self.desc),
                                dev.iova(self.used), dev.iova(self.avail), 0))
        fcntl.ioctl(fd, VHOST_SET_VRING_BASE, struct.pack("II", index, 0))
        fcntl.ioctl(fd, VHOST_SET_VRING_KICK, struct.pack("Ii", index,
                                                          self.kickfd))
        fcntl.ioctl(fd, VHOST_SET_VRING_CALL, struct.pack("Ii", index,
                                                          self.callfd))
        fcntl.ioctl(fd, VHOST_VDPA_SET_VRING_ENABLE,
                    struct.pack("II", index, 1))

    def add(self, bufs):
        """Make a chain of (offset, len, writable) buffers available."""
        idxs = [self.free.pop() for _ in bufs]
        mem = self.dev.mem
        for i, (off, n, writable) in enumerate(bufs):
            flags = VRING_DESC_F_WRITE if writable else 0
            nxt = 0
            if i + 1 < len(bufs):
                flags |= VRING_DESC_F_NEXT
                nxt = idxs[i + 1]
            struct.pack_into("<QIHH", mem, self.desc + 16 * idxs[i],
                             self.dev.iova(off), n, flags, nxt)
        head = idxs[0]
        self.chains[head] = idxs
        struct.pack_into("<H", mem, self.avail + 4 + 2 * (self.avail_idx %
                                                          self.num), head)
        self.avail_idx = (self.avail_idx + 1) & 0xFFFF
        struct.pack_into("<H", mem, self.avail + 2, self.avail_idx)
        os.eventfd_write(self.kickfd, 1)
        return head

    def take_used(self):
        """Return the (head, len) pairs that the device completed."""
        done = []
        idx = struct.unpack_from("<H", self.dev.mem, self.used + 2)[0]
        while self.last_used != idx:
            head, n = struct.unpack_from(
                "<II", self.dev.mem,
                self.used + 4 + 8 * (self.last_used % self.num))
            self.free.extend(self.chains.pop(head))
            done.append((head, n))
            self.last_used = (self.last_used + 1) & 0xFFFF
        return done

    def wait(self, timeout):
        if select.select([self.callfd], [], [], timeout)[0]:
            try:
                os.eventfd_read(self.callfd)
            except BlockingIOError:
                pass


class Device:
    """The virtio-wlan device, driven through /dev/vhost-vdpa-<n>."""

    MEM_SIZE = 4 << 20

    def __init__(self, path):
        self.fd = os.open(path, os.O_RDWR)
        fcntl.ioctl(self.fd, VHOST_SET_OWNER)
        self.device_id = self.get(VHOST_VDPA_GET_DEVICE_ID, "I")[0]
        offered = self.get(VHOST_GET_FEATURES, "Q")[0]
        backend = self.get(VHOST_GET_BACKEND_FEATURES, "Q")[0]
        if not backend & (1 << VHOST_BACKEND_F_IOTLB_MSG_V2):
            sys.exit("vhost-vdpa does not offer IOTLB_MSG_V2")
        fcntl.ioctl(self.fd, VHOST_SET_BACKEND_FEATURES,
                    struct.pack("Q", 1 << VHOST_BACKEND_F_IOTLB_MSG_V2))

        first, _ = self.get(VHOST_VDPA_GET_IOVA_RANGE, "QQ")
        self.base = (first + 0xFFF) & ~0xFFF
        self.mem = mmap.mmap(-1, self.MEM_SIZE)
        uaddr = ctypes.addressof(ctypes.c_char.from_buffer(self.mem))
        os.write(self.fd, struct.pack("IIQQQBB", VHOST_IOTLB_MSG_V2, 0,
                                      self.base, self.MEM_SIZE, uaddr,
                                      VHOST_ACCESS_RW,
                                      VHOST_IOTLB_UPDATE).ljust(72, b"\0"))
        self.next = 0

        cfg = bytearray(struct.pack("II", 0, 16).ljust(24, b"\0"))
        fcntl.ioctl(self.fd, VHOST_VDPA_GET_CONFIG, cfg)
        self.mac = bytes(cfg[8:14])
        self.mtu, self.max_event, self.max_wiphy = struct.unpack_from(
            "<HII", cfg, 14)

        self.set_status(STATUS_ACK | STATUS_DRIVER)
        self.features = offered & ((1 << VIRTIO_F_VERSION_1) |
                                   (1 << VIRTIO_F_ACCESS_PLATFORM))
        fcntl.ioctl(self.fd, VHOST_SET_FEATURES,
                    struct.pack("Q", self.features))
        self.set_status(STATUS_ACK | STATUS_DRIVER | STATUS_FEATURES_OK)

        self.cmdq = Queue(self, VQ_CMD, CMD_QUEUE_SIZE)
        self.evq = Queue(self, VQ_EVENT, EVENT_QUEUE_SIZE)
        self.req = self.alloc(8192)
        self.resp = self.alloc(self.max_wiphy + 8)
        self.event_bufs = {}
        self.events = []
        self.set_status(STATUS_ACK | STATUS_DRIVER | STATUS_FEATURES_OK |
                        STATUS_DRIVER_OK)
        for _ in range(EVENT_BUFFERS):
            self.post_event_buffer(self.alloc(self.max_event))

    def get(self, req, fmt):
        buf = bytearray(struct.calcsize(fmt))
        fcntl.ioctl(self.fd, req, buf)
        return struct.unpack(fmt, buf)

    def set_status(self, status):
        fcntl.ioctl(self.fd, VHOST_VDPA_SET_STATUS, struct.pack("B", status))

    def status(self):
        return self.get(VHOST_VDPA_GET_STATUS, "B")[0]

    def alloc(self, size):
        off = self.next
        self.next = (off + size + 0xFFF) & ~0xFFF
        if self.next > self.MEM_SIZE:
            sys.exit("out of driver memory")
        return off

    def iova(self, off):
        return self.base + off

    def post_event_buffer(self, off):
        head = self.evq.add([(off, self.max_event, True)])
        self.event_bufs[head] = off

    def poll_events(self):
        # Every completed buffer is read before any is posted again, since
        # posting one can reuse the descriptor of another.
        done = [(self.event_bufs.pop(head), n)
                for head, n in self.evq.take_used()]
        datas = [bytes(self.mem[off:off + n]) for off, n in done]
        for off, _ in done:
            self.post_event_buffer(off)
        for data in datas:
            n = len(data)
            if n < 8:
                self.events.append((None, data))
                continue
            kind, flags, length = struct.unpack_from("<HHI", data)
            self.events.append((kind, data[8:8 + length]
                                if length == n - 8 and not flags else None))

    def wait_event(self, kind, timeout=30):
        """Return the payload of the first event of a kind, and the events
        before it."""
        end = time.time() + timeout
        seen = []
        while True:
            self.poll_events()
            while self.events:
                ev = self.events.pop(0)
                if ev[0] == kind:
                    return ev[1], seen
                seen.append(ev)
            left = end - time.time()
            if left <= 0:
                return None, seen
            self.evq.wait(min(left, 0.5))

    def command_raw(self, request, timeout=15):
        """Send one command chain and return (status, payload)."""
        self.mem[self.req:self.req + len(request)] = request
        self.mem[self.resp:self.resp + 8] = bytes(8)
        head = self.cmdq.add([(self.req, len(request), False),
                              (self.resp, self.max_wiphy + 8, True)])
        end = time.time() + timeout
        while time.time() < end:
            for h, n in self.cmdq.take_used():
                if h == head:
                    if n < 8:
                        return None, b""
                    status, _, length = struct.unpack_from(
                        "<HHI", self.mem, self.resp)
                    return status, bytes(
                        self.mem[self.resp + 8:self.resp + 8 + length])
            self.cmdq.wait(0.5)
        return None, b""

    def command(self, kind, payload=b""):
        return self.command_raw(struct.pack("<HHI", kind, 0, len(payload)) +
                                payload)

    def close(self):
        self.set_status(0)
        os.close(self.fd)


def parse_wiphy(data):
    flags, ie_len, ssids, n_bands, n_ciphers, akms = struct.unpack_from(
        "<IHBBBB", data)
    ciphers = struct.unpack_from("<16I", data, 16)[:n_ciphers]
    bands, pos = [], 80
    for _ in range(n_bands):
        band, bflags, n_chans = struct.unpack_from("<BBH", data, pos)
        chans = [struct.unpack_from("<HHI", data, pos + 104 + 8 * i)
                 for i in range(n_chans)]
        bands.append((band, bflags, chans))
        pos += 104 + 8 * n_chans
    return dict(flags=flags, ie_len=ie_len, ssids=ssids, ciphers=ciphers,
                akms=akms, bands=bands, size=pos)


def bss_info(data, off):
    tsf, sig, freq, capab, bi, bssid, ftype, ie_len = struct.unpack_from(
        "<QiHHH6sBxH", data, off)
    return dict(signal=sig, freq=freq, bssid=bssid, ie_len=ie_len)


def scan(dev, scan_id):
    payload = struct.pack("<II6s6sHHH6s", scan_id, 0, bytes(6), bytes(6), 0,
                          0, 0, bytes(6))
    status, _ = dev.command(CMD_SCAN, payload)
    if not check(status == S_OK, f"SCAN {scan_id} is accepted"):
        return {}
    done, seen = dev.wait_event(EV_SCAN_DONE)
    check(done is not None and struct.unpack_from("<I", done)[0] == scan_id,
          f"SCAN_DONE carries id {scan_id}")
    found, bad = {}, []
    for kind, data in seen:
        if kind != EV_BSS or data is None:
            bad.append(kind)
            continue
        info = bss_info(data, 8)
        ies = data[40:40 + info["ie_len"]]
        ids = elements(ies)
        if (struct.unpack_from("<I", data)[0] != scan_id or ids is None or
                not set(ids) <= ALLOWED or len(data) != 40 + info["ie_len"]):
            bad.append(ssid_of(ies))
            continue
        found[ssid_of(ies)] = info
    check(not bad, f"every BSS event is well-formed and allowlisted {bad}")
    return found


def connect(dev, conn_id, ssid, bssid=bytes(6), freq=0):
    payload = struct.pack("<4I5I10I6s6s4H8B32s13s3x", conn_id, 0, 0, 0,
                          *([0] * 5), *([0] * 10), bssid, bytes(6), freq, 0,
                          0x888e, 0, len(ssid), AUTH_OPEN, 0, 0, 0, 0, 0, 0,
                          ssid.encode(), bytes(13))
    return dev.command(CMD_CONNECT, payload)[0]


def connect_result(dev):
    data, _ = dev.wait_event(EV_CONNECT_RESULT, timeout=40)
    if data is None:
        return None
    conn_id, status, req_n, resp_n, treason = struct.unpack_from(
        "<IHHHB", data)
    return dict(id=conn_id, status=status, timeout=treason,
                bss=bss_info(data, 16), req=req_n, resp=resp_n)


def disconnect(dev, conn_id):
    status = dev.command(CMD_DISCONNECT, struct.pack("<IH2x", conn_id, 3))[0]
    data, _ = dev.wait_event(EV_DISCONNECTED, timeout=10)
    if data is None:
        return status, None
    return status, struct.unpack_from("<IHHB", data)


def run_tests(dev, guest):
    check(dev.device_id == 10, "the vDPA device is virtio-wlan (10)")
    check(dev.mtu == 1500 and dev.max_event == 4096 and
          dev.max_wiphy == 16384,
          f"configuration: mac {dev.mac.hex(':')}, mtu {dev.mtu}, "
          f"events {dev.max_event}, wiphy {dev.max_wiphy}")
    check(dev.status() & STATUS_DRIVER_OK, "the device accepts DRIVER_OK")

    status, data = dev.command(CMD_GET_WIPHY)
    if check(status == S_OK, "GET_WIPHY succeeds"):
        w = parse_wiphy(data)
        check(w["size"] == len(data), f"GET_WIPHY is {len(data)} bytes")
        summary = ", ".join(f"band {b} flags {f:#x} {len(c)} channels"
                            for b, f, c in w["bands"])
        check([b for b, _, _ in w["bands"]] == [0, 1, 2],
              f"bands 2.4, 5 and 6 GHz: {summary}")
        check(CIPHER_CCMP in w["ciphers"] and w["ssids"] > 0,
              f"{len(w['ciphers'])} cipher suites, {w['ssids']} scan SSIDs, "
              f"{w['akms']} AKMs, flags {w['flags']:#x}")

    check(dev.command(CMD_GET_WIPHY, b"\0" * 4)[0] == S_INVALID,
          "GET_WIPHY with a payload is INVALID")
    check(dev.command_raw(struct.pack("<HHI", CMD_OPEN, 0, 4))[0] ==
          S_INVALID, "a command whose length disagrees is INVALID")
    check(dev.command_raw(struct.pack("<HHI", CMD_OPEN, 1, 0))[0] ==
          S_INVALID, "a command with flags is INVALID")
    check(dev.command(99)[0] == S_UNSUPPORTED, "an unknown command is "
          "UNSUPPORTED")
    check(dev.command(CMD_SET_MAC, bytes(8))[0] == S_UNSUPPORTED,
          "SET_MAC is UNSUPPORTED")
    check(dev.command(CMD_SCAN, struct.pack("<II6s6sHHH6s", 1, 0, bytes(6),
                                            bytes(6), 0, 0, 0, bytes(6)))[0]
          == S_NETDOWN, "SCAN before OPEN is NETDOWN")

    check(dev.command(CMD_OPEN)[0] == S_OK, "OPEN succeeds")
    found = scan(dev, 1)
    check(set(found) == NETWORKS,
          f"the scan finds the nine networks: {sorted(found)}")
    status, _ = dev.command(CMD_SCAN, struct.pack(
        "<II6s6sHHH6s", 10, 0, bytes(6), bytes(6), 0, 0, 0, bytes(6)))
    check(status == S_OK and
          dev.command(CMD_ABORT_SCAN, struct.pack("<I4x", 7))[0] == S_OK,
          "ABORT_SCAN of another scan succeeds while scan 10 runs")
    done, _ = dev.wait_event(EV_SCAN_DONE)
    check(done is not None and struct.unpack_from("<IB", done) == (10, 0),
          "scan 10 completes without being aborted")

    net = found.get("hwsim-2g-open")
    if net:
        check(connect(dev, 2, "hwsim-2g-open", net["bssid"], net["freq"]) ==
              S_OK, "CONNECT 2 by BSSID is accepted")
        r = connect_result(dev)
        check(r and r["id"] == 2 and r["status"] == 0 and not r["timeout"] and
              r["bss"]["bssid"] == net["bssid"],
              f"CONNECT_RESULT 2 succeeds: {r}")
        status, data = dev.command(CMD_GET_STATION,
                                   struct.pack("<6s2x", net["bssid"]))
        if check(status == S_OK and len(data) == 72, "GET_STATION succeeds"):
            filled, = struct.unpack_from("<I", data)
            signal_dbm = struct.unpack_from("<b", data, 28)[0]
            check(filled & (1 << 8), f"station filled {filled:#x}, signal "
                  f"{signal_dbm} dBm")
        check(dev.command(CMD_AUTHORIZE, struct.pack("<6sBx", net["bssid"],
                                                     1))[0] == S_OK,
              "AUTHORIZE succeeds")
        key = struct.pack("<IBBBB6s2x32s16s", CIPHER_CCMP, 1, 0, 16, 6,
                          bytes(6), bytes(range(16)), bytes(16))
        check(dev.command(CMD_ADD_KEY, key)[0] == S_OK,
              "ADD_KEY of a group key succeeds")
        check(dev.command(CMD_DEL_KEY, struct.pack("<BB6s", 1, 0,
                                                   bytes(6)))[0] == S_OK,
              "DEL_KEY succeeds")
        check(dev.command(CMD_SET_DEFAULT_KEY, struct.pack("<BB6x", 4,
                                                           3))[0] ==
              S_INVALID, "default key 4 is INVALID")
        check(dev.command(CMD_DISCONNECT, struct.pack("<IH2x", 9, 3))[0] ==
              S_OK, "DISCONNECT of another connection succeeds")
        early, _ = dev.wait_event(EV_DISCONNECTED, timeout=1)
        check(early is None and dev.command(
            CMD_GET_STATION, struct.pack("<6s2x", net["bssid"]))[0] == S_OK,
              "it leaves the connection up")
        status, d = disconnect(dev, 2)
        check(status == S_OK and d and d[0] == 2 and d[3] == 1,
              f"DISCONNECT 2 reports a local disconnection: {d}")

    check(connect(dev, 3, "hwsim-5g-open") == S_OK,
          "CONNECT 3 by SSID is accepted")
    r = connect_result(dev)
    check(r and r["id"] == 3 and r["status"] == 0 and
          r["bss"]["freq"] == 5180, f"CONNECT_RESULT 3 succeeds: {r}")
    status, d = disconnect(dev, 3)
    check(status == S_OK and d and d[0] == 3, f"DISCONNECT 3: {d}")

    check(connect(dev, 4, "no-such-network") == S_OK,
          "CONNECT 4 to a missing network is accepted")
    r = connect_result(dev)
    check(r and r["id"] == 4 and r["timeout"] and r["bss"]["freq"] == 0 and
          r["bss"]["bssid"] == bytes(6),
          f"CONNECT_RESULT 4 is a timeout with no BSS: {r}")

    check(dev.command(CMD_CLOSE)[0] == S_OK, "CLOSE succeeds")
    check(dev.command(CMD_SCAN, struct.pack("<II6s6sHHH6s", 5, 0, bytes(6),
                                            bytes(6), 0, 0, 0, bytes(6)))[0]
          == S_NETDOWN, "SCAN after CLOSE is NETDOWN")

    guest.shell("cat /proc/vduse_guest")
    log = guest.wait_for("umem_regs", 10) and guest.wait_for("rx_dropped", 10)
    counters = {}
    for line in guest.log().splitlines():
        parts = line.split()
        if len(parts) == 2 and parts[0] in ("commands", "invalid_commands",
                                            "events", "bss_dropped",
                                            "events_dropped"):
            counters[parts[0]] = int(parts[1])
    check(log and counters.get("commands", 0) > 20 and
          counters.get("invalid_commands", 0) >= 4 and
          counters.get("bss_dropped", 1) == 0 and
          counters.get("events_dropped", 1) == 0,
          f"/proc/vduse_guest shows the backend's counters: {counters}")


class Guest:
    count = 0

    def __init__(self, build, initrd, out):
        Guest.count += 1
        self.path = os.path.join(out, f"guest-{Guest.count}.log")
        self.logf = open(self.path, "w")
        cmd = [os.path.join(build, "linux"), f"umid=vwlan-{os.getpid()}-"
               f"{Guest.count}", "mem=768M", f"initrd={initrd}",
               "mac80211_hwsim.radios=4", "cfg80211.ieee80211_regdom=DE",
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

    def shell(self, line):
        self.proc.stdin.write(line.encode() + b"\n")
        self.proc.stdin.flush()

    def kill(self):
        if self.proc.poll() is None:
            self.proc.send_signal(signal.SIGKILL)
            self.proc.wait()


def write(path, text):
    with open(path, "w") as f:
        f.write(text)


def attach_vhost():
    """Add the device to the vDPA bus and give it to vhost_vdpa."""
    subprocess.run(["modprobe", "vhost_vdpa"], check=True)
    subprocess.run(["vdpa", "dev", "add", "name", NAME, "mgmtdev", "vduse"],
                   check=True)
    sysdev = f"/sys/bus/vdpa/devices/{NAME}"
    if os.path.exists(f"{sysdev}/driver"):
        write(f"{sysdev}/driver/unbind", NAME)
    write("/sys/bus/vdpa/drivers/vhost_vdpa/bind", NAME)
    for _ in range(50):
        nodes = glob.glob(f"{sysdev}/vhost-vdpa-*")
        if nodes:
            return "/dev/" + os.path.basename(nodes[0])
        time.sleep(0.1)
    sys.exit("vhost_vdpa did not bind the device")


def warnings(guest):
    """Return the warnings logged after the guest was ready."""
    log = guest.log()
    log = log[log.find("wlan-hwsim-init: ready"):]
    return [l for l in log.splitlines()
            if "WARNING:" in l or "BUG:" in l or "circular locking" in l]


def main():
    if len(sys.argv) != 4:
        sys.exit(f"usage: {sys.argv[0]} <guest build dir> <initramfs> "
                 "<output dir>")
    build, initrd, out = (os.path.abspath(a) for a in sys.argv[1:])
    os.makedirs(out, exist_ok=True)

    guest = Guest(build, initrd, out)
    dev = None
    try:
        if not check(guest.wait_for(f"{NAME}: created wlan device", 90) and
                     guest.wait_for("wlan-hwsim-init: ready", 90),
                     "the guest creates the virtio-wlan device"):
            return
        dev = Device(attach_vhost())
        run_tests(dev, guest)

        dev.close()
        dev = None
        time.sleep(2)
        found = warnings(guest)
        for w in found:
            print("guest: " + w)
        check(not found, "the guest logs no warning after it is ready")

        guest.kill()
        second = Guest(build, initrd, out)
        check(second.wait_for("cannot take it over", 90),
              "a new guest refuses the device that the first left in use")
        second.kill()
    finally:
        if dev:
            dev.close()
        guest.kill()
        print("removing the device, which can take VDUSE's 30 s message "
              "timeout")
        subprocess.run(["vdpa", "dev", "del", NAME], capture_output=True)
        subprocess.run([sys.executable, "-B",
                        os.path.join(os.path.dirname(__file__),
                                     "vduse-destroy.py"), NAME],
                       capture_output=True)

    print(f"{len(failures)} failures" if failures else "PASS")
    sys.exit(1 if failures else 0)


if __name__ == "__main__":
    main()
