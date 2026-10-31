package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	vduseNameMax = 256
	// VDUSE_DESTROY_DEV is _IOW(0x81, 0x03, char[256]).
	vduseDestroyDev = 0x41008103
)

func vduseExists(name string) bool {
	_, err := os.Stat(filepath.Join("/sys/class/vduse", name))
	return err == nil
}

func vdpaAttached(name string) bool {
	_, err := os.Stat(filepath.Join("/sys/bus/vdpa/devices", name))
	return err == nil
}

func runCmd(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), msg)
	}
	return nil
}

// vdpaAdd adds the device to the vDPA bus. VDUSE refuses the device until
// the guest has set up its queues.
func vdpaAdd(name string) error {
	return runCmd("vdpa", "dev", "add", "name", name, "mgmtdev", "vduse")
}

// bindVirtioVdpa makes sure that virtio_vdpa, which gives the host its
// virtio driver, has a device on the vDPA bus.
func bindVirtioVdpa(name string) error {
	dev := filepath.Join("/sys/bus/vdpa/devices", name)
	link, _ := os.Readlink(filepath.Join(dev, "driver"))
	if filepath.Base(link) == "virtio_vdpa" {
		return nil
	}
	if link != "" {
		if err := writeSysfs(filepath.Join(dev, "driver/unbind"), name); err != nil {
			return err
		}
	}
	return writeSysfs("/sys/bus/vdpa/drivers/virtio_vdpa/bind", name)
}

// vdpaRemove removes the device from the vDPA bus. The host resets the
// device first, and with no guest to answer, the reset waits for VDUSE's
// message timeout, which is lowered when the guest is gone.
func vdpaRemove(name string, guestGone bool) error {
	if !vdpaAttached(name) {
		return nil
	}
	if guestGone {
		writeSysfs(filepath.Join("/sys/class/vduse", name, "msg_timeout"), "1")
	}
	return runCmd("vdpa", "dev", "del", name)
}

func vduseDestroy(name string) error {
	if !vduseExists(name) {
		return nil
	}
	fd, err := unix.Open("/dev/vduse/control", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("cannot open /dev/vduse/control: %v", err)
	}
	defer unix.Close(fd)
	var buf [vduseNameMax]byte
	copy(buf[:], name)
	// A dead guest can leave the device's fd in a cycle of Unix sockets,
	// which the kernel frees soon after, so a busy device is tried for a
	// second.
	for n := 0; ; n++ {
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), vduseDestroyDev,
			uintptr(unsafe.Pointer(&buf[0])))
		if errno == 0 {
			return nil
		}
		if errno != unix.EBUSY || n == 100 || vdpaAttached(name) {
			return fmt.Errorf("cannot destroy the VDUSE device %s: %v", name, errno)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// hostInterfaces returns the network interfaces that the host's virtio
// driver made for the device.
func hostInterfaces(name string) []string {
	nets, _ := filepath.Glob(filepath.Join("/sys/bus/vdpa/devices", name, "virtio*/net/*"))
	for i, n := range nets {
		nets[i] = filepath.Base(n)
	}
	return nets
}
