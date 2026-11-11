package bench

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// A Profile records host CPUs with perf while a job runs.
type Profile struct {
	cmd  *exec.Cmd
	data string
	log  *os.File
}

// StartProfile records the CPUs, or all of them for "", with call graphs,
// from delay after the start until Stop, and at most for limit, into the
// file data. Perf's messages go to <data>.log. Perf runs in a process group
// of its own and is stopped with SIGINT, so that it finishes the file.
func StartProfile(ctx context.Context, cpus, data string, delay, limit time.Duration) (*Profile, error) {
	log, err := os.Create(data + ".log")
	if err != nil {
		return nil, err
	}
	args := []string{"record", "-g", "-F", "999", "-o", data,
		"-D", fmt.Sprint(delay.Milliseconds())}
	if cpus == "" {
		args = append(args, "-a")
	} else {
		args = append(args, "-C", cpus)
	}
	args = append(args, "--", "sleep", fmt.Sprintf("%.3f", (delay+limit).Seconds()))
	cmd := exec.CommandContext(ctx, "perf", args...)
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGINT) }
	cmd.WaitDelay = time.Minute
	if err := cmd.Start(); err != nil {
		log.Close()
		return nil, err
	}
	return &Profile{cmd: cmd, data: data, log: log}, nil
}

// Stop ends the recording and waits for perf to finish the file. Perf
// stops its sleep with SIGTERM and then exits by that signal, or by SIGINT
// if it was stopped before it handled signals.
func (p *Profile) Stop() error {
	defer p.log.Close()
	p.cmd.Process.Signal(syscall.SIGINT)
	err := p.cmd.Wait()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		ws, ok := exit.Sys().(syscall.WaitStatus)
		if ok && ws.Signaled() &&
			(ws.Signal() == syscall.SIGTERM || ws.Signal() == syscall.SIGINT) {
			err = nil
		}
	}
	if err != nil {
		return fmt.Errorf("perf record: %v, see %s.log", err, p.data)
	}
	return os.Chmod(p.data, 0644)
}

// Report writes two reports of the recording, by symbol in <data>.txt and
// by command and object in <data>.dso.txt, which any user can read. Perf
// looks a process's files up in its mount namespace while the process runs,
// so a guest's symbols only resolve once the guest has exited.
func (p *Profile) Report() error {
	for _, r := range []struct {
		file string
		args []string
	}{
		{p.data + ".txt", []string{"--sort", "dso,sym"}},
		{p.data + ".dso.txt", []string{"--sort", "comm,dso", "-g", "none"}},
	} {
		f, err := os.Create(r.file)
		if err != nil {
			return err
		}
		args := append([]string{"report", "-i", p.data, "--stdio", "--no-children",
			"--percent-limit", "0.5"}, r.args...)
		cmd := exec.Command("perf", args...)
		cmd.Stdout, cmd.Stderr = f, f
		err = cmd.Run()
		f.Close()
		if err != nil {
			return fmt.Errorf("perf report: %v, see %s", err, r.file)
		}
		os.Chmod(r.file, 0644)
	}
	return nil
}
