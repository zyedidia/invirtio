package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"invirtio/internal/mconsole"
	"invirtio/internal/pci"
	"invirtio/internal/sysfs"
	"invirtio/internal/vdpa"
)

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	verbose := fs.Bool("v", false, "also show the guest's memory in detail, which needs root for the guest's own view")
	counters := fs.Bool("vv", false, "also show what -v shows and the guest's VDUSE counters, which needs root")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: invirtio status [-v | -vv] [<name>]\n\nShow the state of the enabled devices, or of one.")
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if fs.NArg() > 1 {
		fs.Usage()
		os.Exit(2)
	}

	if _, err := os.Stat(guestPath); err != nil {
		fmt.Println("InvirtIO is not installed.")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	names := cfg.names()
	if fs.NArg() == 1 {
		if _, err := cfg.device(fs.Arg(0)); err != nil {
			return err
		}
		names = []string{fs.Arg(0)}
	}
	if len(names) == 0 {
		fmt.Println("No devices are enabled.")
		return nil
	}
	for i, name := range names {
		if i > 0 {
			fmt.Println()
		}
		level := 0
		if *counters {
			level = 2
		} else if *verbose {
			level = 1
		}
		showDevice(name, cfg.Devices[name], level)
	}
	return nil
}

func systemctlState(verb, unit string) string {
	out, _ := exec.Command("systemctl", verb, unit).Output()
	if s := strings.TrimSpace(string(out)); s != "" {
		return s
	}
	return "unknown"
}

// showDevice shows a device's state, and with a level of 1 the guest's
// memory in detail, and with 2 its VDUSE counters too.
func showDevice(name string, dev *Device, level int) {
	unit := "invirtio@" + name + ".service"
	row := func(label, value string) {
		fmt.Printf("  %-16s %s\n", label+":", value)
	}

	fmt.Printf("%s (%s, %s)\n", name, dev.Type, dev.Address)
	active := systemctlState("is-active", unit)
	row("service", active+", "+systemctlState("is-enabled", unit))
	host := pci.Driver(dev.Address)
	if host == "" {
		host = "none"
	}
	row("host driver", host)
	if drv, err := pci.GuestDriver(dev.Address, modinfoPath); err == nil {
		row("guest driver", drv)
	} else {
		row("guest driver", "none")
	}

	state := "not created"
	if vdpa.Exists(name) {
		state = "created"
		if vdpa.Attached(name) && active != "active" && active != "activating" {
			// A net device stays on the bus for the next guest, which does
			// not come when the service stopped before restarting.
			state += ", on the vDPA bus with no guest, invirtio disable removes it"
		} else if vdpa.Attached(name) {
			state += ", on the vDPA bus"
		} else {
			state += ", not on the vDPA bus"
		}
	}
	row("VDUSE device", state)

	nets := vdpa.HostInterfaces(name)
	if len(nets) == 0 {
		row("host interface", "none")
	}
	for _, n := range nets {
		oper, err := sysfs.Read(filepath.Join("/sys/class/net", n, "operstate"))
		if err != nil {
			oper = "unknown"
		}
		row("host interface", fmt.Sprintf("%s (%s)", n, oper))
	}

	cg := guestCgroup(unit)
	if cg == "" {
		row("memory", "no guest runs")
	} else {
		row("memory", memoryUse(cg))
	}

	if level < 1 {
		return
	}
	if cg != "" {
		row("memory detail", memoryDetail(cg))
		if pinned, err := pinnedMemory(cg); err == nil {
			row("pinned for DMA", formatSize(pinned)+" of that memory")
		} else {
			row("pinned for DMA", "not available")
		}
	}
	// The guest writes its memory figures, its counters and any error, so
	// only printable text of theirs reaches the terminal.
	if meminfo, err := mconsole.Request(mconsolePath(name), "proc meminfo"); err == nil {
		row("guest memory", printable(guestMemory(meminfo)))
	}
	if level < 2 {
		return
	}
	text, err := mconsole.Request(mconsolePath(name), "proc vduse_guest")
	if err != nil {
		row("guest counters", printable(fmt.Sprintf("not available: %v", err)))
		return
	}
	fmt.Println("  guest counters:")
	for _, line := range strings.Split(strings.TrimRight(printable(text), "\n"), "\n") {
		fmt.Println("    " + line)
	}
}

// printable returns s without invalid UTF-8 and without control characters
// other than tabs and newlines, the C1 controls included.
func printable(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == utf8.RuneError, r < 0x20 && r != '\t' && r != '\n', r >= 0x7f && r <= 0x9f:
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
