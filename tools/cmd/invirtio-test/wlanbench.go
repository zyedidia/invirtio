package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"invirtio/internal/bench"
	"invirtio/internal/harness"
	"invirtio/internal/pci"
	"invirtio/internal/vdpa"
)

func init() {
	register("bench-wlan", "benchmark a WiFi card through the guest against its host driver", cmdBenchWlan)
	register("bench-wlan-summary", "print the results of bench-wlan again", cmdBenchWlanSummary)
}

const (
	wlanBenchName = "vwlan-bench"
	wlanBenchNS   = "wlan-bench"
	wlanBenchPort = 5299
)

var wlanBenchTargets = []string{"native", "guest"}

var wlanBenchJobs = []bench.Job{
	{Name: "scan", About: "a scan with iw, before joining", Metrics: []string{"seconds", "bss"}},
	{Name: "connect", About: "from the start of wpa_supplicant until it completes the connection", Metrics: []string{"seconds"}},
	{Name: "upload", About: "iperf3 TCP to the server", Metrics: []string{"mbps", "retransmits"}},
	{Name: "download", About: "iperf3 TCP from the server", Metrics: []string{"mbps", "retransmits"}},
	{Name: "ping-idle", About: "50 pings of the server at 200 ms on an idle link", Metrics: []string{"avg_ms", "max_ms", "mdev_ms", "loss_pct"}},
	{Name: "ping-load", About: "pings of the server at 200 ms during a download", Metrics: []string{"avg_ms", "max_ms", "mdev_ms", "loss_pct"}},
}

// wlanBenchUdhcpc is the script of the DHCP client in the namespace, which
// adds the address and a default route.
const wlanBenchUdhcpc = `#!/bin/sh
case "$1" in
bound|renew)
	ip addr flush dev "$interface"
	ip addr add "$ip/$mask" dev "$interface"
	[ -n "$router" ] && ip route replace default via "${router%% *}"
	;;
esac
exit 0
`

type wlanBenchSuite struct {
	out      string
	build    string
	card     string
	conf     string
	server   string
	noServer bool
	// hostDriver is the card's host driver, and unbindNative allows the
	// benchmark to unbind it.
	hostDriver   string
	unbindNative bool
	seconds      int
	omit         int
	mem          string
	opts         *bench.Options
	results      bench.Results
	// The guest of the guest target and its cgroup, whose memory is
	// sampled between runs and whose CPU use is measured.
	guest  *harness.Guest
	cgroup string
	memory *harness.GuestMemory
	// ctx ends on a signal, which stops a running iperf3, and the target
	// that runs then fails and cleans up.
	ctx context.Context
}

func cmdBenchWlan(args []string) error {
	root := harness.RepoRoot()
	fs := newFlags("bench-wlan", `
usage: invirtio-test bench-wlan [options] -conf <wpa_supplicant.conf> -server <address> [<target>...]

Measure a WiFi card's throughput, latency, scan and connection time against
an iperf3 server that is reached through the access point. Each target is
one of:
  native   the card on its host driver
  guest    the card driven by the invirtio guest under the launcher and
           exported to the host's virtio_wlan

The default runs both. Each round brings up every target once, and every
other round runs the targets in the reverse order. Removing mt7921e hangs
the 7.2-rc host, which also blacklists it, so by default bench-wlan loads
the host driver itself, runs every native round after all the guest
rounds, and never unbinds the host driver. The card then stays on it, and
giving the card back to its guest takes a reboot. With -unbind-native, the
targets interleave and the card returns to its earlier driver.

For each target, the interface is taken from NetworkManager and the system
wpa_supplicant, and its wiphy moves into the network namespace wlan-bench,
so that traffic to a server on this host crosses the radio instead of being
delivered locally. There power save is turned off, the card scans once
with iw, and it joins the network of -conf with wpa_supplicant and gets a
lease with busybox udhcpc. The server is one of this host's addresses on
the access point's uplink, where bench-wlan runs an iperf3 server on port
5299 unless -no-server is given, or a host behind the access point that
runs one. Each round then runs the jobs -reps times, each iperf3 run for
-time seconds after -omit seconds that it leaves out.

The benchmark's programs run on -app-cpus, and the iperf3 server on
-peer-cpus. The card's interrupts and the guest's CPU, its helper threads
and its VDUSE interrupts go to -device-cpu. Each run records how many of the
host's CPUs were busy and how many of each set's, and with the guest, how
many CPUs the guest's cgroup took. During the run, every CPU uses the
performance governor, boost is as -boost says, and irqbalance is stopped.

The guest runs with -mem in a cgroup of its own, as invirtio's guests do,
and the results record the cgroup's peak memory and the most memory that
the guest itself used between runs. The card must not be in use, so stop
its invirtio service first. Afterwards the card returns to its earlier
driver, except that without -unbind-native it stays on a host driver that
a native round bound, and its invirtio service must then not start before
a reboot. Build the guest with scripts/invirtio-build-guest.sh. The output directory
gets results.json, runs.csv with one metric of one run on each line, the
summary, which bench-wlan-summary prints again, and the logs. Run as root on
the linux-host kernel, with iw, wpa_supplicant, iperf3 and busybox.

Jobs:`+wlanBenchJobList())
	build := fs.String("guest", filepath.Join(root, "linux-uml/build-invirtio"), "the build directory of the guest")
	out := fs.String("out", filepath.Join(root, "build/bench-wlan"), "the output directory")
	card := fs.String("device", "0000:06:00.0", "the PCI address of the card")
	conf := fs.String("conf", "", "the wpa_supplicant configuration of the network")
	server := fs.String("server", "", "the address of the iperf3 server")
	noServer := fs.Bool("no-server", false, "use an iperf3 server that already runs")
	seconds := fs.Int("time", 10, "the seconds that each iperf3 run measures")
	omit := fs.Int("omit", 2, "the seconds at the start of each iperf3 run that it leaves out")
	mem := fs.String("mem", "256M", "the guest's memory, as UML's mem= takes it")
	hostDriver := fs.String("host-driver", "mt7921e", "the card's host driver, which is loaded for the native target")
	unbindNative := fs.Bool("unbind-native", false, "allow unbinding the host driver, so that native and guest rounds interleave")
	opts := bench.AddFlags(fs, 5, 2)
	fs.Parse(args)
	targets := fs.Args()
	if len(targets) == 0 {
		targets = wlanBenchTargets
	}
	for _, t := range targets {
		if !slices.Contains(wlanBenchTargets, t) {
			fs.Usage()
			os.Exit(2)
		}
	}
	if *conf == "" || *server == "" {
		fs.Usage()
		os.Exit(2)
	}
	if err := opts.Check(); err != nil {
		return err
	}
	requireRoot()

	s := &wlanBenchSuite{build: *build, conf: *conf, server: *server, noServer: *noServer, mem: *mem,
		hostDriver: *hostDriver, unbindNative: *unbindNative,
		seconds: *seconds, omit: *omit, opts: opts}
	var err error
	if s.card, err = pci.NormalizeAddress(*card); err != nil {
		return err
	}
	if s.out, err = filepath.Abs(*out); err != nil {
		return err
	}
	if err := os.MkdirAll(s.out, 0755); err != nil {
		return err
	}
	if err := harness.TeeOutput(filepath.Join(s.out, "bench.log")); err != nil {
		return err
	}
	for _, tool := range []string{"iw", "wpa_supplicant", "wpa_cli", "iperf3", "busybox", "vdpa", "ping", "taskset", "ethtool"} {
		if harness.Run("sh", "-c", "command -v "+tool).Code != 0 {
			return fmt.Errorf("%s is not installed", tool)
		}
	}
	if err := harness.Modprobe("cfg80211", "vduse", "vdpa", "virtio_vdpa", "virtio_wlan", "vfio-pci"); err != nil {
		return err
	}
	if vdpa.Exists(wlanBenchName) {
		return fmt.Errorf("/dev/vduse/%s exists, perhaps from an earlier run, so remove it first", wlanBenchName)
	}
	if pids := harness.PCIHolders(s.card); len(pids) > 0 {
		return fmt.Errorf("%s is in use by process %d, stop its guest first", s.card, pids[0])
	}
	if err := os.WriteFile(filepath.Join(s.out, "udhcpc.sh"), []byte(wlanBenchUdhcpc), 0755); err != nil {
		return err
	}

	// The card goes back to the driver it had, unless that means unbinding
	// a host driver.
	before := harness.PCIDriver(s.card)
	if hostBound(before) && !s.unbindNative && slices.Contains(targets, "guest") {
		return fmt.Errorf("%s is on %s, which the guest target would have to unbind, so reboot first or give -unbind-native", s.card, before)
	}
	defer func() {
		harness.Cleanup(wlanBenchName)
		if harness.PCIDriver(s.card) == before {
			return
		}
		if d := harness.PCIDriver(s.card); hostBound(d) && !s.unbindNative {
			fmt.Printf("%s stays on %s. Do not start its invirtio service before a reboot, which gives the card back to its guest.\n", s.card, d)
			return
		}
		if before == "vfio-pci" {
			pci.BindVFIO(s.card)
		} else {
			pci.BindNative(s.card)
		}
	}()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer cancel()
	s.ctx = ctx
	tuning, err := opts.Tune()
	if err != nil {
		return err
	}
	defer tuning.Restore()
	s.results.Env = s.env()
	s.results.Targets = targets
	// Without -unbind-native, the native rounds come after all the others,
	// so that the host driver, whose removal can hang, is never unbound.
	type slot struct {
		round  int
		target string
	}
	var others []string
	for _, t := range targets {
		if t != "native" || s.unbindNative {
			others = append(others, t)
		}
	}
	var schedule []slot
	for round := 1; round <= opts.Rounds; round++ {
		for _, t := range bench.Order(others, round) {
			schedule = append(schedule, slot{round, t})
		}
	}
	if len(others) < len(targets) {
		for round := 1; round <= opts.Rounds; round++ {
			schedule = append(schedule, slot{round, "native"})
		}
	}
	for _, sl := range schedule {
		if ctx.Err() != nil {
			return errors.New("stopped by a signal")
		}
		fmt.Printf("== round %d: %s\n", sl.round, sl.target)
		in := bench.Instance{Target: sl.target, Round: sl.round}
		if sl.target == "native" {
			err = s.runNative(&in)
		} else {
			err = s.runGuest(&in)
		}
		if err != nil {
			return fmt.Errorf("round %d: %s: %v", sl.round, sl.target, err)
		}
		s.results.Instances = append(s.results.Instances, in)
		if err := s.results.Save(s.out); err != nil {
			return err
		}
	}
	summary := wlanBenchSummary(&s.results)
	fmt.Print(summary)
	return os.WriteFile(filepath.Join(s.out, "summary.txt"), []byte(summary), 0644)
}

func wlanBenchJobList() string {
	var b strings.Builder
	for _, j := range wlanBenchJobs {
		fmt.Fprintf(&b, "\n  %-10s %s", j.Name, j.About)
	}
	return b.String()
}

func (s *wlanBenchSuite) env() map[string]string {
	env := bench.Environment()
	for k, v := range s.opts.Describe() {
		env[k] = v
	}
	env["card"] = strings.TrimSpace(harness.Run("lspci", "-s", s.card).Stdout)
	env["server"] = s.server
	env["time"] = strconv.Itoa(s.seconds)
	env["omit"] = strconv.Itoa(s.omit)
	env["mem"] = s.mem
	env["guest_build"] = s.build
	env["iperf3"] = strings.SplitN(harness.Run("iperf3", "--version").Stdout, "\n", 2)[0]
	env["wpa_supplicant"] = strings.TrimSpace(harness.Run("wpa_supplicant", "-v").Stdout)
	return env
}

// wiphy waits for a wiphy and an interface under a device directory, and
// for udev to rename the interface.
func wiphy(dir string) (phy, net string) {
	find := func() bool {
		phys := harness.Glob(filepath.Join(dir, "ieee80211/*"))
		nets := harness.Glob(filepath.Join(dir, "net/*"))
		phy, net = "", ""
		if len(phys) > 0 && len(nets) > 0 {
			phy, net = phys[0], nets[0]
		}
		return net != ""
	}
	if harness.Wait(find, 30*time.Second, 100*time.Millisecond) {
		harness.Run("udevadm", "settle")
		find()
	}
	return phy, net
}

// hostBound reports whether a driver is one of the host's own, which
// bench-wlan unbinds only with -unbind-native.
func hostBound(driver string) bool {
	return driver != "" && driver != "vfio-pci"
}

// runNative brings the card up on its host driver. Without -unbind-native,
// a card that the driver already has stays bound.
func (s *wlanBenchSuite) runNative(in *bench.Instance) error {
	if err := harness.Modprobe(s.hostDriver); err != nil {
		return err
	}
	if !hostBound(harness.PCIDriver(s.card)) || s.unbindNative {
		if err := pci.BindNative(s.card); err != nil {
			return err
		}
	}
	phy, net := wiphy(pci.Path(s.card, ""))
	if net == "" {
		return fmt.Errorf("%s's driver %q made no wiphy", s.card, harness.PCIDriver(s.card))
	}
	if s.opts.DeviceCPU != "" {
		restore, err := bench.SteerIRQs(s.card, s.opts.DeviceCPU)
		if err != nil {
			return err
		}
		defer restore()
	}
	return s.measure(in, phy, net)
}

func (s *wlanBenchSuite) runGuest(in *bench.Instance) error {
	logDir, err := harness.MakeLogDir(wlanBenchName)
	if err != nil {
		return err
	}
	defer os.RemoveAll(logDir)
	// The guest runs in a cgroup of its own, as invirtio's guests do, which
	// records its peak memory and its CPU time.
	cg, err := harness.MakeCgroup(wlanBenchName)
	if err != nil {
		return err
	}
	defer harness.RemoveCgroup(cg)
	l := harness.Launch{
		Build:   s.build,
		Type:    "wlan",
		Name:    wlanBenchName,
		LogDir:  logDir,
		Devices: []string{s.card},
		Args:    []string{"mem=" + s.mem},
		Cgroup:  cg,
		IRQCPUs: s.opts.DeviceCPU,
	}
	if s.opts.DeviceCPU != "" {
		l.Args = append(l.Args, "cpu_pin="+s.opts.DeviceCPU)
	}
	g, err := harness.LaunchGuest(filepath.Join(s.out, fmt.Sprintf("guest-%d.log", in.Round)), l)
	if err != nil {
		return err
	}
	defer func() {
		harness.Detach(wlanBenchName, !g.Running())
		g.Stop(30 * time.Second)
		g.Kill()
		harness.Cleanup(wlanBenchName)
		s.guest, s.cgroup, s.memory = nil, "", nil
	}()
	if !g.WaitFor(wlanBenchName+": created", 2*time.Minute) {
		return errors.New("the guest did not create its device, see " + g.Path)
	}
	if err := harness.Attach(wlanBenchName); err != nil {
		return err
	}
	devs, _ := filepath.Glob(filepath.Join("/sys/bus/vdpa/devices", wlanBenchName, "virtio*"))
	if len(devs) == 0 {
		return errors.New("virtio_vdpa made no virtio device")
	}
	phy, net := wiphy(devs[0])
	if net == "" {
		return errors.New("virtio_wlan made no wiphy")
	}
	cpu := -1
	if s.opts.DeviceCPU != "" {
		cpu, _ = strconv.Atoi(s.opts.DeviceCPU)
	}
	if err := harness.PlaceIRQs(wlanBenchName, cpu); err != nil {
		return err
	}
	s.guest, s.cgroup = g, cg
	s.memory = &harness.GuestMemory{Mem: s.mem}
	s.memory.SampleGuest(g.Output("cat /proc/meminfo", 10*time.Second))
	if err := s.measure(in, phy, net); err != nil {
		return err
	}
	in.Counters = g.Counters()
	s.memory.SampleGuest(g.Output("cat /proc/meminfo", 10*time.Second))
	if err := s.memory.ReadCgroup(cg); err != nil {
		return err
	}
	in.Memory = s.memory
	fmt.Println("  memory: " + in.Memory.Summary())
	return nil
}

var (
	wlanBSSRE       = regexp.MustCompile(`(?m)^BSS `)
	wlanPowerSaveRE = regexp.MustCompile(`Power save: (\w+)`)
)

// measure joins the network in the namespace and runs the jobs.
func (s *wlanBenchSuite) measure(in *bench.Instance, phy, net string) error {
	dir := filepath.Join(s.out, fmt.Sprintf("%s-%d", in.Target, in.Round))
	os.MkdirAll(dir, 0755)
	ns := func(argv ...string) []string { return harness.InNS(wlanBenchNS, argv...) }
	app := func(argv ...string) []string { return ns(bench.Pin(s.opts.AppCPUs, argv...)...) }
	harness.Release(net)
	// A new wiphy starts blocked while the host's WiFi is switched off.
	softs, _ := filepath.Glob(filepath.Join("/sys/class/ieee80211", phy, "rfkill*/soft"))
	for _, soft := range softs {
		harness.Write(soft, "0")
	}
	harness.Run("ip", "netns", "del", wlanBenchNS)
	defer func() {
		harness.RunArgv(ns("iw", "phy", phy, "set", "netns", "1"))
		harness.Run("ip", "netns", "del", wlanBenchNS)
		harness.Run("nmcli", "device", "set", net, "managed", "yes")
	}()
	for _, argv := range [][]string{
		{"ip", "netns", "add", wlanBenchNS},
		{"iw", "phy", phy, "set", "netns", "name", wlanBenchNS},
		ns("ip", "link", "set", "lo", "up"),
		ns("ip", "link", "set", net, "up"),
		ns("iw", "dev", net, "set", "power_save", "off"),
	} {
		if err := harness.MustRun(argv[0], argv[1:]...); err != nil {
			return err
		}
	}
	start := func(logName string, argv ...string) (*exec.Cmd, error) {
		logf, err := os.Create(filepath.Join(dir, logName))
		if err != nil {
			return nil, err
		}
		defer logf.Close()
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Stdout, cmd.Stderr = logf, logf
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		return cmd, cmd.Start()
	}
	stop := func(cmd *exec.Cmd) {
		cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			cmd.Process.Kill()
			<-done
		}
	}
	if !s.noServer {
		argv := bench.Pin(s.opts.PeerCPUs, "iperf3", "-s", "-B", s.server, "-p", fmt.Sprint(wlanBenchPort))
		server, err := start("iperf3-server.log", argv...)
		if err != nil {
			return err
		}
		defer stop(server)
	}

	// record runs one repetition of a job with its CPU use.
	record := func(job string, rep int, run func() (map[string]float64, error)) error {
		if s.ctx.Err() != nil {
			return errors.New("stopped by a signal")
		}
		meter := bench.StartMeter(s.opts.Sets(), s.cgroup)
		m, err := run()
		cpu := meter.Stop()
		if err != nil {
			return fmt.Errorf("%s: %v", job, err)
		}
		for k, v := range cpu {
			m[k] = v
		}
		j := wlanBenchJobs[slices.IndexFunc(wlanBenchJobs, func(j bench.Job) bool { return j.Name == job })]
		fmt.Printf("  %s %d: %s\n", job, rep, bench.Format(j, m))
		s.results.Runs = append(s.results.Runs, bench.Run{Job: job, Target: in.Target, Round: in.Round, Rep: rep, Metrics: m})
		if s.guest != nil {
			s.memory.SampleGuest(s.guest.Output("cat /proc/meminfo", 10*time.Second))
		}
		return nil
	}

	err := record("scan", 1, func() (map[string]float64, error) {
		argv := app("iw", "dev", net, "scan")
		t0 := time.Now()
		r := harness.RunContext(s.ctx, dir, time.Minute, argv[0], argv[1:]...)
		elapsed := time.Since(t0).Seconds()
		if !r.OK() {
			return nil, fmt.Errorf("iw scan: %s", strings.TrimSpace(r.Output()))
		}
		return map[string]float64{"seconds": elapsed, "bss": float64(len(wlanBSSRE.FindAllString(r.Stdout, -1)))}, nil
	})
	if err != nil {
		return err
	}

	// The configuration gets a control interface of the test's own, which
	// keeps the passphrase off the command line.
	data, err := os.ReadFile(s.conf)
	if err != nil {
		return err
	}
	ctrl := filepath.Join(dir, "wpa")
	conf := filepath.Join(dir, "wpa_supplicant.conf")
	text := "ctrl_interface=" + ctrl + "\n" + regexp.MustCompile(`(?m)^ctrl_interface=.*\n`).ReplaceAllString(string(data), "")
	if err := os.WriteFile(conf, []byte(text), 0600); err != nil {
		return err
	}
	var wpa *exec.Cmd
	err = record("connect", 1, func() (map[string]float64, error) {
		t0 := time.Now()
		var err error
		if wpa, err = start("wpa_supplicant.log", app("wpa_supplicant", "-i", net, "-D", "nl80211", "-c", conf)...); err != nil {
			wpa = nil
			return nil, err
		}
		joined := harness.Wait(func() bool {
			return strings.Contains(harness.RunArgv(app("wpa_cli", "-p", ctrl, "-i", net, "status")).Stdout, "wpa_state=COMPLETED")
		}, 30*time.Second, 20*time.Millisecond)
		if !joined {
			return nil, fmt.Errorf("wpa_supplicant did not join the network, see %s", filepath.Join(dir, "wpa_supplicant.log"))
		}
		return map[string]float64{"seconds": time.Since(t0).Seconds()}, nil
	})
	if wpa != nil {
		defer stop(wpa)
	}
	if err != nil {
		return err
	}
	ps := wlanPowerSaveRE.FindStringSubmatch(harness.RunArgv(ns("iw", "dev", net, "get", "power_save")).Stdout)
	if ps == nil || ps[1] != "off" {
		return fmt.Errorf("power save is not off on %s", net)
	}
	r := harness.RunArgv(ns("busybox", "udhcpc", "-i", net, "-f", "-n", "-q", "-t", "5", "-T", "2",
		"-s", filepath.Join(s.out, "udhcpc.sh")))
	if !r.OK() {
		return fmt.Errorf("no DHCP lease: %s", strings.TrimSpace(r.Output()))
	}
	in.Info = map[string]string{
		"iw link":    strings.TrimSpace(harness.RunArgv(ns("iw", "dev", net, "link")).Stdout),
		"power_save": ps[1],
		"ethtool -i": strings.TrimSpace(harness.RunArgv(ns("ethtool", "-i", net)).Stdout),
	}
	for _, line := range strings.Split(in.Info["iw link"], "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), ": "); ok {
			in.Info["link "+k] = v
		}
	}

	ping := func(count int) (map[string]float64, error) {
		argv := app("ping", "-q", "-c", fmt.Sprint(count), "-i", "0.2", "-W", "2", s.server)
		r := harness.RunContext(s.ctx, dir, time.Duration(count)*time.Second+time.Minute, argv[0], argv[1:]...)
		p, ok := harness.ParsePing(r.Output())
		if !ok {
			return nil, fmt.Errorf("ping printed %q", strings.TrimSpace(r.Output()))
		}
		return map[string]float64{"avg_ms": p.Avg, "max_ms": p.Max, "mdev_ms": p.Mdev, "loss_pct": p.Loss}, nil
	}
	iperf := func(ctx context.Context, reverse bool) (map[string]float64, error) {
		argv := app("iperf3", "-c", s.server, "-p", fmt.Sprint(wlanBenchPort), "-t", fmt.Sprint(s.seconds),
			"-O", fmt.Sprint(s.omit), "-J")
		if reverse {
			argv = append(argv, "-R")
		}
		var res harness.Iperf
		var err error
		// The server stops listening for a moment between runs.
		for i := 0; i < 5; i++ {
			r := harness.RunContext(ctx, dir, time.Duration(s.seconds+s.omit+60)*time.Second, argv[0], argv[1:]...)
			if res, err = harness.ParseIperf(r.Stdout); err == nil || !strings.Contains(err.Error(), "refused") {
				break
			}
			time.Sleep(time.Second)
		}
		if err != nil {
			return nil, err
		}
		return map[string]float64{"mbps": res.ReceivedBPS / 1e6, "retransmits": float64(res.Retransmits)}, nil
	}
	for rep := 1; rep <= s.opts.Reps; rep++ {
		if err := record("ping-idle", rep, func() (map[string]float64, error) { return ping(50) }); err != nil {
			return err
		}
		if err := record("upload", rep, func() (map[string]float64, error) { return iperf(s.ctx, false) }); err != nil {
			return err
		}
		if err := record("download", rep, func() (map[string]float64, error) { return iperf(s.ctx, true) }); err != nil {
			return err
		}
		// The round trips under load are measured during one more download.
		err := record("ping-load", rep, func() (map[string]float64, error) {
			ctx, cancel := context.WithCancel(s.ctx)
			done := make(chan struct{})
			go func() {
				iperf(ctx, true)
				close(done)
			}()
			time.Sleep(time.Duration(s.omit+1) * time.Second)
			m, err := ping(max(1, (s.seconds-2)*5))
			cancel()
			<-done
			return m, err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func cmdBenchWlanSummary(args []string) error {
	fs := newFlags("bench-wlan-summary", `
usage: invirtio-test bench-wlan-summary <output dir>

Print the summary of a bench-wlan run from its results.json.`)
	fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		os.Exit(2)
	}
	r, err := bench.Load(fs.Arg(0))
	if err != nil {
		return err
	}
	fmt.Print(wlanBenchSummary(r))
	return nil
}

// wlanBenchSummary shows the link of each round's connection before the
// summary of the jobs.
func wlanBenchSummary(r *bench.Results) string {
	var b strings.Builder
	for _, in := range r.Instances {
		fmt.Fprintf(&b, "%s round %d: %s MHz, %s, tx %s, rx %s, signal %s\n", in.Target, in.Round,
			in.Info["link freq"], in.Info["link SSID"], in.Info["link tx bitrate"], in.Info["link rx bitrate"],
			in.Info["link signal"])
	}
	b.WriteString("\n")
	return b.String() + r.Summary(wlanBenchJobs)
}
