#!/usr/bin/env python3
# Check that the host's VDUSE accepts a virtio-wlan device (H1 in
# docs/VIRTIO_WIFI.md). Run as root, with the vduse module of
# linux-host/modules loaded.
#
# usage: vduse-wlan-probe.py [<name>]
#
# The script creates a virtio-wlan device with the queues of the device
# definition, answers its control messages as a guest would, attaches it with
# 'vdpa dev add', and checks that virtio_vdpa registers a virtio device of
# type 10 for it. It then checks that a process without CAP_NET_ADMIN cannot
# create one, and removes the device.

import ctypes
import errno
import fcntl
import glob
import os
import select
import struct
import subprocess
import sys
import threading
import time

VDUSE_BASE = 0x81


def iow(nr, size):
    return (1 << 30) | (size << 16) | (VDUSE_BASE << 8) | nr


VDUSE_SET_API_VERSION = iow(0x01, 8)
VDUSE_CREATE_DEV = iow(0x02, 336)
VDUSE_DESTROY_DEV = iow(0x03, 256)
VDUSE_VQ_SETUP = iow(0x14, 32)

VDUSE_GET_VQ_STATE = 0
MSG_SIZE = 0x98

VIRTIO_ID_MAC80211_WLAN = 10
VIRTIO_F_VERSION_1 = 32
VIRTIO_F_ACCESS_PLATFORM = 33
# The rx, tx, cmd and event queues of include/uapi/linux/virtio_wlan.h.
QUEUE_SIZES = [1024, 512, 32, 128]


def wlan_config():
    """A struct virtio_wlan_config with a locally administered address."""
    mac = bytes([0x02, 0x00, 0x00, 0x00, 0x0a, 0x01])
    return mac + struct.pack("<HII", 1500, 4096, 16384)


def create(ctl, name):
    config = wlan_config()
    features = (1 << VIRTIO_F_VERSION_1) | (1 << VIRTIO_F_ACCESS_PLATFORM)
    arg = (name.encode().ljust(256, b"\0") +
           struct.pack("<IIQII52xI", 0, VIRTIO_ID_MAC80211_WLAN, features,
                       len(QUEUE_SIZES), 4096, len(config)) + config)
    fcntl.ioctl(ctl, VDUSE_CREATE_DEV, bytearray(arg))


def destroy(ctl, name):
    fcntl.ioctl(ctl, VDUSE_DESTROY_DEV,
                ctypes.create_string_buffer(name.encode(), 256))


def serve(fd, stop):
    """Answer every control message, with an empty queue state."""
    while not stop.is_set():
        if not select.select([fd], [], [], 0.1)[0]:
            continue
        try:
            req = os.read(fd, MSG_SIZE)
        except BlockingIOError:
            continue
        kind, request_id = struct.unpack_from("II", req)
        body = b""
        if kind == VDUSE_GET_VQ_STATE:
            body = struct.pack("IH", struct.unpack_from("I", req, 0x18)[0], 0)
        resp = struct.pack("II16x", request_id, 0) + body
        os.write(fd, resp.ljust(MSG_SIZE, b"\0"))


def virtio_device(name):
    for _ in range(50):
        found = glob.glob(f"/sys/bus/vdpa/devices/{name}/virtio*")
        if found:
            return found[0]
        time.sleep(0.1)
    return None


def read(path):
    with open(path) as f:
        return f.read().strip()


def check_net_admin(name):
    """Return the error that creating a device gives without CAP_NET_ADMIN."""
    code = (f"import fcntl, os; import importlib.util as u;"
            f"s = u.spec_from_file_location('p', {repr(os.path.abspath(__file__))});"
            f"m = u.module_from_spec(s); s.loader.exec_module(m);"
            f"ctl = os.open('/dev/vduse/control', os.O_RDWR);"
            f"fcntl.ioctl(ctl, m.VDUSE_SET_API_VERSION, bytearray(8));"
            f"m.create(ctl, {repr(name)})")
    r = subprocess.run(["setpriv", "--inh-caps=-net_admin",
                        "--bounding-set=-net_admin", sys.executable, "-B", "-c",
                        code],
                       capture_output=True, text=True)
    return r.returncode, r.stderr.strip().splitlines()[-1:] or [""]


if __name__ == "__main__":
    name = sys.argv[1] if len(sys.argv) > 1 else "vwlan-probe"
    failed = False

    ctl = os.open("/dev/vduse/control", os.O_RDWR)
    fcntl.ioctl(ctl, VDUSE_SET_API_VERSION, bytearray(8))
    try:
        create(ctl, name)
    except OSError as e:
        sys.exit(f"creating {name} failed: {e.strerror}"
                 " (is the vduse module of linux-host/modules loaded?)")
    print(f"created {name} with queues {QUEUE_SIZES}")

    dev = None
    stop = threading.Event()
    thread = None
    try:
        dev = os.open(f"/dev/vduse/{name}", os.O_RDWR | os.O_NONBLOCK)
        for i, size in enumerate(QUEUE_SIZES):
            fcntl.ioctl(dev, VDUSE_VQ_SETUP,
                        bytearray(struct.pack("<IH26x", i, size)))

        thread = threading.Thread(target=serve, args=(dev, stop), daemon=True)
        thread.start()

        classes = subprocess.run(["vdpa", "mgmtdev", "show", "vduse"],
                                 capture_output=True, text=True).stdout
        print(f"vdpa mgmtdev show vduse: {' '.join(classes.split())}")

        subprocess.run(["vdpa", "dev", "add", "name", name, "mgmtdev", "vduse"],
                       check=True)
        path = virtio_device(name)
        if not path:
            print("FAIL: virtio_vdpa registered no virtio device")
            failed = True
        else:
            device = read(f"{path}/device")
            driver = os.path.basename(os.readlink(f"{path}/driver")) \
                if os.path.exists(f"{path}/driver") else "none"
            print(f"{os.path.basename(path)}: device {device}, "
                  f"vendor {read(f'{path}/vendor')}, driver {driver}, "
                  f"modalias {read(f'{path}/modalias')}")
            if int(device, 16) != VIRTIO_ID_MAC80211_WLAN:
                print("FAIL: the virtio device has the wrong type")
                failed = True

        code, err = check_net_admin(name + "-nocap")
        if code and "Operation not permitted" in err[0]:
            print("without CAP_NET_ADMIN: creating a device fails with EPERM")
        else:
            print(f"FAIL: without CAP_NET_ADMIN: exit {code}, {err[0]}")
            failed = True
            try:
                destroy(ctl, name + "-nocap")
            except OSError:
                pass

        subprocess.run(["vdpa", "dev", "del", name], check=True)
    finally:
        stop.set()
        if thread:
            thread.join()
        if dev is not None:
            os.close(dev)
        try:
            destroy(ctl, name)
            print(f"destroyed {name}")
        except OSError as e:
            print(f"destroying {name} failed: {e.strerror}")
            failed = True

    print("FAIL" if failed else "PASS")
    sys.exit(1 if failed else 0)
