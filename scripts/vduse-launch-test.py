#!/usr/bin/env python3
# Test that a guest run by invirtio-launch exports a VDUSE device through the
# launcher (UML_FEATURES.md, E1). Run as root on the linux-host kernel, with
# the UML mac80211_hwsim guest of docs/VIRTIO_WIFI.md.
#
# usage: vduse-launch-test.py <guest build dir> <initramfs> <output dir>
#
# The script checks that the launcher refuses device arguments that do not
# match its -e options, and logs and device names that it must not use. It
# then boots the guest as nobody and checks that the guest creates the device,
# holds its fd and its log but not the VDUSE control device, and that the log
# belongs to root. It attaches the device, checks that the host gets a wiphy
# and that /proc/vduse_guest can be read through the mconsole while the device
# runs, removes it, and checks that a guest stopped with SIGTERM destroys the
# device. It checks that a device still on the vDPA bus stays with its log
# when the guest stops and that the next guest finds it, and that the launcher
# destroys the device of a guest that was killed. The sandbox must deny no
# system call. The consoles go to launch-*.log in the output directory, and
# the in-flight log goes to a directory in /dev/shm, since the launcher needs
# it on a tmpfs.

import ctypes
import fcntl
import glob
import os
import pwd
import shutil
import signal
import socket
import struct
import subprocess
import sys
import tempfile
import time

NAME = "vlaunch-test"
ROOT = os.path.realpath(os.path.join(os.path.dirname(__file__), ".."))
LAUNCHER = os.path.join(ROOT, "build", "invirtio-launch")
VDUSE_DESTROY_DEV = 0x41008103
# The in-flight log, which main() places on a tmpfs.
LOG = None

failures = []


def check(cond, what):
    print(("ok: " if cond else "FAIL: ") + what)
    if not cond:
        failures.append(what)
    return cond


def run(*args, timeout=None):
    return subprocess.run(args, capture_output=True, text=True,
                          timeout=timeout)


def refused(*args):
    """Run a launcher that must exit at once and return its stderr, or None
    if it did not fail."""
    try:
        r = run(*args, timeout=30)
    except subprocess.TimeoutExpired:
        return None
    return r.stderr if r.returncode else None


def exists():
    return os.path.exists(f"/sys/class/vduse/{NAME}")


def remove():
    """Remove the device from the vDPA bus, without waiting for VDUSE's
    message timeout, and destroy it."""
    if os.path.exists(f"/sys/bus/vdpa/devices/{NAME}"):
        try:
            with open(f"/sys/class/vduse/{NAME}/msg_timeout", "w") as f:
                f.write("1")
        except OSError:
            pass
        run("vdpa", "dev", "del", NAME)
    try:
        fd = os.open("/dev/vduse/control", os.O_RDWR)
        try:
            fcntl.ioctl(fd, VDUSE_DESTROY_DEV,
                        ctypes.create_string_buffer(NAME.encode(), 256))
        finally:
            os.close(fd)
    except OSError:
        pass


class Launcher:
    def __init__(self, build, initrd, out, name, extra=()):
        self.path = os.path.join(out, f"launch-{name}.log")
        self.log_path = LOG
        self.mconsole = os.path.join(out, "mconsole")
        logf = open(self.path, "w")
        cmd = [LAUNCHER, "-u", "nobody", "-i", initrd, "-s", self.mconsole,
               "-t", "20", "-e", f"wlan,{NAME},log={self.log_path}",
               os.path.join(build, "linux"), "mem=768M",
               "mac80211_hwsim.radios=4", "cfg80211.ieee80211_regdom=DE",
               f"vduse_guest.device=wlan,{NAME},netdev=wlan0", *extra]
        self.proc = subprocess.Popen(cmd, stdin=subprocess.DEVNULL,
                                     stdout=logf, stderr=subprocess.STDOUT)
        logf.close()

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

    def stop(self, timeout=40):
        self.proc.send_signal(signal.SIGTERM)
        try:
            return self.proc.wait(timeout)
        except subprocess.TimeoutExpired:
            self.proc.kill()
            return self.proc.wait()

    def kill(self):
        if self.proc.poll() is None:
            self.proc.kill()
            self.proc.wait()


def uid_of(pid):
    try:
        with open(f"/proc/{pid}/status") as f:
            for line in f:
                if line.startswith("Uid:"):
                    return int(line.split()[1])
    except OSError:
        pass
    return None


def uml_pids():
    """Return the UML processes, which alone get the broker's argument."""
    pids = []
    for d in glob.glob("/proc/[0-9]*"):
        try:
            with open(f"{d}/cmdline", "rb") as f:
                if b"vduse_uml.broker_fd=" in f.read():
                    pids.append(int(os.path.basename(d)))
        except OSError:
            pass
    return pids


def fd_targets(pid):
    targets = set()
    for fd in glob.glob(f"/proc/{pid}/fd/*"):
        try:
            targets.add(os.readlink(fd))
        except OSError:
            pass
    return targets


def mconsole(path, command):
    """Send one mconsole request and return the text of its reply."""
    sock = socket.socket(socket.AF_UNIX, socket.SOCK_DGRAM)
    sock.bind(f"\0vlaunch-test-{os.getpid()}")
    sock.settimeout(10)
    try:
        data = command.encode()
        sock.sendto(struct.pack("=III", 0xcafebabe, 2, len(data)) + data,
                    path)
        text = b""
        while True:
            reply = sock.recv(4096)
            err, more, length = struct.unpack("=III", reply[:12])
            text += reply[12:12 + length].rstrip(b"\0")
            if not more:
                return err, text.decode(errors="replace")
    except OSError:
        return 1, ""
    finally:
        sock.close()


def attach():
    """Add the device to the vDPA bus and return the host's wiphy."""
    r = run("vdpa", "dev", "add", "name", NAME, "mgmtdev", "vduse")
    if r.returncode:
        print(r.stderr, end="")
        return None
    for _ in range(100):
        phys = glob.glob(f"/sys/bus/vdpa/devices/{NAME}/virtio*/ieee80211/*")
        if phys:
            return os.path.basename(phys[0])
        time.sleep(0.1)
    return None


def sandbox_ok(launcher):
    return check("The sandbox denied" not in launcher.log(),
                 "the sandbox denies no system call")


def test_args(build, initrd, out):
    """The launcher refuses arguments that do not match its exports before
    it starts a guest. Each case but one also gives the exported device, so
    that only the named error applies."""
    device = f"vduse_guest.device=wlan,{NAME}"
    base = [LAUNCHER, "-u", "nobody", "-i", initrd,
            "-e", f"wlan,{NAME},log={LOG}", os.path.join(build, "linux")]
    cases = [
        ("a device that is not exported",
         [device, "vduse_guest.device=wlan,other"],
         "gives no device that -e exports"),
        ("a device of another type",
         [device, f"vduse_guest.device=net,{NAME}"],
         "gives no device that -e exports"),
        ("a device with its own log", [f"{device},log=/tmp/x"],
         "gives a log"),
        ("a device with its own log under another spelling",
         [f"vduse-guest.device=wlan,{NAME},log=/tmp/x"], "gives a log"),
        ("a broker fd from the caller", [device, "vduse_uml.broker_fd=3"],
         "gives the broker's fd"),
        ("an argument that hides another behind a space",
         [device, "x vduse_uml.broker_fd=3"], "has a space, a quote"),
        ("an argument that hides another behind a vertical tab",
         [device, "x\vvduse_uml.broker_fd=3"], "has a space, a quote"),
        ("an exported device without an argument", [],
         f"no {device} argument"),
    ]
    for what, args, error in cases:
        err = refused(*base, *args)
        check(err is not None and error in err and not exists(),
              f"the launcher refuses {what}: {(err or '').strip()}")

    for name in ("../x", "control"):
        err = refused(LAUNCHER, "-u", "nobody", "-e", f"wlan,{name},log={LOG}",
                      os.path.join(build, "linux"),
                      f"vduse_guest.device=wlan,{name}")
        check(err is not None and "not a valid VDUSE device name" in err,
              f"the launcher refuses the name {name}")

    err = refused(LAUNCHER, "-u", "nobody", "-i", initrd,
                  "-e", f"wlan,{NAME},log={LOG}", "-e", f"net,other,log={LOG}",
                  os.path.join(build, "linux"), device,
                  "vduse_guest.device=net,other")
    check(err is not None and "in use by another device" in err,
          "the launcher refuses two devices with one log")

    # Another user's file in the log's place could be changed by that user.
    theirs = os.path.join(os.path.dirname(LOG), "theirs.inflight")
    with open(theirs, "w"):
        pass
    os.chown(theirs, 65534, 65534)
    err = refused(LAUNCHER, "-u", "nobody", "-i", initrd,
                  "-e", f"wlan,{NAME},log={theirs}",
                  os.path.join(build, "linux"), device)
    check(err is not None and "must belong to root" in err and not exists(),
          "the launcher refuses a log that belongs to another user")

    if run("stat", "-f", "-c", "%T", out).stdout.strip() == "tmpfs":
        print(f"skipped: {out} is a tmpfs, so a log off a tmpfs is not "
              "tested")
        return
    disk = os.path.join(out, "disk.inflight")
    err = refused(LAUNCHER, "-u", "nobody", "-i", initrd,
                  "-e", f"wlan,{NAME},log={disk}",
                  os.path.join(build, "linux"), device)
    check(err is not None and "not on a tmpfs" in err and
          not os.path.exists(disk),
          "the launcher refuses a log that is not on a tmpfs")


def test_export(build, initrd, out):
    """A guest exports the device, which works on the host, and destroys it
    when stopped after it was removed."""
    launcher = Launcher(build, initrd, out, "export")
    try:
        if not check(launcher.wait_for(f"{NAME}: created", 60),
                     "the guest creates the device through the launcher"):
            return
        check(exists(), "the device exists on the host")

        pids = uml_pids()
        uids = {uid_of(p) for p in pids} - {None}
        check(pids and uids == {pwd.getpwnam("nobody").pw_uid},
              f"UML runs as nobody, uid {sorted(uids)}")
        targets = set()
        for p in pids:
            targets |= fd_targets(p)
        check("/dev/vduse/control" not in targets,
              "UML does not hold /dev/vduse/control")
        check(f"/dev/vduse/{NAME}" in targets, "UML holds the device's fd")
        check(launcher.log_path in targets, "UML holds the in-flight log")
        st = os.stat(launcher.log_path)
        check(st.st_uid == 0 and st.st_mode & 0o777 == 0o600 and st.st_size,
              "the log belongs to root, with mode 0600, and is sized")

        phy = attach()
        if check(phy is not None, "the host gets a wiphy"):
            info = run("iw", "phy", phy, "info").stdout
            check("Band 1:" in info and "Band 2:" in info,
                  f"{phy} has the guest's bands")

        # Only the running queues report kick_pending, which uses ppoll.
        for _ in range(50):
            err, text = mconsole(launcher.mconsole, "proc vduse_guest")
            if not err and "kick_pending" in text:
                break
            time.sleep(0.1)
        if not check(not err and NAME in text and "kick_pending" in text,
                     "/proc/vduse_guest can be read while the device runs"):
            print(f"mconsole error {err}: {text}")
        r = run("vdpa", "dev", "del", NAME)
        check(r.returncode == 0, "the device can be removed")

        status = launcher.stop()
        check(status == 0, f"the guest stops with status {status}")
        check(f"{NAME}: destroyed the device" in launcher.log() and
              not exists(), "the guest destroys the device")
        # The launcher also empties the log of a destroyed device when the
        # guest exits, so the guest's warning shows whether its own request
        # worked.
        check(os.path.getsize(LOG) == 0 and
              "did not empty the in-flight log" not in launcher.log(),
              "the launcher empties the log for the guest")
        sandbox_ok(launcher)
    finally:
        launcher.kill()
        remove()


def test_attached(build, initrd, out):
    """A device on the vDPA bus stays with its log when its guest stops, and
    the next guest finds it through the launcher. A wlan device cannot be
    taken over, so that guest refuses it and leaves the log as it was."""
    launcher = Launcher(build, initrd, out, "attached")
    try:
        if not check(launcher.wait_for(f"{NAME}: created", 60),
                     "a second guest creates the device"):
            return
        check(attach() is not None, "the host gets a wiphy")
        size = os.path.getsize(LOG)
        launcher.stop()
        check(f"the device {NAME} stays, since it is in use" in
              launcher.log() and exists(),
              "a device on the bus stays when the guest stops")
        check(size and os.path.getsize(LOG) == size,
              f"the log of a device that stays keeps its {size} bytes")
        sandbox_ok(launcher)

        launcher = Launcher(build, initrd, out, "attached-next")
        check(launcher.wait_for("cannot take it over", 60),
              "the next guest finds the device through the launcher")
        launcher.stop()
        check(exists() and os.path.getsize(LOG) == size,
              "the device and its log stay for the guest after that")
        sandbox_ok(launcher)
    finally:
        launcher.kill()
        remove()


def test_killed(build, initrd, out):
    """The launcher destroys the device of a guest that was killed."""
    launcher = Launcher(build, initrd, out, "killed")
    try:
        if not check(launcher.wait_for(f"{NAME}: created", 60),
                     "a third guest creates the device"):
            return
        # UML's helper processes can be reaped before they are reached.
        for pid in uml_pids():
            try:
                os.kill(pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
        try:
            launcher.proc.wait(20)
        except subprocess.TimeoutExpired:
            pass
        check(f"destroyed the device {NAME}, which the guest left behind" in
              launcher.log() and not exists(),
              "the launcher destroys the device of a killed guest")
        check(os.path.getsize(LOG) == 0,
              "the launcher empties the log of a killed guest")
    finally:
        launcher.kill()
        remove()


def main():
    if len(sys.argv) != 4:
        sys.exit(f"usage: {sys.argv[0]} <guest build dir> <initramfs> "
                 "<output dir>")
    build, initrd, out = (os.path.realpath(a) for a in sys.argv[1:])
    os.makedirs(out, exist_ok=True)
    global LOG
    logs = tempfile.mkdtemp(prefix="vlaunch-test-", dir="/dev/shm")
    LOG = os.path.join(logs, NAME + ".inflight")
    if not os.access(LAUNCHER, os.X_OK):
        sys.exit(f"{LAUNCHER} does not exist, run make in launcher/")
    for module in ("cfg80211", "vduse", "virtio_vdpa", "virtio_wlan"):
        subprocess.run(["modprobe", module], check=True)
    remove()

    try:
        test_args(build, initrd, out)
        test_export(build, initrd, out)
        test_attached(build, initrd, out)
        test_killed(build, initrd, out)
    finally:
        shutil.rmtree(logs)

    if failures:
        print(f"{len(failures)} check(s) failed")
        sys.exit(1)
    print("all checks passed")


if __name__ == "__main__":
    main()
