package main

import (
	"bytes"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

var addressRE = regexp.MustCompile(`^[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]$`)

func validAddress(addr string) bool {
	return addressRE.MatchString(addr)
}

// normalizeAddress takes an address with or without its domain, as lspci
// prints it, and returns it with the domain.
func normalizeAddress(s string) (string, error) {
	addr := strings.ToLower(s)
	if strings.Count(addr, ":") == 1 {
		addr = "0000:" + addr
	}
	if !validAddress(addr) {
		return "", fmt.Errorf("%q is not a PCI address like 0000:06:00.0", s)
	}
	return addr, nil
}

func pciPath(addr, file string) string {
	return filepath.Join("/sys/bus/pci/devices", addr, file)
}

func readSysfs(p string) (string, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// writeSysfs writes to an existing sysfs or cgroup file. Opening a missing
// one fails with ENOENT, where O_CREAT would give EACCES.
func writeSysfs(p, value string) error {
	f, err := os.OpenFile(p, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, err = f.WriteString(value)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// pciType returns the backend for a device's PCI class.
func pciType(addr string) (string, error) {
	class, err := readSysfs(pciPath(addr, "class"))
	if err != nil {
		return "", fmt.Errorf("no PCI device %s", addr)
	}
	if typ := classType(class); typ != "" {
		return typ, nil
	}
	return "", fmt.Errorf("%s has PCI class %s, which is neither a WiFi nor an Ethernet controller", addr, class)
}

// classType returns the backend for a PCI class as sysfs gives it, or "".
// Network controllers of the "other" subclass are WiFi cards, and Ethernet
// controllers are NICs.
func classType(class string) string {
	switch {
	case strings.HasPrefix(class, "0x0280"):
		return "wlan"
	case strings.HasPrefix(class, "0x0200"):
		return "net"
	}
	return ""
}

// pciDriver returns the host driver that a device is bound to, or "".
func pciDriver(addr string) string {
	link, err := os.Readlink(pciPath(addr, "driver"))
	if err != nil {
		return ""
	}
	return filepath.Base(link)
}

func iommuGroup(addr string) (string, error) {
	link, err := os.Readlink(pciPath(addr, "iommu_group"))
	if err != nil {
		return "", fmt.Errorf("%s has no IOMMU group, is the IOMMU enabled?", addr)
	}
	return filepath.Base(link), nil
}

// checkGroup checks that VFIO can use the device's IOMMU group once the
// device is on vfio-pci. VFIO refuses a group in which a driver that does
// not manage DMA itself has a device, whatever the device's class, so every
// other device in the group must have no driver, or have one of the
// drivers that do, which are pci-stub, pcieport, vfio-pci and its variants.
func checkGroup(addr string) error {
	group, err := iommuGroup(addr)
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

// guestDriver returns the driver that the guest binds to a device, by
// matching the device's modalias against the PCI aliases of the drivers
// built into the guest.
func guestDriver(addr, modinfo string) (string, error) {
	alias, err := readSysfs(pciPath(addr, "modalias"))
	if err != nil {
		return "", fmt.Errorf("no PCI device %s", addr)
	}
	data, err := os.ReadFile(modinfo)
	if err != nil {
		return "", err
	}
	if mod := matchDriver(alias, data); mod != "" {
		return mod, nil
	}
	return "", fmt.Errorf("the guest has no driver for %s (%s)", addr, alias)
}

// matchDriver returns the module whose PCI alias in modules.builtin.modinfo,
// a list of NUL-terminated <module>.<key>=<value> entries, matches alias.
func matchDriver(alias string, modinfo []byte) string {
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
