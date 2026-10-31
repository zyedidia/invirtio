// Command invirtio installs InvirtIO and manages the driver guests that
// export a host's WiFi and Ethernet devices as virtio devices.
//
// Usage:
//
//	invirtio install [-n] [-y] -guest <build dir> [-initramfs <file>] [-launcher <file>]
//	invirtio uninstall [-n] [-y]
//	invirtio enable [-n] [-y] [-name <name>] [-memory <size>] <pci address>
//	invirtio disable [-n] [-y] <name | pci address>
//	invirtio list
//	invirtio status [-v] [<name>]
//	invirtio console <name>
//	invirtio run <name>
//
// Each enabled device runs in a guest of its own, as the systemd service
// invirtio@<name>, which runs "invirtio run <name>".
package main

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

func usage() {
	fmt.Fprint(os.Stderr, `usage: invirtio <command> [arguments]

Commands:
  install    install the guest, the launcher and this tool, and the service
  uninstall  disable every device and remove what install added
  enable     run a device's driver in a guest and export it to the host
  disable    stop a device's guest and return the device to its host driver
  list       list the host's WiFi and Ethernet devices and their drivers
  status     show the state of the devices
  console    attach to a device's guest console, Ctrl-] detaches
  run        run a device's guest, as the service does

Run "invirtio <command> -h" for a command's options.
`)
	os.Exit(2)
}

func fatal(format string, args ...any) {
	prefix := "invirtio: "
	if journalStderr {
		prefix = "<3>" + prefix
	}
	fmt.Fprintln(os.Stderr, prefix+oneLine(fmt.Sprintf(format, args...)))
	os.Exit(1)
}

// journalStdout and journalStderr are set when stdout and stderr go to the
// journal, which takes a syslog priority prefix on each line. journald
// rate-limits each priority group on its own, and the guest's lines are at
// debug, so a guest that floods its console does not make journald drop the
// supervisor's messages, which are at notice and warning.
var journalStdout, journalStderr bool

// onJournal reports whether the fd is the stream that JOURNAL_STREAM names,
// as dev:ino.
func onJournal(fd int) bool {
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil {
		return false
	}
	return os.Getenv("JOURNAL_STREAM") == fmt.Sprintf("%d:%d", st.Dev, st.Ino)
}

// oneLine joins a message's lines, so that every line in the journal has a
// prefix.
func oneLine(s string) string {
	return strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", " ")
}

func warn(format string, args ...any) {
	prefix := "invirtio: "
	if journalStderr {
		prefix = "<4>" + prefix
	}
	fmt.Fprintln(os.Stderr, prefix+oneLine(fmt.Sprintf(format, args...)))
}

func requireRoot() {
	if os.Geteuid() != 0 {
		fatal("must run as root")
	}
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "install":
		err = cmdInstall(args)
	case "uninstall":
		err = cmdUninstall(args)
	case "enable":
		err = cmdEnable(args)
	case "disable":
		err = cmdDisable(args)
	case "list":
		err = cmdList(args)
	case "status":
		err = cmdStatus(args)
	case "console":
		err = cmdConsole(args)
	case "run":
		err = cmdRun(args)
	default:
		usage()
	}
	if err != nil {
		fatal("%v", err)
	}
}
