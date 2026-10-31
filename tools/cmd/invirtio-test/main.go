// Command invirtio-test holds InvirtIO's development tools: moving devices
// to and from VFIO, running and cleaning up VDUSE guests by hand, and the
// tests and benchmarks of the guests' backends.
//
// Usage:
//
//	invirtio-test <command> [arguments]
//
// Each command is registered by the file that implements it.
package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"invirtio/internal/harness"
)

type command struct {
	summary string
	run     func(args []string) error
}

var commands = map[string]command{}

// register adds a command. Each file registers its commands in init.
func register(name, summary string, run func(args []string) error) {
	if _, ok := commands[name]; ok {
		panic("invirtio-test: two commands named " + name)
	}
	commands[name] = command{summary, run}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: invirtio-test <command> [arguments]\n\nCommands:")
	names := make([]string, 0, len(commands))
	for n := range commands {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(os.Stderr, "  %-18s %s\n", n, commands[n].summary)
	}
	fmt.Fprintln(os.Stderr, "\nRun \"invirtio-test <command> -h\" for a command's options.")
	os.Exit(2)
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "invirtio-test: "+format+"\n", args...)
	harness.CloseOutput()
	os.Exit(1)
}

// errSuffix formats an error for the end of a check's message, which
// shows it only when there is one.
func errSuffix(err error) string {
	if err == nil {
		return ""
	}
	return ": " + err.Error()
}

// listSuffix formats a list for the end of a check's message, after a
// word that introduces it, and shows nothing for an empty list.
func listSuffix(word string, items []string) string {
	if len(items) == 0 {
		return ""
	}
	return ", " + word + " " + strings.Join(items, ", ")
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
	c, ok := commands[os.Args[1]]
	if !ok {
		usage()
	}
	if err := c.run(os.Args[2:]); err != nil {
		fatal("%v", err)
	}
	harness.CloseOutput()
}
