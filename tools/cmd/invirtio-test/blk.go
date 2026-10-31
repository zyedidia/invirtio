package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"invirtio/internal/harness"
)

func init() {
	register("test-blk", "test the VDUSE virtio-blk backend", cmdTestBlk)
}

const (
	blkName     = "vblk-test"
	blkDiskSize = 256 << 20
	// The reference data covers the start of the disk.
	blkDataSize = 64 << 20
)

type blkTest struct {
	name  string
	about string
	run   func(*blkSuite)
}

var blkTests = []blkTest{
	{"io", "fio verifies the whole disk, with flushes, and the host reads the device's ID", blkTestIO},
	{"fs", "a file system on the disk keeps a file across a remount", blkTestFS},
	{"rebind", "the data survives virtio_blk rebinding and vdpa dev del and add", blkTestRebind},
	{"multiqueue", "four queues on four guest CPUs each serve requests", blkTestMultiqueue},
	{"zero-copy", "fio verifies the disk with guest pages behind the bounce region", blkTestZeroCopy},
	{"stop", "a guest stopped after vdpa dev del destroys its device and log", blkTestStop},
	{"kill", "a guest killed under load is relaunched and fio finishes", blkTestKill},
	{"term", "a guest stopped while attached hands the device to the next one", blkTestTerm},
	{"unused", "a guest killed before the host attaches is replaced cleanly", blkTestUnused},
	{"timeout", "a relaunched guest reports a device that a message timeout broke", blkTestTimeout},
	{"sandbox", "fio verifies the disk of a guest that the launcher runs", blkTestSandbox},
}

type blkSuite struct {
	harness.Checker
	out        string
	guestDir   string
	sandboxDir string
	disk       string
	// ubd is the host path that the guest serves, the disk's file or a
	// device in front of it.
	ubd  string
	log  string
	data string
	ref  []byte
	// The test that runs, its directory, and the guests it started, which a
	// signal kills.
	dir    string
	mu     sync.Mutex
	guests []*harness.Guest
	// ctx ends on a signal, which stops the commands that use the disk.
	ctx context.Context
	// after runs once the test's guests are gone.
	after []func()
}

func cmdTestBlk(args []string) error {
	root := harness.RepoRoot()
	fs := newFlags("test-blk", `
usage: invirtio-test test-blk [options]

Test the VDUSE virtio-blk backend against the host's virtio_blk. Each test
starts its own guests and leaves no device behind. Most tests run UML as
root with a 256 MiB ubd disk backed by a file in the output directory, so
that the data outlives a guest. The sandbox test runs a guest under the
launcher, which serves a ramdisk. Build the guests with
scripts/invirtio-build-guest.sh -t blk-test-ubd and -t blk-test. Every test
checks that the guest's error counters stay at zero and that neither
kernel logs a warning. The guests' consoles go to <test>/guest-<n>.log in
the output directory. Run as root on the linux-host kernel, with fio and
mkfs.ext4.

Tests:`+blkTestList())
	guest := fs.String("guest", filepath.Join(root, "linux-uml/build-blk-test-ubd"), "the build directory of the guest with ubd")
	sandbox := fs.String("sandbox-guest", filepath.Join(root, "linux-uml/build-blk-test"), "the build directory of the guest for the launcher")
	out := fs.String("out", filepath.Join(root, "build/test-blk"), "the output directory")
	runRE := fs.String("run", "", "run only the tests whose names match this regular expression")
	fs.Parse(args)
	if fs.NArg() != 0 {
		fs.Usage()
		os.Exit(2)
	}
	match, err := regexp.Compile(*runRE)
	if err != nil {
		return err
	}
	requireRoot()

	s := &blkSuite{guestDir: *guest, sandboxDir: *sandbox}
	if s.out, err = filepath.Abs(*out); err != nil {
		return err
	}
	if err := s.setup(); err != nil {
		return err
	}

	// A signal stops the commands that use the disk, and the test that runs
	// then fails and cleans up while its guests still serve the disk, and no
	// other test runs. A second signal kills the guests and exits.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.ctx = ctx
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	go func() {
		<-sigs
		fmt.Println("stopping after a signal, a second one kills the guests")
		cancel()
		<-sigs
		s.mu.Lock()
		for _, g := range s.guests {
			g.Kill()
		}
		s.mu.Unlock()
		harness.Cleanup(blkName)
		harness.CloseOutput()
		os.Exit(1)
	}()

	mark := harness.KernelMark()
	for _, t := range blkTests {
		if !match.MatchString(t.name) || ctx.Err() != nil {
			continue
		}
		fmt.Printf("== %s: %s\n", t.name, t.about)
		s.runTest(t)
	}
	if ctx.Err() != nil {
		return fmt.Errorf("stopped by a signal, %d check(s) failed", len(s.Failures()))
	}
	warnings := harness.Warnings(harness.KernelLogSince(mark))
	for _, w := range warnings {
		fmt.Println("warning: " + w)
	}
	s.Check(len(warnings) == 0, "the host's kernel logs no warning")
	return s.Report()
}

func blkTestList() string {
	var b strings.Builder
	for _, t := range blkTests {
		fmt.Fprintf(&b, "\n  %-11s %s", t.name, t.about)
	}
	return b.String()
}

func (s *blkSuite) setup() error {
	for _, tool := range []string{"fio", "mkfs.ext4", "vdpa", "dd", "udevadm"} {
		if harness.Run("sh", "-c", "command -v "+tool).Code != 0 {
			return fmt.Errorf("%s is not installed", tool)
		}
	}
	if err := harness.Modprobe("vduse", "vdpa", "virtio_vdpa", "virtio_blk"); err != nil {
		return err
	}
	// A guest with its bounce region in guest pages pins them.
	unix.Setrlimit(unix.RLIMIT_MEMLOCK, &unix.Rlimit{Cur: unix.RLIM_INFINITY, Max: unix.RLIM_INFINITY})
	if err := os.MkdirAll(s.out, 0755); err != nil {
		return err
	}
	if err := harness.TeeOutput(filepath.Join(s.out, "test.log")); err != nil {
		return err
	}
	s.disk = filepath.Join(s.out, "disk.img")
	s.ubd = s.disk
	s.log = filepath.Join(s.out, blkName+".inflight")
	s.data = filepath.Join(s.out, "data")
	if err := os.WriteFile(s.disk, nil, 0644); err != nil {
		return err
	}
	if err := os.Truncate(s.disk, blkDiskSize); err != nil {
		return err
	}
	s.ref = make([]byte, blkDataSize)
	rand.Read(s.ref)
	return os.WriteFile(s.data, s.ref, 0644)
}

// runTest runs a test from a clean host, and afterwards removes what its
// guests left and checks their logs for kernel warnings.
func (s *blkSuite) runTest(t blkTest) {
	harness.Cleanup(blkName)
	os.Remove(s.log)
	s.dir = filepath.Join(s.out, t.name)
	// An interrupted run can leave the fs test's file system mounted.
	harness.Run("umount", filepath.Join(s.dir, "mnt"))
	os.RemoveAll(s.dir)
	os.MkdirAll(s.dir, 0755)
	s.mu.Lock()
	s.guests = nil
	s.mu.Unlock()

	t.run(s)

	harness.Detach(blkName, s.ctx.Err() != nil)
	for _, g := range s.guests {
		g.Stop(30 * time.Second)
		g.Kill()
	}
	harness.Cleanup(blkName)
	os.Remove(s.log)
	for _, f := range s.after {
		f()
	}
	s.after = nil
	s.ubd = s.disk
	var warnings []string
	for _, g := range s.guests {
		warnings = append(warnings, harness.Warnings(strings.Split(g.Log(), "\n"))...)
	}
	for _, w := range warnings {
		fmt.Println("warning: " + w)
	}
	s.Check(len(warnings) == 0, "%s: no guest logs a kernel warning", t.name)
}

// start starts a guest that serves the ubd disk, with extra options for
// the device and extra arguments for UML, and waits until it has created or
// reconnected to the device.
func (s *blkSuite) start(opts string, args ...string) *harness.Guest {
	// UML runs more than one CPU only with seccomp userspace.
	argv := []string{
		filepath.Join(s.guestDir, "linux"), "umid=" + blkName, "mem=512M", "seccomp=on",
		"initrd=" + filepath.Join(s.guestDir, "initramfs/initramfs.cpio"),
		"ubd0=" + s.ubd,
		"vduse_guest.device=blk," + blkName + ",disk=/dev/ubda,log=" + s.log + opts,
	}
	return s.boot(append(argv, args...))
}

func (s *blkSuite) boot(argv []string) *harness.Guest {
	path := filepath.Join(s.dir, fmt.Sprintf("guest-%d.log", len(s.guests)+1))
	if s.ctx.Err() != nil {
		return nil
	}
	g, err := harness.StartGuest(path, argv...)
	if !s.Check(err == nil, "guest %d starts%s", len(s.guests)+1, errSuffix(err)) {
		return nil
	}
	s.mu.Lock()
	s.guests = append(s.guests, g)
	s.mu.Unlock()
	ready := harness.Wait(func() bool {
		log := g.Log()
		return strings.Contains(log, blkName+": created") ||
			strings.Contains(log, blkName+": reconnecting") ||
			strings.Contains(log, blkName+": the device stopped") || !g.Running()
	}, 60*time.Second, 200*time.Millisecond)
	if !s.Check(ready && g.Running(), "guest %d reaches the device", len(s.guests)) {
		return nil
	}
	return g
}

// logs reports whether the guest's console shows text, as a check.
func (s *blkSuite) logs(g *harness.Guest, text string) bool {
	return s.Check(strings.Contains(g.Log(), text), "the guest logs %q", text)
}

// hostDisk returns the host's block device for the device, or "".
func (s *blkSuite) hostDisk() string {
	blocks := harness.Glob(filepath.Join("/sys/bus/vdpa/devices", blkName, "virtio*/block/*"))
	if len(blocks) == 0 {
		return ""
	}
	return "/dev/" + blocks[0]
}

// virtioDev returns the device's virtio device on the virtio bus.
func (s *blkSuite) virtioDev() string {
	devs := harness.Glob(filepath.Join("/sys/bus/vdpa/devices", blkName, "virtio*"))
	if len(devs) == 0 {
		return ""
	}
	return devs[0]
}

// waitDisk waits for virtio_blk to register the disk and udev to settle,
// and returns it.
func (s *blkSuite) waitDisk() string {
	ok := harness.Wait(func() bool {
		d := s.hostDisk()
		return d != "" && harness.Exists(d)
	}, 60*time.Second, 200*time.Millisecond)
	harness.Run("udevadm", "settle")
	if !s.Check(ok, "the host gets a block device") {
		return ""
	}
	return s.hostDisk()
}

// attach adds the device to the vDPA bus and returns the host's disk.
func (s *blkSuite) attach() string {
	err := harness.Attach(blkName)
	if !s.Check(err == nil, "vdpa dev add attaches the device%s", errSuffix(err)) {
		return ""
	}
	return s.waitDisk()
}

func (s *blkSuite) detach() bool {
	err := harness.Detach(blkName, false)
	return s.Check(err == nil, "vdpa dev del removes the device%s", errSuffix(err))
}

// run runs a command that uses the disk, which a signal stops.
func (s *blkSuite) run(name string, args ...string) harness.Result {
	return harness.RunContext(s.ctx, s.dir, 2*time.Minute, name, args...)
}

// writeData writes the reference data to the start of the disk.
func (s *blkSuite) writeData(dev string) bool {
	r := s.run("dd", "if="+s.data, "of="+dev, "bs=1M", "oflag=direct", "status=none")
	return s.Check(r.OK(), "the reference data is written to %s: %s", dev, strings.TrimSpace(r.Output()))
}

// writeDiskFile writes the reference data to the start of the disk's file
// on the host, while no guest serves it.
func (s *blkSuite) writeDiskFile() bool {
	f, err := os.OpenFile(s.disk, os.O_WRONLY, 0)
	if err == nil {
		_, err = f.WriteAt(s.ref, 0)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	return s.Check(err == nil, "the reference data is written to the disk's file%s", errSuffix(err))
}

// checkData compares the start of the disk with the reference data.
func (s *blkSuite) checkData(dev, after string) bool {
	r := s.run("dd", "if="+dev, "bs=1M", fmt.Sprintf("count=%d", blkDataSize>>20),
		"iflag=direct", "status=none")
	return s.Check(r.OK() && bytes.Equal([]byte(r.Stdout), s.ref), "the data matches after %s", after)
}

// fio runs a job on the disk with the given options, and its output goes to
// <test>/<job>-fio.log.
func (s *blkSuite) fio(job, dev string, args ...string) harness.Result {
	return s.fioJobs(job, dev, args, []string{"--name=" + job})
}

// fioJobs runs fio with options for every job, which fio takes only before
// the first --name, and then the jobs, each a --name and the options of
// that job alone.
func (s *blkSuite) fioJobs(run, dev string, global, jobs []string) harness.Result {
	argv := append([]string{"--filename=" + dev, "--ioengine=io_uring", "--direct=1",
		"--verify=crc32c", "--verify_fatal=1", "--verify_state_save=0",
		"--output=" + filepath.Join(s.dir, run+"-fio.log")}, global...)
	return harness.RunContext(s.ctx, s.dir, 10*time.Minute, "fio", append(argv, jobs...)...)
}

// healthy checks that the guest's error counters are zero.
func (s *blkSuite) healthy(g *harness.Guest) {
	c := g.Counters()
	if !s.Check(len(c) > 0, "the guest reports its counters") {
		return
	}
	var bad []string
	for k, v := range c {
		for _, suffix := range []string{"start_errors", "iova_map_errors", "bad_requests", "inject_errors"} {
			if strings.HasSuffix(k, suffix) && v != 0 {
				bad = append(bad, fmt.Sprintf("%s %d", k, v))
			}
		}
	}
	s.Check(len(bad) == 0, "the guest's error counters are zero%s", listSuffix("but", bad))
}

func blkTestIO(s *blkSuite) {
	g := s.start("")
	if g == nil {
		return
	}
	dev := s.attach()
	if dev == "" {
		return
	}
	serial := harness.Read(filepath.Join("/sys/block", filepath.Base(dev), "serial"))
	s.Check(serial == blkName, "the host reads the device's ID %q as its serial %q", blkName, serial)
	r := s.fio("verify", dev, "--rw=randwrite", "--bs=4k", "--iodepth=16", "--fsync=64")
	s.Check(r.OK(), "fio writes and verifies the whole disk with flushes")
	s.healthy(g)
}

func blkTestFS(s *blkSuite) {
	g := s.start("")
	if g == nil {
		return
	}
	dev := s.attach()
	if dev == "" {
		return
	}
	mnt := filepath.Join(s.dir, "mnt")
	os.MkdirAll(mnt, 0755)
	// The unmount runs after a signal too, while the guest still serves the
	// disk.
	defer harness.RunTimeout(time.Minute, "umount", mnt)
	if !s.Check(s.run("mkfs.ext4", "-F", "-q", dev).OK(), "mkfs.ext4 makes a file system") ||
		!s.Check(s.run("mount", dev, mnt).OK(), "the file system mounts") {
		return
	}
	copied := s.run("cp", s.data, filepath.Join(mnt, "data")).OK() && s.run("sync").OK()
	s.Check(copied, "a file is copied to it")
	s.run("umount", mnt)
	os.WriteFile("/proc/sys/vm/drop_caches", []byte("3"), 0)
	if !s.Check(s.run("mount", dev, mnt).OK(), "the file system mounts again") {
		return
	}
	got, err := os.ReadFile(filepath.Join(mnt, "data"))
	s.Check(err == nil && bytes.Equal(got, s.ref), "the file matches after the page cache is dropped")
	harness.Run("umount", mnt)
	s.healthy(g)
}

func blkTestRebind(s *blkSuite) {
	g := s.start("")
	if g == nil {
		return
	}
	dev := s.attach()
	if dev == "" || !s.writeData(dev) {
		return
	}
	v := s.virtioDev()
	unbound := harness.Write("/sys/bus/virtio/drivers/virtio_blk/unbind", v) == nil
	bound := unbound && harness.Write("/sys/bus/virtio/drivers/virtio_blk/bind", v) == nil
	if !s.Check(bound, "virtio_blk unbinds and binds %s again", v) {
		return
	}
	if dev = s.waitDisk(); dev == "" || !s.checkData(dev, "virtio_blk binds again") {
		return
	}
	if !s.detach() {
		return
	}
	if dev = s.attach(); dev != "" {
		s.checkData(dev, "vdpa dev del and add")
	}
	s.healthy(g)
}

func blkTestMultiqueue(s *blkSuite) {
	const queues = 4
	g := s.start(fmt.Sprintf(",queues=%d", queues), fmt.Sprintf("ncpus=%d", queues))
	if g == nil {
		return
	}
	dev := s.attach()
	if dev == "" {
		return
	}
	mq := harness.Glob(filepath.Join("/sys/block", filepath.Base(dev), "mq/*"))
	if !s.Check(len(mq) == queues, "the host's disk has %d queues, %d", queues, len(mq)) {
		return
	}
	// One job runs on a CPU of each queue, so that every queue gets
	// requests, and each verifies a range of its own.
	var jobs []string
	for i, q := range mq {
		cpus := harness.Read(filepath.Join("/sys/block", filepath.Base(dev), "mq", q, "cpu_list"))
		cpu := strings.FieldsFunc(cpus, func(r rune) bool { return r == ',' || r == '-' })
		if !s.Check(len(cpu) > 0, "queue %s has host CPUs", q) {
			return
		}
		jobs = append(jobs, fmt.Sprintf("--name=q%s", q), "--cpus_allowed="+cpu[0],
			fmt.Sprintf("--offset=%d", i*blkDiskSize/queues), fmt.Sprintf("--size=%d", blkDiskSize/queues))
	}
	r := s.fioJobs("multiqueue", dev, []string{"--rw=randwrite", "--bs=4k", "--iodepth=16"}, jobs)
	s.Check(r.OK(), "fio writes and verifies through every queue")
	c := g.Counters()
	for i := 0; i < queues; i++ {
		n := c[fmt.Sprintf("q%d_requests", i)]
		s.Check(n > 0, "queue %d serves requests, %d", i, n)
	}
	s.healthy(g)
}

func blkTestZeroCopy(s *blkSuite) {
	g := s.start(",bounce=guest")
	if g == nil {
		return
	}
	dev := s.attach()
	if dev == "" {
		return
	}
	r := s.fio("verify", dev, "--rw=randwrite", "--bs=4k", "--iodepth=16")
	s.Check(r.OK(), "fio writes and verifies the whole disk")
	text := g.Output("cat /proc/vduse_guest", 10*time.Second)
	region := ""
	for _, line := range strings.Split(text, "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "bounce_region" {
			region = f[1]
		}
	}
	s.Check(region == "guest", "guest pages back the bounce region, %q", region)
	regs := harness.ParseCounters(text)["umem_regs"]
	s.Check(regs > 0, "the guest registered its pages with VDUSE, %d times", regs)
	s.healthy(g)
}

func blkTestStop(s *blkSuite) {
	g := s.start("")
	if g == nil || s.attach() == "" || !s.detach() {
		return
	}
	g.Signal(syscall.SIGTERM)
	s.Check(g.Wait(30*time.Second), "the guest stops on SIGTERM")
	s.logs(g, blkName+": destroyed the device")
	s.Check(!harness.Exists("/dev/vduse/"+blkName), "no VDUSE device is left")
	s.Check(!harness.Exists(s.log), "no in-flight log is left")
}

// delayDisk puts a dm-delay device in front of the disk's file, which delays
// each write by the given milliseconds, so that the guest holds requests
// long enough for them to be in flight when it dies. It is removed once the
// test's guests are gone.
func (s *blkSuite) delayDisk(ms int) bool {
	if err := harness.Modprobe("dm_delay"); !s.Check(err == nil, "dm-delay loads%s", errSuffix(err)) {
		return false
	}
	r := harness.Run("losetup", "-f", "--show", s.disk)
	if !s.Check(r.OK(), "a loop device holds the disk's file: %s", strings.TrimSpace(r.Output())) {
		return false
	}
	loop := strings.TrimSpace(r.Stdout)
	name := blkName + "-delay"
	table := fmt.Sprintf("0 %d delay %s 0 0 %s 0 %d", blkDiskSize/512, loop, loop, ms)
	if err := harness.MustRun("dmsetup", "create", name, "--table", table); err != nil {
		harness.Run("losetup", "-d", loop)
		s.Check(false, "dm-delay delays the disk's writes%s", errSuffix(err))
		return false
	}
	s.after = append(s.after, func() {
		harness.Run("dmsetup", "remove", name)
		harness.Run("losetup", "-d", loop)
	})
	s.ubd = "/dev/mapper/" + name
	return true
}

// blkReplays returns how many requests a guest's console says it replays.
func blkReplays(log string) int {
	n := 0
	for _, line := range strings.Split(log, "\n") {
		var q, m int
		if i := strings.Index(line, blkName+": queue "); i >= 0 {
			if _, err := fmt.Sscanf(line[i:], blkName+": queue %d has %d requests to replay", &q, &m); err == nil {
				n += m
			}
		}
	}
	return n
}

func blkTestKill(s *blkSuite) {
	if !s.delayDisk(10) {
		return
	}
	g := s.start("")
	if g == nil {
		return
	}
	dev := s.attach()
	if dev == "" {
		return
	}
	// fio writes and verifies across the kill and the relaunch, and with
	// each write delayed, the guest holds requests when it dies.
	done := make(chan harness.Result, 1)
	go func() {
		done <- s.fio("kill", dev, "--rw=randwrite", "--bs=4k", "--size=8M", "--iodepth=16")
	}()
	time.Sleep(3 * time.Second)
	g.Signal(syscall.SIGKILL)
	g.Wait(10 * time.Second)
	g = s.start("")
	if g == nil {
		// fio's requests wait for a guest, so the device is removed with a
		// short timeout to end them.
		harness.Detach(blkName, true)
		<-done
		return
	}
	s.logs(g, blkName+": reconnecting to the blk device")
	r := <-done
	s.Check(r.OK(), "fio finishes and verifies across kill -9 and a relaunch")
	n := blkReplays(g.Log())
	s.Check(n > 0, "the relaunched guest replays the requests that the killed one held, %d", n)
	s.healthy(g)
}

func blkTestTerm(s *blkSuite) {
	g := s.start("")
	if g == nil {
		return
	}
	dev := s.attach()
	if dev == "" || !s.writeData(dev) {
		return
	}
	g.Signal(syscall.SIGTERM)
	s.Check(g.Wait(30*time.Second), "the guest stops on SIGTERM")
	s.logs(g, "the device stays on the host's vDPA bus for the next guest")
	g = s.start("")
	if g == nil {
		return
	}
	s.logs(g, blkName+": reconnecting to the blk device")
	s.checkData(dev, "SIGTERM and a relaunch")
	s.healthy(g)
}

func blkTestUnused(s *blkSuite) {
	if !s.writeDiskFile() {
		return
	}
	g := s.start("")
	if g == nil {
		return
	}
	g.Signal(syscall.SIGKILL)
	g.Wait(10 * time.Second)
	g = s.start("")
	if g == nil {
		return
	}
	s.logs(g, blkName+": created blk device")
	if dev := s.attach(); dev != "" {
		s.checkData(dev, "a relaunch before the host attached")
	}
	s.healthy(g)
}

func blkTestTimeout(s *blkSuite) {
	g := s.start("")
	if g == nil || s.attach() == "" {
		return
	}
	// With no guest to answer it, the reset of an unbind times out, and
	// VDUSE marks the device broken.
	harness.Write(filepath.Join("/sys/class/vduse", blkName, "msg_timeout"), "2")
	g.Signal(syscall.SIGKILL)
	g.Wait(10 * time.Second)
	harness.Write("/sys/bus/virtio/drivers/virtio_blk/unbind", s.virtioDev())
	g = s.start("")
	if g == nil {
		return
	}
	s.logs(g, blkName+": the device stopped working after a message timed out")
	g.Signal(syscall.SIGKILL)
	g.Wait(10 * time.Second)
	s.Check(harness.MustRun("vdpa", "dev", "del", blkName) == nil, "vdpa dev del removes the broken device")
}

func blkTestSandbox(s *blkSuite) {
	logs, err := os.MkdirTemp("/dev/shm", "vblk-test-")
	if !s.Check(err == nil, "a directory for the in-flight log is made on a tmpfs%s", errSuffix(err)) {
		return
	}
	defer os.RemoveAll(logs)
	launcher := filepath.Join(harness.RepoRoot(), "build/invirtio-launch")
	argv := []string{
		launcher, "-u", "nobody", "-t", "20",
		"-i", filepath.Join(s.sandboxDir, "initramfs/initramfs.cpio"),
		"-e", "blk," + blkName + ",log=" + filepath.Join(logs, "inflight"),
		filepath.Join(s.sandboxDir, "linux"), "mem=512M", "brd.rd_nr=1",
		"brd.rd_size=" + strconv.Itoa(blkDiskSize>>10),
		"vduse_guest.device=blk," + blkName + ",disk=/dev/ram0",
	}
	g := s.boot(argv)
	if g == nil {
		return
	}
	dev := s.attach()
	if dev == "" {
		return
	}
	r := s.fio("verify", dev, "--rw=randwrite", "--bs=4k", "--iodepth=16", "--fsync=64")
	s.Check(r.OK(), "fio writes and verifies the whole disk")
	s.healthy(g)
	s.Check(strings.Contains(g.Log(), "Installed the sandbox filter") &&
		!strings.Contains(g.Log(), "The sandbox denied"), "the guest runs under its seccomp filter, which denies nothing")
	if !s.detach() {
		return
	}
	g.Signal(syscall.SIGTERM)
	s.Check(g.Wait(30*time.Second) && g.ExitCode() == 0, "the launcher stops the guest in order")
	s.Check(!harness.Exists("/dev/vduse/"+blkName), "no VDUSE device is left")
}
