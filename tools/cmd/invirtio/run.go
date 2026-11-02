package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"

	"invirtio/internal/command"
	"invirtio/internal/sysfs"
	"invirtio/internal/vdpa"
)

// How long the guest has to create its device before the supervisor gives up
// on attaching it.
const attachTimeout = 120 * time.Second

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: invirtio run <name>\n\nRun the guest of an enabled device in the foreground and attach its device\nto the host. The service invirtio@<name> runs this.")
	}
	fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		os.Exit(2)
	}
	requireRoot()
	name := fs.Arg(0)
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	dev, err := cfg.device(name)
	if err != nil {
		return err
	}
	kernel, _, _ := guestFiles(dev.Type)
	if _, err := os.Stat(kernel); err != nil {
		return fmt.Errorf("the guest of a %s device is not installed, run invirtio install", dev.Type)
	}
	return supervise(name, dev)
}

// modules returns the host modules that a device of the type needs.
func modules(typ string) []string {
	mods := []string{"vfio-pci", "vduse", "vdpa", "virtio_vdpa"}
	switch typ {
	case "wlan":
		mods = append(mods, "virtio_wlan")
	case "gpu":
		mods = append(mods, "virtio_gpu")
	}
	return mods
}

func guestArgs(name string, dev *Device, cgroup string) []string {
	kernel, initrd, _ := guestFiles(dev.Type)
	device := dev.Address
	if dev.ROM != "" {
		device += ",rom=" + dev.ROM
	}
	if dev.WCBars != "" {
		device += ",wc_bars=" + dev.WCBars
	}
	args := []string{
		"-u", guestUser, "-b", "-d", device, "-i", initrd,
		"-s", mconsolePath(name), "-t", "30",
		"-e", fmt.Sprintf("%s,%s,log=%s", dev.Type, name, logPath(name)),
	}
	// A GPU's guest pins the BOs that the GPU reaches in system memory.
	if dev.Type == "gpu" {
		args = append(args, "-l", "1024")
	}
	if cgroup != "" {
		args = append(args, "-g", cgroup)
	}
	if dev.IRQCPUs != "" {
		args = append(args, "-a", dev.IRQCPUs)
	}
	backend := "vduse_guest.device=" + dev.Type + "," + name
	for _, opt := range dev.Options {
		backend += "," + opt
	}
	args = append(args, kernel, "mem="+dev.memory(), backend)
	return append(args, dev.Args...)
}

func supervise(name string, dev *Device) error {
	// The launcher's parent-death signal follows the thread that started
	// it, so that thread stays with this goroutine.
	runtime.LockOSThread()
	journalStdout = onJournal(1)
	journalStderr = onJournal(2)

	// A stop signal that arrives while the guest starts is handled once the
	// launcher runs, or ends the run before it starts.
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)

	if err := command.Run("modprobe", append([]string{"-a"}, modules(dev.Type)...)...); err != nil {
		return err
	}
	if err := os.MkdirAll(runDir, 0700); err != nil {
		return err
	}
	if err := os.Chmod(runDir, 0700); err != nil {
		return err
	}
	lock, err := lockDevice(name)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := mountLogDir(name); err != nil {
		return fmt.Errorf("cannot mount the tmpfs for the in-flight log: %v", err)
	}

	// A wlan or gpu device cannot be taken over by a new guest, so one that
	// an earlier guest left on the bus is removed. A net device stays, and
	// the guest reconnects to it.
	if dev.Type != "net" {
		cleanup(name, dev.Type == "gpu")
	}

	cgroup, err := setupCgroup(name, dev)
	if errors.Is(err, errNotService) {
		warn("the guest runs without a cgroup of its own, since %v", err)
	} else if err != nil {
		return fmt.Errorf("cannot bound the guest's memory: %v", err)
	}

	master, slave, err := openPty()
	if err != nil {
		return fmt.Errorf("cannot create the guest's console: %v", err)
	}
	defer master.Close()

	cmd := exec.Command(launcherPath, guestArgs(name, dev, cgroup)...)
	cmd.Stdin = slave
	cmd.Stdout = slave
	// The launcher's stderr is also the guest's, so it goes to the journal
	// through a filter, as the console does.
	errRead, errWrite, err := os.Pipe()
	if err != nil {
		return err
	}
	defer errRead.Close()
	cmd.Stderr = errWrite
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:    true,
		Pdeathsig: syscall.SIGKILL,
	}
	select {
	case <-sigs:
		errWrite.Close()
		return nil
	default:
	}
	err = cmd.Start()
	errWrite.Close()
	if err != nil {
		return err
	}
	slave.Close()

	errDone := make(chan struct{})
	go func() {
		defer close(errDone)
		journal := newLineLogger(name + " launcher")
		buf := make([]byte, 4096)
		for {
			n, err := errRead.Read(buf)
			if n > 0 {
				journal.Write(buf[:n])
			}
			if err != nil {
				journal.close()
				return
			}
		}
	}()

	console := newConsoleServer(master, cmd.Process)
	sock := consolePath(name)
	os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		warn("the console is not available: %v", err)
	} else {
		os.Chmod(sock, 0600)
		go console.serve(ln)
		defer func() {
			ln.Close()
			os.Remove(sock)
			console.close()
		}()
	}

	outputDone := make(chan struct{})
	go func() {
		defer close(outputDone)
		journal := newLineLogger(name)
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				console.output(buf[:n])
				journal.Write(buf[:n])
			}
			if err != nil {
				journal.close()
				return
			}
		}
	}()

	var launcherGone atomic.Bool
	att := startAttacher(name, dev.Type, &launcherGone)

	exited := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		launcherGone.Store(true)
		exited <- err
	}()

	stopping, failed, released := false, false, false
	attFailed := att.failed
	var waitErr error
wait:
	for {
		select {
		case <-sigs:
			// A launcher that exited with the signal leaves no guest to
			// answer the host's reset, and the cleanup below removes the
			// device.
			select {
			case waitErr = <-exited:
				stopping = true
				break wait
			default:
			}
			if !stopping {
				stopping = true
				// The host resets the device as it removes it, which the
				// guest answers while it still runs.
				att.stop()
				if dev.Type == "gpu" {
					waitReleased(name)
					released = true
				}
				if err := vdpa.Remove(name, launcherGone.Load()); err != nil {
					warn("%v", err)
				}
				logf("stopping the guest of %s", name)
			}
			cmd.Process.Signal(syscall.SIGTERM)
		case waitErr = <-exited:
			break wait
		case <-attFailed:
			// A guest whose device never reaches the host is stopped, so
			// that the service fails rather than waits.
			attFailed = nil
			if !stopping {
				stopping = true
				failed = true
				logf("stopping the guest of %s, whose device did not reach the host", name)
				att.stop()
				vdpa.Remove(name, launcherGone.Load())
				cmd.Process.Signal(syscall.SIGTERM)
			}
		}
	}
	// A stop signal that came as the launcher exited is still a stop.
	select {
	case <-sigs:
		stopping = true
	default:
	}
	// VDUSE takes a device onto the bus without its guest, so one that the
	// attacher adds now waits for the message timeout, which is lowered.
	if !vdpa.Attached(name) && vdpa.Exists(name) {
		sysfs.Write(filepath.Join("/sys/class/vduse", name, "msg_timeout"), "1")
	}
	att.stop()
	live := att.isLive()
	// The output ends when the launcher closes the pty and its stderr.
	timeout := time.After(5 * time.Second)
	for _, done := range []chan struct{}{outputDone, errDone} {
		select {
		case <-done:
		case <-timeout:
		}
	}

	// A GPU's guest that exited by itself while the display manager runs on
	// it leaves the display manager and its clients on the device, which
	// stop only once this service has stopped. The device stays on the bus,
	// and a transient unit switches the display manager back to the host's
	// driver, which removes it.
	_, switched := os.Stat(guestDropinPath(name))
	if !stopping && dev.Type == "gpu" && switched == nil {
		logf("switching the display manager back from %s", name)
		if err := command.Run("systemd-run", "--no-block", "--collect", "--quiet",
			"--unit=invirtio-display-"+name, binPath, "display", "off", name); err != nil {
			warn("%v", err)
		}
	} else if stopping || dev.Type != "net" || !live || !vdpa.Attached(name) {
		// A net device stays on the bus only if a live guest had it there
		// and it is still there.
		cleanup(name, dev.Type == "gpu" && !released)
	} else if vdpa.Attached(name) {
		logf("%s stays on the vDPA bus for the next guest", name)
	}
	if failed {
		return fmt.Errorf("the device of %s did not reach the host", name)
	}
	if stopping {
		return nil
	}
	if waitErr == nil {
		return fmt.Errorf("the guest of %s exited", name)
	}
	return fmt.Errorf("the guest of %s exited: %v", name, waitErr)
}

// lockDevice keeps a second supervisor from running the same device, which
// would remove the running guest's device and sockets.
func lockDevice(name string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(runDir, name+".lock"), os.O_RDWR|os.O_CREATE|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another invirtio run has %s, is invirtio@%s running?", name, name)
	}
	return f, nil
}

// mountLogDir mounts the tmpfs that holds a device's in-flight log, unless it
// is mounted. It stays mounted when the guest exits, so that the next guest
// finds the log, and disable unmounts it.
func mountLogDir(name string) error {
	dir := logDir(name)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if isMountPoint(dir) {
		return nil
	}
	return unix.Mount("tmpfs", dir, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, logTmpfsOptions)
}

// isMountPoint reports whether dir is on another filesystem than its parent.
func isMountPoint(dir string) bool {
	var st, parent unix.Stat_t
	if unix.Stat(dir, &st) != nil || unix.Stat(filepath.Dir(dir), &parent) != nil {
		return false
	}
	return st.Dev != parent.Dev
}

// cleanup removes a device whose guest is gone from the host, once no
// process has its DRM nodes open if wait is set.
func cleanup(name string, wait bool) {
	if wait {
		waitReleased(name)
	}
	if err := vdpa.Remove(name, true); err != nil {
		warn("%v", err)
	}
	if err := vdpa.Destroy(name); err != nil {
		warn("%v", err)
	}
}

// attacher adds the device to the vDPA bus once the guest has created it
// and set up its queues.
type attacher struct {
	mu      sync.Mutex
	stopped bool
	// live is set when the device was on the bus and bound while the
	// launcher still ran. An add for a guest that has died waits for the
	// host's probe, which no guest answers, until after the launcher exits,
	// so it never sets live.
	live bool
	done chan struct{}
	// failed is closed when the device does not reach the host.
	failed chan struct{}
}

func startAttacher(name, typ string, launcherGone *atomic.Bool) *attacher {
	a := &attacher{done: make(chan struct{}), failed: make(chan struct{})}
	go func() {
		defer close(a.done)
		ok := false
		defer func() {
			if !ok && !a.isStopped() {
				close(a.failed)
			}
		}()
		deadline := time.Now().Add(attachTimeout)
		var last error
		bounced := false
		for time.Now().Before(deadline) {
			a.mu.Lock()
			if a.stopped {
				a.mu.Unlock()
				return
			}
			if vdpa.Attached(name) {
				err := vdpa.BindVirtio(name)
				a.live = err == nil && !launcherGone.Load()
				a.mu.Unlock()
				if err != nil {
					warn("%s is on the vDPA bus, but not on virtio_vdpa: %v", name, err)
					return
				}
				ok = true
				if typ == "gpu" {
					card := waitCard(name)
					if card == "" {
						warn("%s is on the vDPA bus, but virtio_gpu registered no DRM card for it", name)
						ok = false
						return
					}
					logf("%s is on the vDPA bus, host DRM card %s", name, card)
				} else {
					logf("%s is on the vDPA bus, host interface %s",
						name, strings.Join(waitInterfaces(name), ", "))
				}
				notifyReady()
				return
			}
			// The backing of a GPU's 2D resources stays in the bounce
			// region, which VDUSE sizes before the first attach.
			if vdpa.Exists(name) && typ == "gpu" && !bounced {
				if err := sysfs.Write(filepath.Join("/sys/class/vduse", name, "bounce_size"),
					strconv.Itoa(gpuBounceSize)); err != nil {
					warn("cannot set the bounce region of %s to %d bytes: %v", name, gpuBounceSize, err)
				}
				bounced = true
			}
			if vdpa.Exists(name) {
				last = vdpa.Add(name)
			}
			a.mu.Unlock()
			time.Sleep(200 * time.Millisecond)
		}
		if last != nil {
			warn("cannot attach %s: %v", name, last)
		} else {
			warn("the guest did not create %s within %v", name, attachTimeout)
		}
	}()
	return a
}

func (a *attacher) isStopped() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stopped
}

func (a *attacher) isLive() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.live
}

// stop waits for an attach in progress, and no attach starts after it.
func (a *attacher) stop() {
	a.mu.Lock()
	a.stopped = true
	a.mu.Unlock()
	<-a.done
}

// The size of a GPU's bounce region, in bytes, of which virtio_gpu lets the
// backing of 2D resources take half.
const gpuBounceSize = 256 << 20

// waitCard gives virtio_gpu a moment to register a device's DRM card, and
// returns its name, or "".
func waitCard(name string) string {
	for i := 0; i < 100; i++ {
		cards, _ := filepath.Glob(filepath.Join("/sys/bus/vdpa/devices", name, "drm", "card*"))
		if len(cards) > 0 {
			return filepath.Base(cards[0])
		}
		time.Sleep(100 * time.Millisecond)
	}
	return ""
}

// waitReleased waits until no process has the DRM nodes of a GPU's device
// open, for up to a minute, since removing virtio_gpu's device from under an
// open file breaks the host.
func waitReleased(name string) {
	var pids []string
	for i := 0; i < 600; i++ {
		if pids = drmHolders(name); len(pids) == 0 {
			return
		}
		if i == 0 {
			logf("waiting for the processes %s to close the DRM nodes of %s", strings.Join(pids, ", "), name)
		}
		time.Sleep(100 * time.Millisecond)
	}
	warn("the processes %s still have the DRM nodes of %s open", strings.Join(pids, ", "), name)
}

// drmHolders returns the processes that have a DRM node of a device open, by
// the nodes' device numbers.
func drmHolders(name string) []string {
	nodes := map[uint64]bool{}
	files, _ := filepath.Glob(filepath.Join("/sys/bus/vdpa/devices", name, "drm", "*", "dev"))
	for _, f := range files {
		var major, minor uint32
		if data, err := os.ReadFile(f); err == nil {
			if _, err := fmt.Sscanf(string(data), "%d:%d", &major, &minor); err == nil {
				nodes[unix.Mkdev(major, minor)] = true
			}
		}
	}
	if len(nodes) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var pids []string
	fds, _ := filepath.Glob("/proc/[0-9]*/fd/*")
	for _, fd := range fds {
		var st unix.Stat_t
		if unix.Stat(fd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFCHR || !nodes[st.Rdev] {
			continue
		}
		if pid := strings.Split(fd, "/")[2]; !seen[pid] {
			seen[pid] = true
			pids = append(pids, pid)
		}
	}
	return pids
}

// notifyReady tells systemd that the device is on the host, which a service
// of Type=notify waits for.
func notifyReady() {
	sock := os.Getenv("NOTIFY_SOCKET")
	if sock == "" {
		return
	}
	if sock[0] == '@' {
		sock = "\x00" + sock[1:]
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: sock, Net: "unixgram"})
	if err != nil {
		warn("cannot tell systemd that the device is ready: %v", err)
		return
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("READY=1")); err != nil {
		warn("cannot tell systemd that the device is ready: %v", err)
	}
}

// waitInterfaces gives the host's driver a moment to make its interface.
func waitInterfaces(name string) []string {
	for i := 0; i < 50; i++ {
		if nets := vdpa.HostInterfaces(name); len(nets) > 0 {
			return nets
		}
		time.Sleep(100 * time.Millisecond)
	}
	return []string{"none"}
}

var errNotService = errors.New("invirtio run is not running as the service")

// setupCgroup gives the guest a cgroup of its own below the service's,
// which systemd delegates, so that memory.max bounds all the memory the
// guest pins. systemd starts the supervisor in the subgroup supervisor, so
// that the service's cgroup holds no processes and can enable controllers
// for its children. A host without swap accounting has no memory.swap.max.
func setupCgroup(name string, dev *Device) (string, error) {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	var rel string
	for _, line := range strings.Split(string(data), "\n") {
		if p, ok := strings.CutPrefix(line, "0::"); ok {
			rel = p
		}
	}
	if !strings.HasSuffix(rel, "/invirtio@"+name+".service/supervisor") {
		return "", errNotService
	}
	base := filepath.Join("/sys/fs/cgroup", filepath.Dir(rel))
	limit, err := dev.memoryMax()
	if err != nil {
		return "", err
	}

	guest := filepath.Join(base, "guest")
	if err := os.Mkdir(guest, 0755); err != nil && !os.IsExist(err) {
		return "", err
	}
	steps := []struct {
		file, value string
		optional    bool
	}{
		{filepath.Join(base, "cgroup.subtree_control"), "+memory +pids", false},
		{filepath.Join(guest, "memory.max"), strconv.FormatInt(limit, 10), false},
		{filepath.Join(guest, "memory.swap.max"), "0", true},
		{filepath.Join(guest, "pids.max"), "4096", false},
	}
	for _, s := range steps {
		err := sysfs.Write(s.file, s.value)
		if s.optional && errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("%s: %v", s.file, err)
		}
	}
	return guest, nil
}

// stdoutMu keeps the supervisor's messages and the guest's console lines,
// which go to the journal through stdout, from interleaving.
var stdoutMu sync.Mutex

func logf(format string, args ...any) {
	msg := oneLine(fmt.Sprintf(format, args...))
	if journalStdout {
		msg = "<5>" + msg
	}
	stdoutMu.Lock()
	defer stdoutMu.Unlock()
	fmt.Fprintln(os.Stdout, msg)
}

// lineLogger writes the guest's console to the journal a line at a time,
// without control characters or escape sequences, which the guest chooses.
// Text must be valid UTF-8, and the C1 controls are dropped, since
// journalctl can show a line as it is.
type lineLogger struct {
	prefix  string
	line    []byte
	pending []byte
	escape  bool
	csi     bool
	out     io.Writer
}

const maxLogLine = 1024

func newLineLogger(name string) *lineLogger {
	return &lineLogger{prefix: name + ": ", out: os.Stdout}
}

func (l *lineLogger) Write(p []byte) {
	for _, b := range p {
		l.feed(b)
	}
}

// close writes a last line that has no newline.
func (l *lineLogger) close() {
	if len(l.line) > 0 {
		l.flush()
	}
}

func (l *lineLogger) feed(b byte) {
	if len(l.pending) > 0 || (b >= 0x80 && !l.escape && !l.csi) {
		l.pending = append(l.pending, b)
		if !utf8.FullRune(l.pending) {
			return
		}
		r, size := utf8.DecodeRune(l.pending)
		rest := append([]byte(nil), l.pending[size:]...)
		if (r != utf8.RuneError || size > 1) && r > 0x9f {
			if len(l.line)+size > maxLogLine {
				l.flush()
			}
			l.line = append(l.line, l.pending[:size]...)
		}
		l.pending = l.pending[:0]
		for _, c := range rest {
			l.feed(c)
		}
		return
	}
	switch {
	case l.csi:
		l.csi = b < 0x40 || b > 0x7e
	case l.escape:
		l.escape = false
		l.csi = b == '['
	case b == 0x1b:
		l.escape = true
	case b == '\n':
		l.flush()
	case b == '\t' || (b >= 0x20 && b < 0x7f):
		// A long line is split, so that the guest cannot hide the next
		// message behind it.
		if len(l.line) == maxLogLine {
			l.flush()
		}
		l.line = append(l.line, b)
	}
}

// flush writes a line with one write, so that no other message lands in it.
func (l *lineLogger) flush() {
	line := make([]byte, 0, len(l.prefix)+len(l.line)+4)
	if journalStdout {
		line = append(line, "<7>"...)
	}
	line = append(line, l.prefix...)
	line = append(line, l.line...)
	line = append(line, '\n')
	stdoutMu.Lock()
	l.out.Write(line)
	stdoutMu.Unlock()
	l.line = l.line[:0]
}
