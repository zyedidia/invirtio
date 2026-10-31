package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	verbose := fs.Bool("v", false, "also show the guest's VDUSE counters, which needs root")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: invirtio status [-v] [<name>]\n\nShow the state of the enabled devices, or of one.")
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
		showDevice(name, cfg.Devices[name], *verbose)
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

func showDevice(name string, dev *Device, verbose bool) {
	unit := "invirtio@" + name + ".service"
	row := func(label, value string) {
		fmt.Printf("  %-16s %s\n", label+":", value)
	}

	fmt.Printf("%s (%s, %s)\n", name, dev.Type, dev.Address)
	active := systemctlState("is-active", unit)
	row("service", active+", "+systemctlState("is-enabled", unit))
	host := pciDriver(dev.Address)
	if host == "" {
		host = "none"
	}
	row("host driver", host)
	if drv, err := guestDriver(dev.Address, modinfoPath); err == nil {
		row("guest driver", drv)
	} else {
		row("guest driver", "none")
	}

	state := "not created"
	if vduseExists(name) {
		state = "created"
		if vdpaAttached(name) && active != "active" && active != "activating" {
			// A net device stays on the bus for the next guest, which does
			// not come when the service stopped before restarting.
			state += ", on the vDPA bus with no guest, invirtio disable removes it"
		} else if vdpaAttached(name) {
			state += ", on the vDPA bus"
		} else {
			state += ", not on the vDPA bus"
		}
	}
	row("VDUSE device", state)

	nets := hostInterfaces(name)
	if len(nets) == 0 {
		row("host interface", "none")
	}
	for _, n := range nets {
		oper, err := readSysfs(filepath.Join("/sys/class/net", n, "operstate"))
		if err != nil {
			oper = "unknown"
		}
		row("host interface", fmt.Sprintf("%s (%s)", n, oper))
	}

	if !verbose {
		return
	}
	// The guest writes the counters and any error, so only printable text
	// of theirs reaches the terminal.
	text, err := mconsole(mconsolePath(name), "proc vduse_guest")
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
