package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"invirtio/internal/harness"
)

func init() {
	register("test-gpu", "test the VDUSE virtio-gpu backend against the host's virtio_gpu", cmdTestGpu)
}

const (
	gpuName = "vgpu-test"
	// The DRM capset, VIRTIO_GPU_CAPSET_DRM, and its size.
	gpuCapsetDRM  = 6
	gpuCapsetSize = 592
	// Virglrenderer's context type for amdgpu.
	gpuContextAmdgpu = 2
	// The size of Mesa's shared memory buffer, blob 0, and the offset of
	// its responses, which the guest writes into the buffer's header.
	gpuShmemSize      = 0x4000
	gpuShmemRspOffset = 88
)

// The virtio_gpu ioctls, DRM_IOWR(DRM_COMMAND_BASE + nr, size).
func drmIOWR(nr, size uintptr) uintptr {
	return 3<<30 | size<<16 | 'd'<<8 | (0x40 + nr)
}

var (
	virtgpuMap                = drmIOWR(0x01, 16)
	virtgpuGetparam           = drmIOWR(0x03, 16)
	virtgpuResourceCreate     = drmIOWR(0x04, 56)
	virtgpuGetCaps            = drmIOWR(0x09, 24)
	virtgpuResourceCreateBlob = drmIOWR(0x0a, 48)
	virtgpuContextInit        = drmIOWR(0x0b, 16)
	// DRM_IOCTL_PRIME_HANDLE_TO_FD, DRM_IOWR(0x2d, struct drm_prime_handle).
	drmPrimeHandleToFD uintptr = 3<<30 | 12<<16 | 'd'<<8 | 0x2d
)

// The blob arguments of DRM_IOCTL_VIRTGPU_RESOURCE_CREATE_BLOB.
const (
	virtgpuBlobMemHost3D    = 2
	virtgpuBlobFlagMappable = 1
)

// IORING_REGISTER_BUFFERS, and the size of struct io_uring_params.
const (
	ioringRegisterBuffers = 0
	ioringParamsSize      = 120
)

// The parameters of DRM_IOCTL_VIRTGPU_GETPARAM.
const (
	virtgpuParam3D           = 1
	virtgpuParamResourceBlob = 3
	virtgpuParamHostVisible  = 4
	virtgpuParamContextInit  = 6
	virtgpuParamCapsetIDs    = 7
)

// The parameters of DRM_IOCTL_VIRTGPU_CONTEXT_INIT.
const (
	virtgpuContextCapsetID = 1
	virtgpuContextNumRings = 2
)

// gpuGuestCounters are the backend's error counters, which stay at zero
// in the tests that send no commands that the device refuses.
var gpuGuestCounters = []string{"invalid_commands"}

type gpuTest struct {
	name  string
	about string
	run   func(*gpuSuite)
}

var gpuTests = []gpuTest{
	{"probe", "virtio_gpu probes a device with virgl, blobs, context init and the DRM capset, and no scanouts", gpuTestProbe},
	{"contexts", "host processes create and destroy DRM contexts, and the device serves new ones after a reset", gpuTestContexts},
	{"virgl", "the device refuses virgl resources and contexts, and keeps serving", gpuTestVirgl},
	{"map", "the shared memory buffer maps through the DRM fd and a dma-buf, and the guest's exit revokes both", gpuTestMap},
	{"mesa", "RADV from scripts/mesa-build.sh finds the render node", gpuTestMesa},
}

type gpuSuite struct {
	harness.Checker
	build string
	out   string
	// device is the iGPU's PCI address, with its ROM, or "" for a guest
	// without a GPU.
	device string
	// mesa is the prefix of the Mesa that scripts/mesa-build.sh built.
	mesa string
	// The test that runs, its directory, the directory of its in-flight
	// log, and the guests it started, which a second signal kills. Ctx ends
	// on a signal.
	dir    string
	logDir string
	mu     sync.Mutex
	guests []*harness.Guest
	ctx    context.Context
	// stuck is set when a file of the device stays open, after an ioctl
	// that did not return or in another process. Removing virtio_gpu while
	// a file is open breaks the host, so the device stays attached and no
	// other test runs.
	stuck bool
}

func cmdTestGpu(args []string) error {
	root := harness.RepoRoot()
	fs := newFlags("test-gpu", `
usage: invirtio-test test-gpu [options]

Test the VDUSE virtio-gpu backend against the host's virtio_gpu. Each test
starts its own guest under the launcher and leaves no device behind. If a
process keeps a file of the device open, the run stops and leaves the
device attached, since removing virtio_gpu would break the host. Build the
guest with scripts/invirtio-build-guest.sh -t amdgpu. Without -device, the
guest has no GPU, and the device reports a DRM capset of zeros. With
-device, the guest drives the iGPU, and the capset describes it. The host
must then not use the iGPU, so stop the display manager first. Every test
checks that the guest's error counters stay at zero, unless it sends
commands that the device refuses, and that neither kernel logs a warning.
The guests' consoles go to <test>/guest-<n>.log in the output directory.
Run as root on the linux-host kernel.

Tests:`+gpuTestList())
	build := fs.String("guest", filepath.Join(root, "linux-uml/build-amdgpu"), "the build directory of the guest")
	out := fs.String("out", filepath.Join(root, "build/test-gpu"), "the output directory")
	device := fs.String("device", "", "the PCI address of the iGPU to pass through, such as 0000:01:00.0")
	rom := fs.String("rom", "", "the VBIOS image of the iGPU")
	mesa := fs.String("mesa", filepath.Join(root, "build/mesa"), "the prefix of the Mesa that scripts/mesa-build.sh built")
	runRE := fs.String("run", "", "run only the tests whose names match this regular expression")
	fs.Parse(args)
	if fs.NArg() != 0 || (*rom != "") != (*device != "") {
		fs.Usage()
		os.Exit(2)
	}
	match, err := regexp.Compile(*runRE)
	if err != nil {
		return err
	}
	requireRoot()

	s := &gpuSuite{build: *build, mesa: *mesa}
	if *device != "" {
		r, err := filepath.Abs(*rom)
		if err != nil {
			return err
		}
		s.device = *device + ",rom=" + r
	}
	if s.out, err = filepath.Abs(*out); err != nil {
		return err
	}
	if err := s.setup(); err != nil {
		return err
	}
	defer os.RemoveAll(s.logDir)

	// A signal fails the test that runs, and no other test runs. A second
	// signal kills the guests and exits.
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
		// The test's files can still be open, and removing virtio_gpu then
		// breaks the host, so the device stays on the vDPA bus.
		s.mu.Lock()
		for _, g := range s.guests {
			g.Kill()
		}
		s.mu.Unlock()
		fmt.Printf("the device %s stays attached, remove it with vdpa dev del once no process uses it\n", gpuName)
		os.RemoveAll(s.logDir)
		harness.CloseOutput()
		os.Exit(1)
	}()

	mark := harness.KernelMark()
	for _, t := range gpuTests {
		if !match.MatchString(t.name) || ctx.Err() != nil {
			continue
		}
		fmt.Printf("== %s: %s\n", t.name, t.about)
		s.runTest(t)
		if s.stuck {
			// The device stays attached, and the guests are no longer needed.
			for _, g := range s.guests {
				g.Kill()
			}
			return fmt.Errorf("a file of %s is still open, so the device stays attached without its guest, remove it with vdpa dev del once no process uses it, %d check(s) failed",
				gpuName, len(s.Failures()))
		}
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

func gpuTestList() string {
	var b strings.Builder
	for _, t := range gpuTests {
		fmt.Fprintf(&b, "\n  %-9s %s", t.name, t.about)
	}
	return b.String()
}

func (s *gpuSuite) setup() error {
	if harness.Run("sh", "-c", "command -v vdpa").Code != 0 {
		return fmt.Errorf("vdpa is not installed")
	}
	if err := harness.Modprobe("vduse", "vdpa", "virtio_vdpa", "virtio_gpu"); err != nil {
		return err
	}
	if err := os.MkdirAll(s.out, 0755); err != nil {
		return err
	}
	if err := harness.TeeOutput(filepath.Join(s.out, "test.log")); err != nil {
		return err
	}
	var err error
	s.logDir, err = harness.MakeLogDir(gpuName)
	return err
}

// runTest runs a test from a clean host, and afterwards removes what its
// guests left and checks their logs for kernel warnings.
func (s *gpuSuite) runTest(t gpuTest) {
	if !s.released() {
		return
	}
	harness.Cleanup(gpuName)
	os.Remove(filepath.Join(s.logDir, "inflight"))
	s.dir = filepath.Join(s.out, t.name)
	os.RemoveAll(s.dir)
	os.MkdirAll(s.dir, 0755)
	s.mu.Lock()
	s.guests = nil
	s.mu.Unlock()

	t.run(s)
	if s.stuck || !s.released() {
		return
	}

	harness.Detach(gpuName, s.ctx.Err() != nil)
	for _, g := range s.guests {
		g.Stop(30 * time.Second)
		g.Kill()
	}
	harness.Cleanup(gpuName)
	var warnings []string
	for _, g := range s.guests {
		warnings = append(warnings, harness.Warnings(strings.Split(g.Log(), "\n"))...)
	}
	for _, w := range warnings {
		fmt.Println("warning: " + w)
	}
	s.Check(len(warnings) == 0, "%s: no guest logs a kernel warning", t.name)
}

// start starts a guest under the launcher and waits until it has created
// the device and its shell answers.
func (s *gpuSuite) start() *harness.Guest {
	n := len(s.guests) + 1
	if s.ctx.Err() != nil {
		return nil
	}
	l := harness.Launch{
		Build:  s.build,
		Type:   "gpu",
		Name:   gpuName,
		LogDir: s.logDir,
		Args:   []string{"mem=2G", "init_shell"},
	}
	if s.device != "" {
		l.Devices = []string{s.device}
	}
	g, err := harness.LaunchGuest(filepath.Join(s.dir, fmt.Sprintf("guest-%d.log", n)), l)
	if !s.Check(err == nil, "guest %d starts%s", n, errSuffix(err)) {
		return nil
	}
	s.mu.Lock()
	s.guests = append(s.guests, g)
	s.mu.Unlock()
	ok := g.WaitFor(gpuName+": created gpu device", 2*time.Minute) &&
		strings.Contains(g.Output("echo ready", time.Minute), "ready")
	if !s.Check(ok && g.Running(), "guest %d creates the device and starts its shell", n) {
		return nil
	}
	return g
}

// attach adds the device to the vDPA bus and returns the host's render
// node for it.
func (s *gpuSuite) attach() string {
	err := harness.Attach(gpuName)
	if !s.Check(err == nil, "vdpa dev add attaches the device%s", errSuffix(err)) {
		return ""
	}
	// virtio_gpu registers its DRM device on the virtio device's parent.
	pattern := filepath.Join("/sys/bus/vdpa/devices", gpuName, "drm/renderD*")
	var node string
	ok := harness.Wait(func() bool {
		nodes := harness.Glob(pattern)
		if len(nodes) == 0 {
			return false
		}
		node = "/dev/dri/" + filepath.Base(nodes[0])
		return harness.Exists(node)
	}, 30*time.Second, 200*time.Millisecond)
	if !s.Check(ok, "virtio_gpu registers a render node") {
		return ""
	}
	return node
}

// drmHolders returns the processes that hold one of the device's DRM nodes
// open, found by the nodes' device numbers.
func drmHolders() []string {
	nodes := map[uint64]bool{}
	for _, n := range harness.Glob(filepath.Join("/sys/bus/vdpa/devices", gpuName, "drm/*")) {
		var major, minor uint32
		dev := harness.Read(filepath.Join("/sys/bus/vdpa/devices", gpuName, "drm", n, "dev"))
		if _, err := fmt.Sscanf(dev, "%d:%d", &major, &minor); err == nil {
			nodes[unix.Mkdev(major, minor)] = true
		}
	}
	if len(nodes) == 0 {
		return nil
	}
	var pids []string
	fds, _ := filepath.Glob("/proc/[0-9]*/fd/*")
	for _, fd := range fds {
		var st unix.Stat_t
		if unix.Stat(fd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFCHR || !nodes[st.Rdev] {
			continue
		}
		pid := strings.Split(fd, "/")[2]
		if len(pids) == 0 || pids[len(pids)-1] != pid {
			pids = append(pids, pid)
		}
	}
	return pids
}

// released waits until no process holds the device's DRM nodes, which a
// desktop can open when the device appears. If one keeps them open, the run
// is stuck, since removing virtio_gpu then breaks the host.
func (s *gpuSuite) released() bool {
	var pids []string
	ok := harness.Wait(func() bool {
		pids = drmHolders()
		return len(pids) == 0
	}, 30*time.Second, 200*time.Millisecond)
	if !ok {
		s.Check(false, "no process holds the device's DRM nodes%s", listSuffix("held by", pids))
		s.stuck = true
	}
	return ok
}

// virtioDev returns the device's virtio device on the virtio bus.
func (s *gpuSuite) virtioDev() string {
	devs := harness.Glob(filepath.Join("/sys/bus/vdpa/devices", gpuName, "virtio*"))
	if len(devs) == 0 {
		return ""
	}
	return devs[0]
}

// counters checks that the guest's error counters are at zero.
func (s *gpuSuite) counters(g *harness.Guest) {
	c := g.Counters()
	if !s.Check(len(c) > 0, "the guest reports its counters") {
		return
	}
	for _, name := range gpuGuestCounters {
		v, ok := c[name]
		if s.Check(ok, "the guest reports its %s", name) {
			s.Check(v == 0, "the guest's %s stays at zero, not %d", name, v)
		}
	}
}

// contexts returns the guest's number of contexts, or -1 if it does not
// report it.
func (s *gpuSuite) contexts(g *harness.Guest) int64 {
	if v, ok := g.Counters()["contexts"]; ok {
		return v
	}
	return -1
}

// ioctl runs an ioctl whose argument holds the addresses of the objects in
// pinned, which stay in place until it returns.
func ioctl(fd int, req uintptr, arg unsafe.Pointer, pinned ...any) error {
	var p runtime.Pinner
	defer p.Unpin()
	for _, o := range pinned {
		p.Pin(o)
	}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), req, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}

func openRender(node string) (int, error) {
	return unix.Open(node, unix.O_RDWR|unix.O_CLOEXEC, 0)
}

func getparam(fd int, param uint64) (uint64, error) {
	value := new(uint64)
	arg := struct{ param, value uint64 }{param, uint64(uintptr(unsafe.Pointer(value)))}
	err := ioctl(fd, virtgpuGetparam, unsafe.Pointer(&arg), value)
	return *value, err
}

func getCaps(fd int, id uint32, size uint32) ([]byte, error) {
	buf := make([]byte, size)
	arg := struct {
		id, version uint32
		addr        uint64
		size, pad   uint32
	}{id, 0, uint64(uintptr(unsafe.Pointer(&buf[0]))), size, 0}
	err := ioctl(fd, virtgpuGetCaps, unsafe.Pointer(&arg), &buf[0])
	return buf, err
}

// contextInit creates the file's context with a capset and Mesa's number
// of rings.
func contextInit(fd int, capset uint64) error {
	params := &[2]struct{ param, value uint64 }{
		{virtgpuContextCapsetID, capset}, {virtgpuContextNumRings, 64},
	}
	arg := struct {
		num, pad uint32
		params   uint64
	}{2, 0, uint64(uintptr(unsafe.Pointer(&params[0])))}
	return ioctl(fd, virtgpuContextInit, unsafe.Pointer(&arg), params)
}

func gpuTestProbe(s *gpuSuite) {
	mark := harness.KernelMark()
	g := s.start()
	if g == nil {
		return
	}
	node := s.attach()
	if node == "" {
		return
	}
	log := strings.Join(harness.KernelLogSince(mark), "\n")
	for _, line := range []string{
		"features: +virgl -edid +resource_blob +host_visible",
		"features: +context_init -blob_alignment",
		"KMS disabled",
		"number of cap sets: 1",
		fmt.Sprintf("cap set 0: id %d, max-version 0, max-size %d", gpuCapsetDRM, gpuCapsetSize),
	} {
		s.Check(strings.Contains(log, line), "virtio_gpu logs %q", line)
	}

	fd, err := openRender(node)
	if !s.Check(err == nil, "the render node opens%s", errSuffix(err)) {
		return
	}
	defer unix.Close(fd)
	for _, p := range []struct {
		name        string
		param, want uint64
	}{
		{"3D_FEATURES", virtgpuParam3D, 1},
		{"RESOURCE_BLOB", virtgpuParamResourceBlob, 1},
		{"HOST_VISIBLE", virtgpuParamHostVisible, 1},
		{"CONTEXT_INIT", virtgpuParamContextInit, 1},
		{"SUPPORTED_CAPSET_IDs", virtgpuParamCapsetIDs, 1 << gpuCapsetDRM},
	} {
		v, err := getparam(fd, p.param)
		s.Check(err == nil && v == p.want, "VIRTGPU_PARAM_%s is %d, not %d%s", p.name, p.want, v, errSuffix(err))
	}

	caps, err := getCaps(fd, gpuCapsetDRM, gpuCapsetSize)
	if !s.Check(err == nil, "the DRM capset is read%s", errSuffix(err)) {
		return
	}
	le := binary.LittleEndian
	if s.device == "" {
		s.Check(bytes.Count(caps, []byte{0}) == len(caps), "without a GPU, the capset is zeros")
	} else {
		name, _, _ := bytes.Cut(caps[464:], []byte{0})
		s.Check(le.Uint32(caps[16:]) == gpuContextAmdgpu, "the capset's context type is amdgpu, not %d", le.Uint32(caps[16:]))
		s.Check(le.Uint32(caps[4:]) == 3, "the capset's DRM version is 3, not %d", le.Uint32(caps[4:]))
		s.Check(len(name) > 0, "the capset names the GPU %q", name)
		fmt.Printf("capset: DRM %d.%d.%d, address32_hi 0x%x, family %d, asic 0x%x, %q\n",
			le.Uint32(caps[4:]), le.Uint32(caps[8:]), le.Uint32(caps[12:]),
			le.Uint32(caps[24:]), le.Uint32(caps[24+24+12:]), le.Uint32(caps[24+24:]), name)
	}
	s.counters(g)
}

func gpuTestContexts(s *gpuSuite) {
	g := s.start()
	if g == nil {
		return
	}
	node := s.attach()
	if node == "" {
		return
	}
	var fds []int
	defer func() {
		for _, fd := range fds {
			unix.Close(fd)
		}
	}()
	for i := 0; i < 2; i++ {
		fd, err := openRender(node)
		if !s.Check(err == nil, "the render node opens%s", errSuffix(err)) {
			return
		}
		fds = append(fds, fd)
		err = contextInit(fd, gpuCapsetDRM)
		s.Check(err == nil, "process %d creates a DRM context%s", i+1, errSuffix(err))
	}
	ok := harness.Wait(func() bool { return g.Counters()["contexts"] == 2 }, 10*time.Second, 200*time.Millisecond)
	s.Check(ok, "the guest has 2 contexts")
	s.counters(g)

	unix.Close(fds[0])
	fds = fds[1:]
	ok = harness.Wait(func() bool { return g.Counters()["contexts"] == 1 }, 10*time.Second, 200*time.Millisecond)
	s.Check(ok, "closing one file destroys its context in the guest")

	unix.Close(fds[0])
	fds = nil
	ok = harness.Wait(func() bool { return s.contexts(g) == 0 }, 10*time.Second, 200*time.Millisecond)
	s.Check(ok, "closing the other file destroys its context in the guest")

	// Removing virtio_gpu while a file is open breaks the host, so the
	// device is reset with no context left.
	before, ok := g.Counters()["resets"]
	if !s.Check(ok, "the guest reports its resets") {
		return
	}
	if !s.released() {
		return
	}
	dev := s.virtioDev()
	err := harness.Write("/sys/bus/virtio/drivers/virtio_gpu/unbind", filepath.Base(dev))
	if !s.Check(err == nil, "virtio_gpu unbinds from the device%s", errSuffix(err)) {
		return
	}
	ok = harness.Wait(func() bool { return g.Counters()["resets"] > before }, 10*time.Second, 200*time.Millisecond)
	s.Check(ok, "unbinding resets the device in the guest")
	err = harness.Write("/sys/bus/virtio/drivers/virtio_gpu/bind", filepath.Base(dev))
	if !s.Check(err == nil, "virtio_gpu binds to the device again%s", errSuffix(err)) {
		return
	}
	node = ""
	ok = harness.Wait(func() bool {
		nodes := harness.Glob(filepath.Join("/sys/bus/vdpa/devices", gpuName, "drm/renderD*"))
		if len(nodes) > 0 {
			node = "/dev/dri/" + filepath.Base(nodes[0])
		}
		return node != "" && harness.Exists(node)
	}, 30*time.Second, 200*time.Millisecond)
	if !s.Check(ok, "virtio_gpu registers a render node again") {
		return
	}
	fd, err := openRender(node)
	if !s.Check(err == nil, "the new render node opens%s", errSuffix(err)) {
		return
	}
	err = contextInit(fd, gpuCapsetDRM)
	s.Check(err == nil, "a process creates a DRM context after the reset%s", errSuffix(err))
	ok = harness.Wait(func() bool { return g.Counters()["contexts"] == 1 }, 10*time.Second, 200*time.Millisecond)
	s.Check(ok, "the guest has the new context")
	unix.Close(fd)
	ok = harness.Wait(func() bool { return s.contexts(g) == 0 }, 10*time.Second, 200*time.Millisecond)
	s.Check(ok, "closing the file destroys its context in the guest")
	s.counters(g)
}

func gpuTestVirgl(s *gpuSuite) {
	g := s.start()
	if g == nil {
		return
	}
	node := s.attach()
	if node == "" {
		return
	}
	fd, err := openRender(node)
	if !s.Check(err == nil, "the render node opens%s", errSuffix(err)) {
		return
	}
	// A 3D resource makes virtio_gpu create a virgl context for the file.
	rc := struct {
		target, format, bind, width, height, depth, arraySize, lastLevel uint32
		nrSamples, flags, boHandle, resHandle, size, stride              uint32
	}{target: 2, format: 1, width: 64, height: 64, depth: 1, arraySize: 1}
	done := make(chan error, 1)
	go func() { done <- ioctl(fd, virtgpuResourceCreate, unsafe.Pointer(&rc)) }()
	select {
	case <-done:
		s.Check(true, "creating a virgl resource returns")
	case <-time.After(30 * time.Second):
		s.Check(false, "creating a virgl resource returns")
		s.stuck = true
		return
	}
	unix.Close(fd)

	ok := harness.Wait(func() bool { return g.Counters()["invalid_commands"] > 0 }, 10*time.Second, 200*time.Millisecond)
	s.Check(ok, "the device refuses the virgl commands")

	fd, err = openRender(node)
	if !s.Check(err == nil, "the render node opens%s", errSuffix(err)) {
		return
	}
	defer unix.Close(fd)
	err = contextInit(fd, gpuCapsetDRM)
	s.Check(err == nil, "a process creates a DRM context afterwards%s", errSuffix(err))
	ok = harness.Wait(func() bool { return g.Counters()["contexts"] == 1 }, 10*time.Second, 200*time.Millisecond)
	s.Check(ok, "the guest has the new context")
}

// createShmem creates the context's shared memory buffer, blob 0, and
// returns its GEM handle.
func createShmem(fd int) (uint32, error) {
	arg := struct {
		blobMem, blobFlags, boHandle, resHandle uint32
		size                                    uint64
		pad, cmdSize                            uint32
		cmd, blobID                             uint64
	}{blobMem: virtgpuBlobMemHost3D, blobFlags: virtgpuBlobFlagMappable, size: gpuShmemSize}
	err := ioctl(fd, virtgpuResourceCreateBlob, unsafe.Pointer(&arg))
	return arg.boHandle, err
}

// mapOffset returns the fake offset at which a GEM object is mapped.
func mapOffset(fd int, handle uint32) (int64, error) {
	arg := struct {
		offset      uint64
		handle, pad uint32
	}{handle: handle}
	err := ioctl(fd, virtgpuMap, unsafe.Pointer(&arg))
	return int64(arg.offset), err
}

func primeHandleToFD(fd int, handle uint32) (int, error) {
	arg := struct {
		handle, flags uint32
		fd            int32
	}{handle: handle, flags: unix.O_CLOEXEC | unix.O_RDWR}
	err := ioctl(fd, drmPrimeHandleToFD, unsafe.Pointer(&arg))
	return int(arg.fd), err
}

// readFaults reads the first word of a mapping and reports whether the read
// faulted.
func readFaults(b []byte) (faulted bool) {
	done := make(chan bool)
	go func() {
		debug.SetPanicOnFault(true)
		defer func() { done <- recover() != nil }()
		// An atomic load always reads the page, which a plain load whose
		// value is unused need not.
		atomic.LoadUint32((*uint32)(unsafe.Pointer(&b[0])))
	}()
	return <-done
}

// registerBuffer registers a mapping as an io_uring buffer and returns the
// error, or nil if io_uring is not available.
func registerBuffer(b []byte) (error, bool) {
	var params [ioringParamsSize]byte
	ring, _, errno := unix.Syscall(unix.SYS_IO_URING_SETUP, 1, uintptr(unsafe.Pointer(&params[0])), 0)
	if errno != 0 {
		return nil, false
	}
	defer unix.Close(int(ring))
	iov := unix.Iovec{Base: &b[0]}
	iov.SetLen(len(b))
	_, _, errno = unix.Syscall6(unix.SYS_IO_URING_REGISTER, ring, ioringRegisterBuffers,
		uintptr(unsafe.Pointer(&iov)), 1, 0, 0)
	if errno != 0 {
		return errno, true
	}
	return nil, true
}

func gpuTestMap(s *gpuSuite) {
	g := s.start()
	if g == nil {
		return
	}
	node := s.attach()
	if node == "" {
		return
	}
	// The files and mappings are released before the guest's device is
	// removed, and the deferred release skips those already gone.
	var drm, dmabuf []byte
	fd, buf := -1, -1
	defer func() {
		for _, m := range [][]byte{drm, dmabuf} {
			if m != nil {
				unix.Munmap(m)
			}
		}
		for _, f := range []int{buf, fd} {
			if f >= 0 {
				unix.Close(f)
			}
		}
	}()
	fd, err := openRender(node)
	if !s.Check(err == nil, "the render node opens%s", errSuffix(err)) {
		return
	}
	err = contextInit(fd, gpuCapsetDRM)
	if !s.Check(err == nil, "a process creates a DRM context%s", errSuffix(err)) {
		return
	}
	handle, err := createShmem(fd)
	if !s.Check(err == nil, "the process creates its shared memory buffer%s", errSuffix(err)) {
		return
	}
	offset, err := mapOffset(fd, handle)
	if !s.Check(err == nil, "the buffer gets a mapping offset%s", errSuffix(err)) {
		return
	}

	private, err := unix.Mmap(fd, offset, gpuShmemSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE)
	if err == nil {
		unix.Munmap(private)
	}
	s.Check(err == unix.EINVAL, "a private mapping of the buffer is refused%s", errSuffix(err))

	drm, err = unix.Mmap(fd, offset, gpuShmemSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if !s.Check(err == nil, "the buffer maps through the DRM fd%s", errSuffix(err)) {
		return
	}
	le := binary.LittleEndian
	s.Check(le.Uint32(drm[4:]) == gpuShmemRspOffset, "the guest's header gives the responses' offset %d, not %d",
		gpuShmemRspOffset, le.Uint32(drm[4:]))
	drm[gpuShmemSize-1] = 0x5a

	buf, err = primeHandleToFD(fd, handle)
	if !s.Check(err == nil, "the buffer is exported as a dma-buf%s", errSuffix(err)) {
		buf = -1
		return
	}
	dmabuf, err = unix.Mmap(buf, 0, gpuShmemSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if !s.Check(err == nil, "the dma-buf maps%s", errSuffix(err)) {
		return
	}
	s.Check(le.Uint32(dmabuf[4:]) == gpuShmemRspOffset && dmabuf[gpuShmemSize-1] == 0x5a,
		"the dma-buf mapping shows the guest's header and the host's write")

	if err, ok := registerBuffer(drm); ok {
		s.Check(err == unix.EFAULT, "registering the mapping as an io_uring buffer fails with EFAULT%s", errSuffix(err))
	} else {
		fmt.Println("io_uring is not available, so its check is skipped")
	}
	ok := harness.Wait(func() bool { return g.Counters()["resources"] == 1 }, 10*time.Second, 200*time.Millisecond)
	s.Check(ok, "the guest has the buffer's resource")
	s.counters(g)

	// The guest's exit closes its VDUSE fd, which revokes the region.
	g.Kill()
	ok = harness.Wait(func() bool { return readFaults(drm) && readFaults(dmabuf) }, 30*time.Second, 200*time.Millisecond)
	s.Check(ok, "after the guest exits, both mappings give SIGBUS")

	unix.Munmap(drm)
	unix.Munmap(dmabuf)
	drm, dmabuf = nil, nil
	unix.Close(buf)
	unix.Close(fd)
	buf, fd = -1, -1
	if !s.released() {
		return
	}
	err = harness.Detach(gpuName, true)
	s.Check(err == nil, "vdpa dev del removes the device of the dead guest%s", errSuffix(err))
}

// mesaEnv returns an environment that uses the Mesa prefix, as
// scripts/mesa-env.sh sets up.
func (s *gpuSuite) mesaEnv() []string {
	return append(os.Environ(),
		"LD_LIBRARY_PATH="+filepath.Join(s.mesa, "lib"),
		"VK_DRIVER_FILES="+filepath.Join(s.mesa, "share/vulkan/icd.d/radeon_icd.x86_64.json"),
		"__EGL_VENDOR_LIBRARY_FILENAMES="+filepath.Join(s.mesa, "share/glvnd/egl_vendor.d/50_mesa.json"),
		"GBM_BACKENDS_PATH="+filepath.Join(s.mesa, "lib/gbm"))
}

// gpuTestMesa checks that libdrm reports the render node, whose parent is a
// vDPA device, so that RADV considers it. Without a GPU the capset is
// zeros, and RADV stops there.
func gpuTestMesa(s *gpuSuite) {
	if !harness.Exists(filepath.Join(s.mesa, "lib/libvulkan_radeon.so")) {
		s.Check(false, "the Mesa prefix %s is built", s.mesa)
		return
	}
	g := s.start()
	if g == nil {
		return
	}
	node := s.attach()
	if node == "" {
		return
	}
	cmd := exec.CommandContext(s.ctx, "vulkaninfo", "--summary")
	cmd.Env = append(s.mesaEnv(), "RADV_DEBUG=startup")
	// Vulkaninfo fails without a GPU behind the capset, so only its
	// output counts.
	out, err := cmd.CombinedOutput()
	os.WriteFile(filepath.Join(s.dir, "vulkaninfo.log"), out, 0644)
	if len(out) == 0 && !s.Check(err == nil, "vulkaninfo runs%s", errSuffix(err)) {
		return
	}
	s.Check(strings.Contains(string(out), "radv: info: Found device '"+node+"'"),
		"RADV finds %s, see vulkaninfo.log", node)
	s.counters(g)
}
