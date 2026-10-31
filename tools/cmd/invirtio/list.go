package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"invirtio/internal/pci"
	"invirtio/internal/sysfs"
)

func cmdList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: invirtio list\n\nList the host's WiFi and Ethernet devices, the host driver that has each\nnow, the guest driver that would drive it, and the name it is enabled as.")
	}
	fs.Parse(args)
	if fs.NArg() != 0 {
		fs.Usage()
		os.Exit(2)
	}

	paths, err := filepath.Glob("/sys/bus/pci/devices/*")
	if err != nil {
		return err
	}
	sort.Strings(paths)
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	modinfo, modinfoErr := os.ReadFile(modinfoPath)
	names := lspciNames()

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ADDRESS\tTYPE\tHOST DRIVER\tGUEST DRIVER\tENABLED AS\tDEVICE")
	found := false
	for _, p := range paths {
		addr := filepath.Base(p)
		class, err := sysfs.Read(filepath.Join(p, "class"))
		if err != nil {
			continue
		}
		typ := pci.ClassType(class)
		if typ == "" {
			continue
		}
		found = true

		host := pci.Driver(addr)
		if host == "" {
			host = "none"
		}
		guest := "unknown"
		if modinfoErr == nil {
			alias, _ := sysfs.Read(filepath.Join(p, "modalias"))
			guest = pci.MatchDriver(alias, modinfo)
			if guest == "" {
				guest = "not supported"
			}
		}
		enabled := "-"
		if n, _, err := cfg.lookup(addr); err == nil {
			enabled = n
		}
		name := names[addr]
		if name == "" {
			name = pciIDs(p)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", addr, typ, host, guest, enabled, name)
	}
	w.Flush()

	if !found {
		fmt.Println("The host has no WiFi or Ethernet devices on PCI.")
	}
	if modinfoErr != nil {
		fmt.Println("\nThe guest is not installed, so its drivers are unknown. Run invirtio install.")
	}
	return nil
}

// lspciNames returns each device's name as lspci prints it, without its
// class, or an empty map if lspci is missing.
func lspciNames() map[string]string {
	names := map[string]string{}
	out, err := exec.Command("lspci", "-D").Output()
	if err != nil {
		return names
	}
	for _, line := range strings.Split(string(out), "\n") {
		if addr, name, ok := parseLspci(line); ok {
			names[addr] = name
		}
	}
	return names
}

// parseLspci splits a line of lspci -D, such as "0000:06:00.0 Network
// controller: MEDIATEK Corp. MT7922 ...", into the address and the name.
func parseLspci(line string) (string, string, bool) {
	addr, rest, ok := strings.Cut(line, " ")
	if !ok || !pci.ValidAddress(addr) {
		return "", "", false
	}
	if _, name, ok := strings.Cut(rest, ": "); ok {
		return addr, name, true
	}
	return addr, rest, true
}

// pciIDs returns a device's vendor and device IDs, as vvvv:dddd.
func pciIDs(dir string) string {
	vendor, _ := sysfs.Read(filepath.Join(dir, "vendor"))
	device, _ := sysfs.Read(filepath.Join(dir, "device"))
	return strings.TrimPrefix(vendor, "0x") + ":" + strings.TrimPrefix(device, "0x")
}
