// Package command runs host commands.
package command

import (
	"fmt"
	"os/exec"
	"strings"
)

// Run runs a command and returns an error with its output if it fails.
func Run(name string, args ...string) error {
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
