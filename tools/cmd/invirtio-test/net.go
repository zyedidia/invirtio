package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"invirtio/internal/harness"
	"invirtio/internal/pci"
)

func init() {
	register("test-net", "test the VDUSE virtio-net backend", cmdTestNet)
}

const (
	netName = "vnet-test"
	netMTU  = 9000
	// The X710's ports, which are cabled to each other.
	netGuestPort = "0000:05:00.0"
	netPeerPort  = "0000:05:00.1"
	netPeerNS    = "vnet-peer"
	// The lowest rate that an iperf3 run passes with, in bits per second.
	netMinRate = 50e6
)

// netFeatures are the bits of /sys/class/net/<net>/device/features, which
// lists bit 0 first, that the host must negotiate.
var netFeatures = []struct {
	name string
	bit  int
}{
	{"CSUM", 0}, {"GUEST_CSUM", 1}, {"MTU", 3}, {"MAC", 5}, {"GUEST_TSO4", 7},
	{"GUEST_TSO6", 8}, {"GUEST_ECN", 9}, {"HOST_TSO4", 11}, {"HOST_TSO6", 12},
	{"HOST_ECN", 13}, {"MRG_RXBUF", 15}, {"STATUS", 16}, {"SPEED_DUPLEX", 63},
}

// netCounters are the guest's error counters, which stay at zero.
// rx_no_buffer and rx_dropped grow whenever the host is slow to refill its
// buffers, so they are left out.
var netCounters = []string{"tx_invalid", "tx_dropped", "rx_oversized", "rx_unsupported",
	"q0_bad_requests", "q1_bad_requests", "q0_inject_errors", "q1_inject_errors"}

// netErrors are the guest's messages that report a failure.
var netErrors = []string{"is broken", "dropping a bad request", "cannot ", "is gone"}

type netTest struct {
	name  string
	about string
	run   func(*netSuite)
}

var netTests = []netTest{
	{"device", "the host interface has the exported address, MTU, features and speed", netTestDevice},
	{"traffic", "pings and iperf3 pass both ways at MTU 9000 and 1500 with segmentation offloads", netTestTraffic},
	{"link", "the far end's link state reaches the host's carrier", netTestLink},
	{"reattach", "traffic passes after vdpa dev del and add", netTestReattach},
	{"kill", "a guest killed under traffic is relaunched and traffic resumes", netTestKill},
	{"stop", "a guest stopped after vdpa dev del destroys its device", netTestStop},
}

type netSuite struct {
	harness.Checker
	build string
	out   string
	x710  bool
	// The host's address on the device and the far end's.
	hostAddr string
	peerAddr string
	// peerNet is the far end's interface, in netPeerNS with the X710, and in
	// the guest otherwise.
	peerNet string
	netdev  string
	far     *netFarEnd
	// portDriver is port 0's driver before the run, which it restores.
	portDriver string
	// The test that runs, its directory, the directory of its in-flight
	// log, and the guests it started, which a second signal kills.
	dir    string
	logDir string
	mu     sync.Mutex
	guests []*harness.Guest
	// ctx ends on a signal, which stops the commands that send traffic.
	ctx context.Context
}

func cmdTestNet(args []string) error {
	root := harness.RepoRoot()
	fs := newFlags("test-net", `
usage: invirtio-test test-net [options]

Test the VDUSE virtio-net backend against the host's virtio_net. Each test
starts its own guest under the launcher and leaves no device behind. By
default, the guest exports one end of a veth pair whose other end, in the
guest, runs an iperf3 server. With -x710, the guest drives port 0 of the
X710 through VFIO and exports it, and port 1, which is cabled to it, stays
on the host's i40e in the network namespace vnet-peer as the far end. Build
the guest with scripts/invirtio-build-guest.sh -t net-test. Every test
checks that the guest's error counters stay at zero and that neither
kernel logs a warning. The guests' consoles go to <test>/guest-<n>.log in
the output directory. Run as root on the linux-host kernel, with iperf3,
and with ethtool for -x710.

Tests:`+netTestList())
	build := fs.String("guest", filepath.Join(root, "linux-uml/build-net-test"), "the build directory of the guest")
	out := fs.String("out", filepath.Join(root, "build/test-net"), "the output directory")
	x710 := fs.Bool("x710", false, "export port 0 of the X710, with port 1 as the far end")
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

	s := &netSuite{build: *build, x710: *x710}
	if s.out, err = filepath.Abs(*out); err != nil {
		return err
	}
	if err := s.setup(); err != nil {
		return err
	}
	defer s.teardown()

	// A signal stops the traffic, and the test that runs then fails and
	// cleans up, and no other test runs. A second signal kills the guests
	// and exits.
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
		harness.Cleanup(netName)
		s.teardown()
		harness.CloseOutput()
		os.Exit(1)
	}()

	mark := harness.KernelMark()
	for _, t := range netTests {
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

func netTestList() string {
	var b strings.Builder
	for _, t := range netTests {
		fmt.Fprintf(&b, "\n  %-9s %s", t.name, t.about)
	}
	return b.String()
}

func (s *netSuite) setup() error {
	tools := []string{"iperf3", "vdpa", "ip", "ping"}
	if s.x710 {
		tools = append(tools, "ethtool")
	}
	for _, tool := range tools {
		if harness.Run("sh", "-c", "command -v "+tool).Code != 0 {
			return fmt.Errorf("%s is not installed", tool)
		}
	}
	if err := harness.Modprobe("vduse", "vdpa", "virtio_vdpa", "virtio_net"); err != nil {
		return err
	}
	if err := os.MkdirAll(s.out, 0755); err != nil {
		return err
	}
	if err := harness.TeeOutput(filepath.Join(s.out, "test.log")); err != nil {
		return err
	}
	if !s.x710 {
		s.hostAddr, s.peerAddr = "10.72.0.1", "10.72.0.2"
		s.peerNet, s.netdev = "veth1", "veth0"
		return nil
	}
	s.hostAddr, s.peerAddr = "10.71.0.1", "10.71.0.2"
	s.netdev = "eth0"
	if err := harness.Modprobe("vfio-pci", "i40e"); err != nil {
		return err
	}
	if pids := harness.PCIHolders(netGuestPort); len(pids) > 0 {
		return fmt.Errorf("%s is in use by process %d, stop its guest first", netGuestPort, pids[0])
	}
	s.portDriver = harness.PCIDriver(netGuestPort)
	if net := harness.PCINetdev(netGuestPort); net != "" {
		harness.Release(net)
	}
	var err error
	if s.far, err = netStartPeer(s.peerAddr, "iperf3"); err != nil {
		return err
	}
	s.peerNet = s.far.net
	return nil
}

// teardown returns the X710's ports to the host.
func (s *netSuite) teardown() {
	if !s.x710 {
		return
	}
	s.far.stop()
	s.far = nil
	netRestorePort(s.portDriver)
}

// netRestorePort returns port 0 to the driver that it had before the run.
func netRestorePort(driver string) {
	if harness.PCIDriver(netGuestPort) == driver {
		return
	}
	if driver == "vfio-pci" {
		pci.BindVFIO(netGuestPort)
	} else {
		pci.BindNative(netGuestPort)
	}
}

// runTest runs a test from a clean host, and afterwards removes what its
// guests left and checks their logs.
func (s *netSuite) runTest(t netTest) {
	harness.Cleanup(netName)
	s.dir = filepath.Join(s.out, t.name)
	os.RemoveAll(s.dir)
	os.MkdirAll(s.dir, 0755)
	var err error
	s.logDir, err = harness.MakeLogDir(netName)
	if !s.Check(err == nil, "%s: a directory for the in-flight log is made%s", t.name, errSuffix(err)) {
		return
	}
	s.mu.Lock()
	s.guests = nil
	s.mu.Unlock()
	if s.x710 {
		s.peer(nil, "ip link set "+s.peerNet+" mtu "+fmt.Sprint(netMTU)+" up")
	}

	t.run(s)

	harness.Detach(netName, s.ctx.Err() != nil)
	for _, g := range s.guests {
		g.Stop(30 * time.Second)
		g.Kill()
	}
	harness.Cleanup(netName)
	os.RemoveAll(s.logDir)
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
// or reconnected to the device and its init is ready.
func (s *netSuite) start() *harness.Guest {
	n := len(s.guests) + 1
	if s.ctx.Err() != nil {
		return nil
	}
	l := harness.Launch{
		Build:   s.build,
		Type:    "net",
		Name:    netName,
		Options: fmt.Sprintf("netdev=%s,mtu=%d", s.netdev, netMTU),
		LogDir:  s.logDir,
		Args:    []string{"mem=512M", "init_shell"},
	}
	if s.x710 {
		l.Devices = []string{netGuestPort}
	} else {
		l.Args = append(l.Args, "veth_test")
	}
	g, err := harness.LaunchGuest(filepath.Join(s.dir, fmt.Sprintf("guest-%d.log", n)), l)
	if !s.Check(err == nil, "guest %d starts%s", n, errSuffix(err)) {
		return nil
	}
	s.mu.Lock()
	s.guests = append(s.guests, g)
	s.mu.Unlock()
	ready := harness.Wait(func() bool {
		log := g.Log()
		return (strings.Contains(log, netName+": created") ||
			strings.Contains(log, netName+": reconnecting")) &&
			strings.Contains(log, "net-test-init: ready") || !g.Running()
	}, 2*time.Minute, 200*time.Millisecond)
	if !s.Check(ready && g.Running(), "guest %d reaches the device", n) {
		return nil
	}
	return g
}

// attach adds the device to the vDPA bus and returns the host's interface,
// configured with the given MTU.
func (s *netSuite) attach() string {
	err := harness.Attach(netName)
	if !s.Check(err == nil, "vdpa dev add attaches the device%s", errSuffix(err)) {
		return ""
	}
	var net string
	harness.Wait(func() bool {
		nets := harness.VirtioNetdevs(netName)
		if len(nets) > 0 {
			net = nets[0]
		}
		return net != ""
	}, 30*time.Second, 100*time.Millisecond)
	if !s.Check(net != "", "the host gets an interface") {
		return ""
	}
	harness.Release(net)
	harness.PlaceIRQs(netName, -1)
	if !s.configure(net, netMTU) {
		return ""
	}
	return net
}

// up starts a guest and attaches its device, and waits for carrier.
func (s *netSuite) up() (*harness.Guest, string) {
	g := s.start()
	if g == nil {
		return nil, ""
	}
	net := s.attach()
	if net == "" || !s.Check(s.waitCarrier(net, true), "%s has carrier", net) {
		return nil, ""
	}
	return g, net
}

func (s *netSuite) configure(net string, mtu int) bool {
	var err error
	for _, argv := range [][]string{
		{"ip", "link", "set", net, "mtu", fmt.Sprint(mtu)},
		{"ip", "addr", "replace", s.hostAddr + "/24", "dev", net},
		{"ip", "link", "set", net, "up"},
	} {
		if err = harness.MustRun(argv[0], argv[1:]...); err != nil {
			break
		}
	}
	return s.Check(err == nil, "%s is configured with MTU %d%s", net, mtu, errSuffix(err))
}

func (s *netSuite) waitCarrier(net string, on bool) bool {
	want := map[bool]string{true: "1", false: "0"}[on]
	return harness.Wait(func() bool {
		return harness.Read(filepath.Join("/sys/class/net", net, "carrier")) == want
	}, 30*time.Second, 100*time.Millisecond)
}

// run runs a command that sends traffic, which a signal stops.
func (s *netSuite) run(timeout time.Duration, argv ...string) harness.Result {
	return harness.RunContext(s.ctx, s.dir, timeout, argv[0], argv[1:]...)
}

// peer runs a shell command at the far end, in the guest g with the veth
// pair, and returns its output and whether it succeeded.
func (s *netSuite) peer(g *harness.Guest, cmd string) (string, bool) {
	if s.x710 {
		r := s.run(2*time.Minute, harness.InNS(netPeerNS, "sh", "-c", cmd)...)
		return r.Output(), r.OK()
	}
	out := g.Output(cmd+"; echo peer-status=$?", 2*time.Minute)
	return out, strings.Contains(out, "peer-status=0")
}

// ping pings the far end from the host, or the host from the far end, and
// reports whether replies came back.
func (s *netSuite) ping(g *harness.Guest, fromPeer bool) bool {
	var out string
	if fromPeer {
		out, _ = s.peer(g, "ping -c 5 -W 1 "+s.hostAddr)
	} else {
		out = s.run(time.Minute, "ping", "-c", "5", "-W", "1", s.peerAddr).Output()
	}
	p, ok := harness.ParsePing(out)
	return ok && p.Received > 0
}

// iperf runs iperf3 from the host against the far end's server, and shows
// the state of both ends when the run fails or is slow.
func (s *netSuite) iperf(g *harness.Guest, seconds int, args ...string) (harness.Iperf, error) {
	argv := append([]string{"iperf3", "-c", s.peerAddr, "-J", "-t", fmt.Sprint(seconds)}, args...)
	r := s.run(time.Duration(seconds+60)*time.Second, argv...)
	res, err := harness.ParseIperf(r.Stdout)
	if err == nil && res.ReceivedBPS > netMinRate || s.ctx.Err() != nil {
		return res, err
	}
	fmt.Printf("%s exited with %d%s: %s\n", strings.Join(argv, " "), r.Code, errSuffix(r.Err), strings.TrimSpace(r.Stderr))
	fmt.Println(r.Stdout[max(0, len(r.Stdout)-2000):])
	for _, net := range harness.VirtioNetdevs(netName) {
		fmt.Println(harness.Run("ip", "-s", "-s", "link", "show", net).Stdout)
	}
	if !s.x710 {
		fmt.Println("guest iperf3 server:" + g.Output("pidof iperf3; tail -20 /tmp/iperf3.log", 10*time.Second))
	}
	fmt.Println("guest:" + g.Output("ip -s link show "+s.netdev+"; cat /proc/vduse_guest", 10*time.Second))
	return res, err
}

// healthy checks the guest's error counters and its log.
func (s *netSuite) healthy(g *harness.Guest) {
	c := g.Counters()
	if !s.Check(len(c) > 0, "the guest reports its counters") {
		return
	}
	var bad []string
	for _, name := range netCounters {
		if v, ok := c[name]; !ok || v != 0 {
			bad = append(bad, fmt.Sprintf("%s %d", name, v))
		}
	}
	s.Check(len(bad) == 0, "the guest's error counters are zero%s", listSuffix("but", bad))
	var errs []string
	for _, line := range strings.Split(g.Log(), "\n") {
		for _, e := range netErrors {
			if strings.Contains(line, e) {
				errs = append(errs, strings.TrimSpace(line))
				break
			}
		}
	}
	for _, e := range errs {
		fmt.Println("  " + e)
	}
	s.Check(len(errs) == 0, "the guest logs no error")
}

func netTestDevice(s *netSuite) {
	g, net := s.up()
	if g == nil {
		return
	}
	sys := func(file string) string { return harness.Read(filepath.Join("/sys/class/net", net, file)) }
	out := g.Output("cat /sys/class/net/"+s.netdev+"/address", 10*time.Second)
	mac := strings.TrimSpace(out)
	s.Check(mac != "" && sys("address") == mac, "%s has the address of the guest's %s, %s", net, s.netdev, mac)
	s.Check(sys("mtu") == fmt.Sprint(netMTU), "%s has the guest's MTU %d: %s", net, netMTU, sys("mtu"))
	bits := sys("device/features")
	var missing []string
	for _, f := range netFeatures {
		if f.bit >= len(bits) || bits[f.bit] != '1' {
			missing = append(missing, f.name)
		}
	}
	s.Check(len(missing) == 0, "every feature is negotiated%s", listSuffix("missing", missing))
	s.Check(sys("speed") == "10000" && sys("duplex") == "full",
		"%s reports 10000 Mb/s full duplex: %s %s", net, sys("speed"), sys("duplex"))
	s.healthy(g)
}

func netTestTraffic(s *netSuite) {
	g, net := s.up()
	if g == nil {
		return
	}
	before := g.Counters()
	s.Check(s.ping(g, false), "the host pings the far end")
	s.Check(s.ping(g, true), "the far end pings the host")
	if s.x710 {
		// The guest's veth pair has no IPv6.
		out, _ := s.peer(g, "ip -6 -o addr show dev "+s.peerNet+" scope link")
		m := regexp.MustCompile(`inet6 (fe80::[0-9a-f:]+)/`).FindStringSubmatch(out)
		ok := m != nil
		if ok {
			p, rtt := harness.ParsePing(s.run(time.Minute, "ping", "-c", "5", "-W", "1", m[1]+"%"+net).Output())
			ok = rtt && p.Received > 0
		}
		s.Check(ok, "the host pings the far end's IPv6 link-local address")
	}
	for _, mtu := range []int{netMTU, 1500} {
		if !s.configure(net, mtu) {
			return
		}
		_, ok := s.peer(g, fmt.Sprintf("ip link set %s mtu %d", s.peerNet, mtu))
		s.Check(ok, "the far end's MTU is set to %d", mtu)
		for _, reverse := range []bool{false, true} {
			for _, streams := range []int{1, 4} {
				args := []string{"-P", fmt.Sprint(streams)}
				dir := "host to far end"
				if reverse {
					args = append(args, "-R")
					dir = "far end to host"
				}
				r, err := s.iperf(g, 5, args...)
				if !s.Check(err == nil && r.ReceivedBPS > netMinRate, "iperf3 at MTU %d, %s, with %d stream(s): %.2f Gb/s%s",
					mtu, dir, streams, r.ReceivedBPS/1e9, errSuffix(err)) {
					return
				}
			}
		}
	}
	after := g.Counters()
	for _, name := range []string{"tx_gso", "rx_gso"} {
		s.Check(after[name] > before[name], "the guest's %s grows: %d to %d", name, before[name], after[name])
	}
	s.healthy(g)
}

func netTestLink(s *netSuite) {
	g, net := s.up()
	if g == nil {
		return
	}
	_, ok := s.peer(g, "ip link set "+s.peerNet+" down")
	s.Check(ok && s.waitCarrier(net, false), "%s loses carrier when the far end goes down", net)
	_, ok = s.peer(g, "ip link set "+s.peerNet+" up")
	s.Check(ok && s.waitCarrier(net, true), "%s gets carrier back when the far end comes up", net)
	s.Check(s.ping(g, false), "the host pings the far end")
	c := g.Counters()
	s.Check(c["link_changes"] >= 2, "the guest reports the link changes: %d", c["link_changes"])
	s.healthy(g)
}

func netTestReattach(s *netSuite) {
	g, _ := s.up()
	if g == nil {
		return
	}
	for i := 0; i < 2; i++ {
		if err := harness.Detach(netName, false); !s.Check(err == nil, "vdpa dev del removes the device%s", errSuffix(err)) {
			return
		}
		net := s.attach()
		if net == "" {
			return
		}
		s.Check(s.waitCarrier(net, true) && s.ping(g, false), "traffic passes after attaching again")
	}
	r, err := s.iperf(g, 5)
	s.Check(err == nil && r.ReceivedBPS > netMinRate, "iperf3 passes: %.2f Gb/s%s", r.ReceivedBPS/1e9, errSuffix(err))
	s.healthy(g)
}

func netTestKill(s *netSuite) {
	g, net := s.up()
	if g == nil {
		return
	}
	// iperf3 runs across the kill and the relaunch. The veth pair's server
	// runs in the guest and dies with it, so only the X710's run finishes.
	ctx, cancel := context.WithCancel(s.ctx)
	var r harness.Result
	done := make(chan struct{})
	go func() {
		r = harness.RunContext(ctx, s.dir, 3*time.Minute, "iperf3", "-c", s.peerAddr, "-J", "-t", "60")
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()
	time.Sleep(5 * time.Second)
	uml := harness.UMLPid(g.Pid())
	if !s.Check(uml > 0, "the guest's UML process is found") {
		return
	}
	s.healthy(g)
	syscall.Kill(uml, syscall.SIGKILL)
	s.Check(g.Wait(30*time.Second), "the launcher exits when its guest is killed")
	g2 := s.start()
	if g2 == nil {
		return
	}
	s.Check(strings.Contains(g2.Log(), netName+": reconnecting"), "the relaunched guest reconnects to the device")
	s.Check(s.waitCarrier(net, true), "%s, which stays, gets carrier back", net)
	<-done
	if s.x710 {
		res, err := harness.ParseIperf(r.Stdout)
		s.Check(err == nil && res.LastBytes > 0, "an iperf3 run across the kill finishes with traffic in its last second%s",
			errSuffix(err))
	}
	s.Check(s.ping(g2, false), "the host pings the far end through the relaunched guest")
	res, err := s.iperf(g2, 5)
	s.Check(err == nil && res.ReceivedBPS > netMinRate, "iperf3 passes: %.2f Gb/s%s", res.ReceivedBPS/1e9, errSuffix(err))
	s.healthy(g2)
}

func netTestStop(s *netSuite) {
	g, _ := s.up()
	if g == nil {
		return
	}
	s.Check(s.ping(g, false), "the host pings the far end")
	s.healthy(g)
	s.Check(strings.Contains(g.Log(), "Installed the sandbox filter") &&
		!strings.Contains(g.Log(), "The sandbox denied"), "the guest runs under its seccomp filter, which denies nothing")
	if err := harness.Detach(netName, false); !s.Check(err == nil, "vdpa dev del removes the device%s", errSuffix(err)) {
		return
	}
	g.Signal(syscall.SIGTERM)
	s.Check(g.Wait(30*time.Second) && g.ExitCode() == 0, "the launcher stops the guest in order")
	s.Check(!harness.Exists("/dev/vduse/"+netName), "no VDUSE device is left")
	if s.x710 {
		s.Check(harness.PCIDriver(netGuestPort) == "vfio-pci", "port 0 stays on vfio-pci")
	}
}

// netFarEnd is port 1 of the X710 in netPeerNS, with its servers.
type netFarEnd struct {
	net     string
	servers []*exec.Cmd
	// linkDownOnClose is i40e's flag before the test, which it restores.
	linkDownOnClose string
}

var linkDownOnCloseRE = regexp.MustCompile(`(?m)^link-down-on-close\s*:\s*(on|off)`)

// netStartPeer moves port 1 of the X710 to netPeerNS with the address
// addr, and starts the servers there.
func netStartPeer(addr string, servers ...string) (*netFarEnd, error) {
	// Deleting a namespace that an earlier run left returns port 1 to the
	// host.
	harness.Run("ip", "netns", "del", netPeerNS)
	harness.Wait(func() bool { return harness.PCINetdev(netPeerPort) != "" }, 10*time.Second, 100*time.Millisecond)
	f := &netFarEnd{net: harness.PCINetdev(netPeerPort)}
	if f.net == "" {
		return nil, fmt.Errorf("%s has no interface on the host", netPeerPort)
	}
	m := linkDownOnCloseRE.FindStringSubmatch(harness.Run("ethtool", "--show-priv-flags", f.net).Stdout)
	if m == nil {
		return nil, fmt.Errorf("ethtool shows no link-down-on-close flag for %s", f.net)
	}
	f.linkDownOnClose = m[1]
	// i40e keeps the link up when the interface goes down unless this flag
	// is set, and then the guest's port never loses its link.
	if err := harness.MustRun("ethtool", "--set-priv-flags", f.net, "link-down-on-close", "on"); err != nil {
		return nil, err
	}
	harness.Release(f.net)
	for _, argv := range [][]string{
		{"ip", "netns", "add", netPeerNS},
		{"ip", "link", "set", f.net, "netns", netPeerNS},
		harness.InNS(netPeerNS, "ip", "link", "set", f.net, "mtu", fmt.Sprint(netMTU), "up"),
		harness.InNS(netPeerNS, "ip", "addr", "add", addr+"/24", "dev", f.net),
		harness.InNS(netPeerNS, "ip", "link", "set", "lo", "up"),
	} {
		if err := harness.MustRun(argv[0], argv[1:]...); err != nil {
			f.stop()
			return nil, err
		}
	}
	for _, server := range servers {
		argv := harness.InNS(netPeerNS, server, "-s")
		if server == "netserver" {
			argv = harness.InNS(netPeerNS, server, "-D")
		}
		cmd := exec.Command(argv[0], argv[1:]...)
		// The server is in a process group of its own, so that a terminal's
		// signals reach only invirtio-test.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			f.stop()
			return nil, err
		}
		f.servers = append(f.servers, cmd)
	}
	return f, nil
}

// stop stops the servers and returns port 1 to the host.
func (f *netFarEnd) stop() {
	if f == nil {
		return
	}
	for _, cmd := range f.servers {
		cmd.Process.Kill()
		cmd.Wait()
	}
	f.servers = nil
	harness.RunArgv(harness.InNS(netPeerNS, "ip", "link", "set", f.net, "netns", "1"))
	harness.Run("ip", "netns", "del", netPeerNS)
	harness.Wait(func() bool { return harness.PCINetdev(netPeerPort) != "" }, 10*time.Second, 100*time.Millisecond)
	harness.Run("ethtool", "--set-priv-flags", f.net, "link-down-on-close", f.linkDownOnClose)
}
