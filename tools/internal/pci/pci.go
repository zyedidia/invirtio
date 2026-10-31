// Package pci finds the host's PCI devices and their drivers, and moves
// devices between their host drivers and vfio-pci.
package pci

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"invirtio/internal/sysfs"
)

var addressRE = regexp.MustCompile(`^[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]$`)

func ValidAddress(addr string) bool {
	return addressRE.MatchString(addr)
}

// NormalizeAddress takes an address with or without its domain, as lspci
// prints it, and returns it with the domain.
func NormalizeAddress(s string) (string, error) {
	addr := strings.ToLower(s)
	if strings.Count(addr, ":") == 1 {
		addr = "0000:" + addr
	}
	if !ValidAddress(addr) {
		return "", fmt.Errorf("%q is not a PCI address like 0000:06:00.0", s)
	}
	return addr, nil
}

func Path(addr, file string) string {
	return filepath.Join("/sys/bus/pci/devices", addr, file)
}

// Type returns the backend for a device's PCI class.
func Type(addr string) (string, error) {
	class, err := sysfs.Read(Path(addr, "class"))
	if err != nil {
		return "", fmt.Errorf("no PCI device %s", addr)
	}
	if typ := ClassType(class); typ != "" {
		return typ, nil
	}
	return "", fmt.Errorf("%s has PCI class %s, which is neither a WiFi nor an Ethernet controller", addr, class)
}

// ClassType returns the backend for a PCI class as sysfs gives it, or "".
// Network controllers of the "other" subclass are WiFi cards, and Ethernet
// controllers are NICs.
func ClassType(class string) string {
	switch {
	case strings.HasPrefix(class, "0x0280"):
		return "wlan"
	case strings.HasPrefix(class, "0x0200"):
		return "net"
	}
	return ""
}

// Driver returns the host driver that a device is bound to, or "".
func Driver(addr string) string {
	link, err := os.Readlink(Path(addr, "driver"))
	if err != nil {
		return ""
	}
	return filepath.Base(link)
}

func IOMMUGroup(addr string) (string, error) {
	link, err := os.Readlink(Path(addr, "iommu_group"))
	if err != nil {
		return "", fmt.Errorf("%s has no IOMMU group, is the IOMMU enabled?", addr)
	}
	return filepath.Base(link), nil
}

// CheckGroup checks that VFIO can use the device's IOMMU group once the
// device is on vfio-pci. VFIO refuses a group in which a driver that does
// not manage DMA itself has a device, whatever the device's class, so every
// other device in the group must have no driver, or have one of the
// drivers that do, which are pci-stub, pcieport, vfio-pci and its variants.
func CheckGroup(addr string) error {
	group, err := IOMMUGroup(addr)
	if err != nil {
		return err
	}
	peers, err := os.ReadDir(filepath.Join("/sys/kernel/iommu_groups", group, "devices"))
	if err != nil {
		return err
	}
	for _, p := range peers {
		peer := p.Name()
		if peer == addr {
			continue
		}
		// The group's link covers peers that are not PCI devices.
		drv := ""
		if link, err := os.Readlink(filepath.Join("/sys/kernel/iommu_groups", group, "devices", peer, "driver")); err == nil {
			drv = filepath.Base(link)
		}
		switch {
		case drv == "", drv == "pci-stub", drv == "pcieport", strings.Contains(drv, "vfio"):
		default:
			return fmt.Errorf("%s shares IOMMU group %s with %s, which %s drives, so VFIO cannot use the group", addr, group, peer, drv)
		}
	}
	return nil
}

// GuestDriver returns the driver that the guest binds to a device, by
// matching the device's modalias against the PCI aliases of the drivers
// built into the guest.
func GuestDriver(addr, modinfo string) (string, error) {
	alias, err := sysfs.Read(Path(addr, "modalias"))
	if err != nil {
		return "", fmt.Errorf("no PCI device %s", addr)
	}
	data, err := os.ReadFile(modinfo)
	if err != nil {
		return "", err
	}
	if mod := MatchDriver(alias, data); mod != "" {
		return mod, nil
	}
	return "", fmt.Errorf("the guest has no driver for %s (%s)", addr, alias)
}

// MatchDriver returns the module whose PCI alias in modules.builtin.modinfo,
// a list of NUL-terminated <module>.<key>=<value> entries, matches alias.
func MatchDriver(alias string, modinfo []byte) string {
	for _, entry := range bytes.Split(modinfo, []byte{0}) {
		mod, pattern, ok := strings.Cut(string(entry), ".alias=")
		if !ok || !strings.HasPrefix(pattern, "pci:") {
			continue
		}
		if m, _ := path.Match(pattern, alias); m {
			return mod
		}
	}
	return ""
}

// InUse returns what holds the block devices behind a device, its mounts
// and its swap, so that the disk the host runs from is not taken away.
func InUse(addr string) []string {
	var uses []string
	blocks, _ := filepath.Glob("/sys/block/*")
	for _, b := range blocks {
		dev, err := filepath.EvalSymlinks(filepath.Join(b, "device"))
		if err != nil || !strings.Contains(dev+"/", "/"+addr+"/") {
			continue
		}
		name := filepath.Base(b)
		out, _ := exec.Command("lsblk", "-nro", "NAME,MOUNTPOINT", "/dev/"+name).Output()
		for _, line := range strings.Split(string(out), "\n") {
			if f := strings.Fields(line); len(f) == 2 {
				uses = append(uses, fmt.Sprintf("/dev/%s is mounted at %s", f[0], f[1]))
			}
		}
		// The disk itself, or one of its partitions, such as sda1 or
		// nvme0n1p1.
		part := regexp.MustCompile("^/dev/" + regexp.QuoteMeta(name) + "(p?[0-9]+)?$")
		out, _ = exec.Command("swapon", "--show=NAME", "--noheadings").Output()
		for _, swap := range strings.Fields(string(out)) {
			if part.MatchString(swap) {
				uses = append(uses, swap+" is active swap")
			}
		}
	}
	return uses
}

// BindVFIO moves a device from its host driver to vfio-pci. driver_override
// is set first, so that the device cannot go back to its own driver once it
// is unbound.
func BindVFIO(addr string) error {
	if Driver(addr) == "vfio-pci" {
		return nil
	}
	if err := exec.Command("modprobe", "vfio-pci").Run(); err != nil {
		return fmt.Errorf("cannot load vfio-pci: %v", err)
	}
	if err := sysfs.Write(Path(addr, "driver_override"), "vfio-pci"); err != nil {
		return err
	}
	if Driver(addr) != "" {
		if err := sysfs.Write(Path(addr, "driver/unbind"), addr); err != nil {
			return err
		}
	}
	sysfs.Write("/sys/bus/pci/drivers_probe", addr)
	if drv := Driver(addr); drv != "vfio-pci" {
		return fmt.Errorf("binding %s to vfio-pci failed, its driver is %q", addr, drv)
	}
	return nil
}

// BindNative returns a device to the host driver that matches it. Clearing
// the override comes first, or the probe binds vfio-pci again.
func BindNative(addr string) error {
	if err := sysfs.Write(Path(addr, "driver_override"), "\n"); err != nil {
		return err
	}
	if Driver(addr) != "" {
		if err := sysfs.Write(Path(addr, "driver/unbind"), addr); err != nil {
			return err
		}
	}
	return sysfs.Write("/sys/bus/pci/drivers_probe", addr)
}
