// Package harness holds what the tests and benchmarks of invirtio-test
// share: checks and their report, running host commands, waiting, kernel
// log warnings, and UML guests whose shell is driven through their console.
package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"invirtio/internal/pci"
	"invirtio/internal/sysfs"
	"invirtio/internal/vdpa"
)

// Checker records the outcome of each check, which it prints as it goes.
type Checker struct {
	mu       sync.Mutex
	failures []string
}

// Check prints the outcome of a check and returns cond.
func (c *Checker) Check(cond bool, format string, args ...any) bool {
	what := fmt.Sprintf(format, args...)
	if cond {
		fmt.Println("ok: " + what)
	} else {
		fmt.Println("FAIL: " + what)
		c.mu.Lock()
		c.failures = append(c.failures, what)
		c.mu.Unlock()
	}
	return cond
}

// Failures returns the checks that failed.
func (c *Checker) Failures() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.failures...)
}

// Report prints the number of failed checks, and returns an error if any
// failed, for the command to exit with.
func (c *Checker) Report() error {
	n := len(c.Failures())
	if n > 0 {
		return fmt.Errorf("%d check(s) failed", n)
	}
	fmt.Println("all checks passed")
	return nil
}

// Result is the outcome of a command. A command that did not start or was
// killed at its timeout has Code -1.
type Result struct {
	Stdout   string
	Stderr   string
	Code     int
	TimedOut bool
	Err      error
}

// OK reports whether the command exited with status 0.
func (r Result) OK() bool { return r.Code == 0 && r.Err == nil }

// Output returns stdout and stderr together.
func (r Result) Output() string { return r.Stdout + r.Stderr }

// env makes the tools' output, which the tests match, English.
var env = append(os.Environ(), "LC_ALL=C")

// Run runs a command to completion.
func Run(name string, args ...string) Result {
	return RunTimeout(0, name, args...)
}

// RunArgv runs a command given as one list, such as one from InNS.
func RunArgv(argv []string) Result {
	return Run(argv[0], argv[1:]...)
}

// RunTimeout runs a command and kills it if it outlives the timeout, unless
// the timeout is 0.
func RunTimeout(timeout time.Duration, name string, args ...string) Result {
	return RunContext(context.Background(), "", timeout, name, args...)
}

// RunContext runs a command in the directory dir, or the current one, as
// RunTimeout does, and also stops it when the context ends. The command
// runs in a process group of its own, which gets SIGTERM, since fio puts its
// job processes in sessions of their own and stops them on SIGTERM. After
// WaitDelay, the command is killed and its output closed.
func RunContext(ctx context.Context, dir string, timeout time.Duration, name string, args ...string) Result {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	r := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	var exit *exec.ExitError
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		r.Code, r.TimedOut, r.Err = -1, true, ctx.Err()
	case ctx.Err() != nil:
		r.Code, r.Err = -1, ctx.Err()
	case errors.As(err, &exit):
		r.Code = exit.ExitCode()
	case err != nil:
		r.Code, r.Err = -1, err
	}
	return r
}

// MustRun runs a command and returns an error with its output if it fails.
func MustRun(name string, args ...string) error {
	r := Run(name, args...)
	if !r.OK() {
		return fmt.Errorf("%s %s: %s", name, strings.Join(args, " "),
			strings.TrimSpace(r.Output()+errText(r.Err)))
	}
	return nil
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return " " + err.Error()
}

// Modprobe loads host modules.
func Modprobe(mods ...string) error {
	return MustRun("modprobe", append([]string{"-a"}, mods...)...)
}

// Wait polls cond until it holds or the timeout passes, and returns the
// last outcome.
func Wait(cond func() bool, timeout, step time.Duration) bool {
	end := time.Now().Add(timeout)
	for time.Now().Before(end) {
		if cond() {
			return true
		}
		time.Sleep(step)
	}
	return cond()
}

// Read returns a file's contents without surrounding whitespace, or "".
func Read(path string) string {
	s, _ := sysfs.Read(path)
	return s
}

// Write writes to an existing sysfs or procfs file.
func Write(path, value string) error {
	return sysfs.Write(path, value)
}

// Exists reports whether a path exists.
func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Glob returns the base names of the paths that match a pattern.
func Glob(pattern string) []string {
	paths, _ := filepath.Glob(pattern)
	for i, p := range paths {
		paths[i] = filepath.Base(p)
	}
	return paths
}

// KernelLog returns the lines of the host kernel's log.
func KernelLog() []string {
	return strings.Split(strings.TrimRight(Run("dmesg").Stdout, "\n"), "\n")
}

// KernelMark writes a unique line to the host kernel's log, after which
// KernelLogSince returns the log.
func KernelMark() string {
	mark := fmt.Sprintf("invirtio-test: mark %d-%d", os.Getpid(), time.Now().UnixNano())
	os.WriteFile("/dev/kmsg", []byte(mark+"\n"), 0)
	return mark
}

// KernelLogSince returns the kernel log after a mark, or the whole log if
// the mark has left the ring buffer.
func KernelLogSince(mark string) []string {
	lines := KernelLog()
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.Contains(lines[i], mark) {
			return lines[i+1:]
		}
	}
	return lines
}

// Warnings returns the lines that report a kernel warning, a bug or a
// lockdep splat.
func Warnings(lines []string) []string {
	var found []string
	for _, l := range lines {
		if strings.Contains(l, "WARNING:") || strings.Contains(l, "BUG:") ||
			strings.Contains(l, "circular locking") {
			found = append(found, l)
		}
	}
	return found
}

// RepoRoot returns the repository that the running program was built in,
// as the parent of the build directory that holds it.
func RepoRoot() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	return filepath.Dir(filepath.Dir(exe))
}

// PCIDriver returns the driver of a PCI device, or "".
func PCIDriver(addr string) string { return pci.Driver(addr) }

// PCIHolders returns the processes that hold the VFIO group of a PCI
// device open.
func PCIHolders(addr string) []int {
	group, err := pci.IOMMUGroup(addr)
	if err != nil {
		return nil
	}
	dev := "/dev/vfio/" + group
	var pids []int
	fds, _ := filepath.Glob("/proc/[0-9]*/fd/*")
	for _, fd := range fds {
		if link, err := os.Readlink(fd); err == nil && link == dev {
			pid, _ := strconv.Atoi(strings.Split(fd, "/")[2])
			if len(pids) == 0 || pids[len(pids)-1] != pid {
				pids = append(pids, pid)
			}
		}
	}
	return pids
}

// WaitNetdev waits for a PCI device's driver to register its network
// interface and for udev to rename it, and returns its name, or "".
func WaitNetdev(addr string, timeout time.Duration) string {
	if !Wait(func() bool { return PCINetdev(addr) != "" }, timeout, 100*time.Millisecond) {
		return ""
	}
	Run("udevadm", "settle")
	return PCINetdev(addr)
}

// PCINetdev returns the network interface of a PCI device, or "".
func PCINetdev(addr string) string {
	nets := Glob(pci.Path(addr, "net/*"))
	if len(nets) == 0 {
		return ""
	}
	return nets[0]
}

// Attach adds a VDUSE device to the vDPA bus and binds it to virtio_vdpa.
func Attach(name string) error {
	if err := vdpa.Add(name); err != nil {
		return err
	}
	return vdpa.BindVirtio(name)
}

// Detach removes a VDUSE device from the vDPA bus. With guestGone, the
// message timeout is lowered first, since no guest answers the reset.
func Detach(name string, guestGone bool) error {
	return vdpa.Remove(name, guestGone)
}

// Destroy destroys a VDUSE device.
func Destroy(name string) error {
	return vdpa.Destroy(name)
}

// Cleanup removes a VDUSE device whose guest is gone, from the vDPA bus and
// from the host.
func Cleanup(name string) {
	vdpa.Remove(name, true)
	vdpa.Destroy(name)
}

// VirtioNetdevs returns the network interfaces that the host's virtio
// driver made for a device on the vDPA bus.
func VirtioNetdevs(name string) []string {
	return vdpa.HostInterfaces(name)
}

// Guest is a UML guest whose console is its stdin and its log file.
type Guest struct {
	Path  string
	cmd   *exec.Cmd
	stdin interface {
		Write([]byte) (int, error)
		Close() error
	}
	logf  *os.File
	start int64
	marks int
	done  chan struct{}
}

// StartGuest starts a guest from argv, with its console appended to the
// log file at path, and reads the log from where this run starts.
func StartGuest(path string, argv ...string) (*Guest, error) {
	logf, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		return nil, err
	}
	st, err := logf.Stat()
	if err != nil {
		logf.Close()
		return nil, err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout = logf
	cmd.Stderr = logf
	// The guest runs in a process group of its own, so that a terminal's
	// signals reach only the program that started it, which decides how
	// to stop it, and UML's kill(0) when it dies reaches only the guest.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		logf.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		logf.Close()
		return nil, err
	}
	g := &Guest{Path: path, cmd: cmd, stdin: stdin, logf: logf, start: st.Size(),
		done: make(chan struct{})}
	go func() {
		cmd.Wait()
		close(g.done)
	}()
	return g, nil
}

// Pid returns the pid of the guest's first process.
func (g *Guest) Pid() int { return g.cmd.Process.Pid }

// Running reports whether the guest's first process still runs.
func (g *Guest) Running() bool {
	select {
	case <-g.done:
		return false
	default:
		return true
	}
}

// Log returns the guest's console since it started.
func (g *Guest) Log() string {
	data, err := os.ReadFile(g.Path)
	if err != nil || int64(len(data)) < g.start {
		return ""
	}
	return string(data[g.start:])
}

// WaitFor waits until the console shows text, and reports whether it does.
// It stops waiting when the guest exits.
func (g *Guest) WaitFor(text string, timeout time.Duration) bool {
	Wait(func() bool { return strings.Contains(g.Log(), text) || !g.Running() },
		timeout, 200*time.Millisecond)
	return strings.Contains(g.Log(), text)
}

// Send writes a line to the guest's console.
func (g *Guest) Send(line string) error {
	_, err := g.stdin.Write([]byte(line + "\n"))
	return err
}

// Output runs a command in the guest's shell and returns what it printed,
// or "" if it did not finish within the timeout.
func (g *Guest) Output(line string, timeout time.Duration) string {
	g.marks++
	start := fmt.Sprintf("mark-%d-start", g.marks)
	end := fmt.Sprintf("mark-%d-end", g.marks)
	// The console echoes the input, which holds the markers only split.
	split := func(m string) string { return m[:len(m)-1] + "''" + m[len(m)-1:] }
	if g.Send("echo "+split(start)+"; "+line+"; echo "+split(end)) != nil {
		return ""
	}
	if !g.WaitFor(end, timeout) {
		return ""
	}
	log := g.Log()
	i := strings.Index(log, start)
	j := strings.Index(log, end)
	if i < 0 || j < i {
		return ""
	}
	// The console ends its lines with a carriage return too.
	return strings.ReplaceAll(log[i+len(start):j], "\r", "")
}

var (
	queueRE   = regexp.MustCompile(`^\s+queue (\d+)\s*$`)
	counterRE = regexp.MustCompile(`^\s+(\w+)\s+(-?\d+)\s*$`)
)

// ParseCounters parses /proc/vduse_guest. Each queue's counters are
// prefixed by q<index>_.
func ParseCounters(text string) map[string]int64 {
	counters := map[string]int64{}
	prefix := ""
	for _, line := range strings.Split(text, "\n") {
		if m := queueRE.FindStringSubmatch(line); m != nil {
			prefix = "q" + m[1] + "_"
			continue
		}
		if m := counterRE.FindStringSubmatch(line); m != nil {
			v, _ := strconv.ParseInt(m[2], 10, 64)
			counters[prefix+m[1]] = v
		}
	}
	return counters
}

// Counters returns the guest's VDUSE counters, read in its shell.
func (g *Guest) Counters() map[string]int64 {
	return ParseCounters(g.Output("cat /proc/vduse_guest", 10*time.Second))
}

// Signal sends a signal to the guest's first process.
func (g *Guest) Signal(sig syscall.Signal) error {
	return g.cmd.Process.Signal(sig)
}

// Wait waits for the guest to exit, up to the timeout, and reports whether
// it did.
func (g *Guest) Wait(timeout time.Duration) bool {
	select {
	case <-g.done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// ExitCode returns the guest's exit status, or -1 if it was killed or still
// runs.
func (g *Guest) ExitCode() int {
	if g.Running() || g.cmd.ProcessState == nil {
		return -1
	}
	return g.cmd.ProcessState.ExitCode()
}

// Stop sends SIGTERM and waits for the guest to exit, and kills it if it
// does not within the timeout.
func (g *Guest) Stop(timeout time.Duration) {
	if !g.Running() {
		return
	}
	g.Signal(syscall.SIGTERM)
	if !g.Wait(timeout) {
		g.Kill()
	}
}

// Kill kills the guest and waits for it.
func (g *Guest) Kill() {
	if g.Running() {
		g.Signal(syscall.SIGKILL)
		<-g.done
	}
	g.stdin.Close()
	g.logf.Close()
}
