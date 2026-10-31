package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

// An action is one step of a stage. show is the shell command that does the
// same, which is printed before the action runs.
type action struct {
	show string
	run  func() error
}

type stage struct {
	title string
	// notes, if set, say what happens beyond the commands.
	notes   string
	actions []action
}

// runStages prints each stage's commands and runs them. With dry, it only
// prints them. Unless yes is set, it waits for Enter before each stage.
func runStages(stages []stage, dry, yes bool) error {
	if !dry && !yes && !term.IsTerminal(int(os.Stdin.Fd())) {
		return errors.New("each stage waits for Enter, so stdin must be a terminal, or use -y")
	}
	in := bufio.NewReader(os.Stdin)
	for i, s := range stages {
		if i > 0 {
			fmt.Println()
		}
		fmt.Printf("Stage %d of %d: %s\n", i+1, len(stages), s.title)
		if s.notes != "" {
			for _, line := range strings.Split(s.notes, "\n") {
				fmt.Println("  " + line)
			}
		}
		if len(s.actions) == 0 {
			fmt.Println("  Nothing to do.")
			continue
		}
		// Only a command's first line gets a prompt, and the lines of a
		// heredoc that follow it are its input.
		for _, a := range s.actions {
			for j, line := range strings.Split(a.show, "\n") {
				if j == 0 {
					fmt.Println("  $ " + line)
				} else {
					fmt.Println("    " + line)
				}
			}
		}
		if dry {
			continue
		}
		if !yes {
			fmt.Print("Press Enter to run these, or Ctrl-C to stop. ")
			if _, err := in.ReadString('\n'); err != nil {
				return errors.New("stopped before the stage ran")
			}
		}
		for _, a := range s.actions {
			if err := a.run(); err != nil {
				return err
			}
		}
	}
	if dry {
		fmt.Println("\nThis was a dry run, and nothing was changed.")
	}
	return nil
}

// shellQuote quotes an argument for the shell if it needs it.
func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./:=@%+,") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func showCmd(name string, args ...string) string {
	words := []string{shellQuote(name)}
	for _, a := range args {
		words = append(words, shellQuote(a))
	}
	return strings.Join(words, " ")
}

func cmdAction(name string, args ...string) action {
	return action{
		show: showCmd(name, args...),
		run:  func() error { return runCmd(name, args...) },
	}
}

func mkdirAction(dir string, mode os.FileMode) action {
	return action{
		show: showCmd("mkdir", "-p", "-m", fmt.Sprintf("%04o", mode), dir),
		run:  func() error { return os.MkdirAll(dir, mode) },
	}
}

// copyAction installs a file as install(1) does, as a new file in its place,
// so that a process that runs the old file keeps it.
func copyAction(src, dst string, mode os.FileMode) action {
	return action{
		show: showCmd("install", "-m", fmt.Sprintf("%04o", mode), src, dst),
		run:  func() error { return copyFile(src, dst, mode) },
	}
}

// sysfsAction writes a value to a sysfs file, as echo would. needed, if set,
// decides when the stage runs whether the write is needed, since an earlier
// stage can change the state it depends on.
func sysfsAction(path, value string, needed func() bool) action {
	show := "echo " + shellQuote(value) + " > " + shellQuote(path)
	if value == "" {
		show = "echo > " + shellQuote(path)
	}
	return action{
		show: show,
		run: func() error {
			if needed != nil && !needed() {
				return nil
			}
			return writeSysfs(path, value+"\n")
		},
	}
}

// writeAction writes a file through a temporary file and a rename, as
// install(1) replaces a file, so that a crash leaves the old file or the
// new one.
func writeAction(path, content string, mode os.FileMode) action {
	show := showCmd("install", "-m", fmt.Sprintf("%04o", mode), "/dev/stdin", path) +
		" <<'EOF'\n" + strings.TrimSuffix(content, "\n") + "\nEOF"
	return action{
		show: show,
		run: func() error {
			tmp := path + ".tmp"
			if err := os.WriteFile(tmp, []byte(content), mode); err != nil {
				return err
			}
			if err := os.Chmod(tmp, mode); err != nil {
				os.Remove(tmp)
				return err
			}
			return os.Rename(tmp, path)
		},
	}
}
