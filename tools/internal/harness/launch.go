package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Launch describes a guest that the launcher runs in its sandbox, as
// invirtio does, exporting one VDUSE device.
type Launch struct {
	// Build is the guest's build directory, with linux and
	// initramfs/initramfs.cpio.
	Build string
	// Type and Name are the VDUSE device's.
	Type string
	Name string
	// Options are added to the device's vduse_guest.device= argument.
	Options string
	// LogDir holds the in-flight log, on a tmpfs, and outlives the guest so
	// that a relaunched one finds the log.
	LogDir string
	// Devices are the PCI devices that the guest drives, bound to vfio-pci
	// by the launcher.
	Devices []string
	// Args are added to the guest's command line.
	Args []string
}

// MakeLogDir makes a directory on a tmpfs for in-flight logs.
func MakeLogDir(name string) (string, error) {
	return os.MkdirTemp("/dev/shm", name+"-")
}

// LaunchGuest starts the guest under the launcher, with its console in the
// log file at path.
func LaunchGuest(path string, l Launch) (*Guest, error) {
	argv := []string{
		filepath.Join(RepoRoot(), "build/invirtio-launch"), "-u", "nobody", "-t", "20",
		"-l", "1024", "-i", filepath.Join(l.Build, "initramfs/initramfs.cpio"),
		"-e", fmt.Sprintf("%s,%s,log=%s", l.Type, l.Name, filepath.Join(l.LogDir, "inflight")),
	}
	if len(l.Devices) > 0 {
		argv = append(argv, "-b")
	}
	for _, d := range l.Devices {
		argv = append(argv, "-d", d)
	}
	device := "vduse_guest.device=" + l.Type + "," + l.Name
	if l.Options != "" {
		device += "," + l.Options
	}
	argv = append(argv, filepath.Join(l.Build, "linux"), device)
	return StartGuest(path, append(argv, l.Args...)...)
}

// UMLPid returns the UML process that a launcher started. UML's helper and
// stub processes share its command line, and UML is the one whose parent,
// the launcher's init, does not. Only the launcher's descendants count,
// since other guests run UML the same way.
func UMLPid(launcher int) int {
	isUML := func(pid int) bool {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		return err == nil && strings.Contains(string(data), "vduse_uml.broker_fd=")
	}
	below := func(pid int) bool {
		for p := Parent(pid); p > 1; p = Parent(p) {
			if p == launcher {
				return true
			}
		}
		return false
	}
	procs, _ := filepath.Glob("/proc/[0-9]*")
	for _, p := range procs {
		pid, err := strconv.Atoi(filepath.Base(p))
		if err != nil || !isUML(pid) || !below(pid) {
			continue
		}
		if ppid := Parent(pid); ppid > 0 && !isUML(ppid) {
			return pid
		}
	}
	return 0
}
