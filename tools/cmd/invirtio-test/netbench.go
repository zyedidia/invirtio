package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"invirtio/internal/harness"
	"invirtio/internal/pci"
	"invirtio/internal/vdpa"
)

func init() {
	register("bench-net", "benchmark the VDUSE virtio-net backend against the host's i40e", cmdBenchNet)
	register("bench-net-summary", "print the results of bench-net again", cmdBenchNetSummary)
}

const (
	netBenchName = "vnet-bench"
	// The namespace of port 0 when it runs on the host's i40e.
	netNativeNS = "vnet-native"
	netHostAddr = "10.71.0.1"
	netPeerAddr = "10.71.0.2"
)

// A netBenchJob is one measurement, run against every target. Its metrics
// are the keys of a run's Metrics that the summary shows.
type netBenchJob struct {
	name    string
	about   string
	metrics []string
	run     func(s *netBenchSuite, ns string) (map[string]float64, error)
}

var netBenchJobs = []netBenchJob{
	{"tcp-tx-1", "TCP to the far end, one stream", []string{"gbps", "retransmits"}, netBenchTCP(false, 1)},
	{"tcp-tx-4", "TCP to the far end, four streams", []string{"gbps", "retransmits"}, netBenchTCP(false, 4)},
	{"tcp-rx-1", "TCP from the far end, one stream", []string{"gbps", "retransmits"}, netBenchTCP(true, 1)},
	{"tcp-rx-4", "TCP from the far end, four streams", []string{"gbps", "retransmits"}, netBenchTCP(true, 4)},
	{"udp64-tx", "64-byte UDP datagrams to the far end at an unlimited rate", []string{"sent_kpps", "received_kpps", "loss_pct"}, netBenchUDP(false)},
	{"udp64-rx", "64-byte UDP datagrams from the far end at an unlimited rate", []string{"sent_kpps", "received_kpps", "loss_pct"}, netBenchUDP(true)},
	{"tcp-rr", "netperf TCP_RR with 1-byte messages", []string{"tps", "mean_us", "p99_us"}, netBenchRR("TCP_RR")},
	{"udp-rr", "netperf UDP_RR with 1-byte messages", []string{"tps", "mean_us", "p99_us"}, netBenchRR("UDP_RR")},
	{"ping-idle", "ping round trips on an idle link", []string{"avg_us", "max_us", "mdev_us"}, netBenchPing(false)},
	{"ping-load", "ping round trips during a TCP stream", []string{"avg_us", "max_us", "mdev_us"}, netBenchPing(true)},
}

// NetBenchRun is one repetition of one job against one target.
type NetBenchRun struct {
	Job     string             `json:"job"`
	Rep     int                `json:"rep"`
	Metrics map[string]float64 `json:"metrics"`
	// HostCPUs is how many of the host's CPUs were busy. UMLCPUs is the
	// guest's UML process's share of them, and GuestBusy how busy the
	// guest's own CPU was, from 0 to 1.
	HostCPUs  float64 `json:"host_cpus"`
	UMLCPUs   float64 `json:"uml_cpus,omitempty"`
	GuestBusy float64 `json:"guest_busy,omitempty"`
	HasGuest  bool    `json:"has_guest"`
}

type NetBenchTarget struct {
	Name string        `json:"name"`
	Runs []NetBenchRun `json:"runs"`
	// The guest's /proc/vduse_guest after the target's jobs.
	Counters map[string]int64 `json:"counters,omitempty"`
}

type NetBenchResults struct {
	Host    map[string]string `json:"host"`
	Targets []NetBenchTarget  `json:"targets"`
}

type netBenchSuite struct {
	out     string
	build   string
	reps    int
	seconds int
	jobs    []netBenchJob
	far     *netFarEnd
	// portDriver is port 0's driver before the run, which it restores.
	portDriver string
	results    NetBenchResults
	// The guest of the guest target, whose CPU use is measured.
	guest *harness.Guest
	uml   int
	// ctx ends on a signal, which stops a running job, and the target that
	// runs then fails and cleans up.
	ctx context.Context
}

func cmdBenchNet(args []string) error {
	root := harness.RepoRoot()
	fs := newFlags("bench-net", `
usage: invirtio-test bench-net [options] [<target>...]

Benchmark the VDUSE virtio-net backend on the X710, whose two ports are
cabled to each other. Port 1 stays on the host's i40e in the network
namespace vnet-peer, with iperf3 and netserver, as the far end. Each target
is one of:
  native   port 0 on the host's i40e, in the namespace vnet-native
  guest    port 0 driven by a guest under the launcher through VFIO and
           exported to the host's virtio_net

The default runs both. Both ports have an MTU of 9000. Each job runs -reps
times, and each throughput job for -time seconds. A job's CPU use is the
number of the host's CPUs that were busy, and with the guest, the share of
them that its UML process took and how busy the guest's own CPU was. Build
the guest with scripts/invirtio-build-guest.sh -t net-bench. The output
directory gets results.json and the summary, which bench-net-summary prints
again. Run as root on the linux-host kernel, with iperf3, netperf and
ethtool.

Jobs:`+netBenchJobList())
	build := fs.String("guest", filepath.Join(root, "linux-uml/build-net-bench"), "the build directory of the guest")
	out := fs.String("out", filepath.Join(root, "build/bench-net"), "the output directory")
	reps := fs.Int("reps", 3, "the repetitions of each job")
	seconds := fs.Int("time", 10, "the seconds that each throughput job runs")
	jobs := fs.String("jobs", "", "run only the jobs whose names contain this string")
	fs.Parse(args)
	targets := fs.Args()
	if len(targets) == 0 {
		targets = []string{"native", "guest"}
	}
	for _, t := range targets {
		if t != "native" && t != "guest" {
			fs.Usage()
			os.Exit(2)
		}
	}
	requireRoot()

	s := &netBenchSuite{build: *build, reps: *reps, seconds: *seconds}
	for _, j := range netBenchJobs {
		if strings.Contains(j.name, *jobs) {
			s.jobs = append(s.jobs, j)
		}
	}
	var err error
	if s.out, err = filepath.Abs(*out); err != nil {
		return err
	}
	if err := os.MkdirAll(s.out, 0755); err != nil {
		return err
	}
	if err := harness.TeeOutput(filepath.Join(s.out, "bench.log")); err != nil {
		return err
	}
	for _, tool := range []string{"iperf3", "netperf", "netserver", "ethtool", "vdpa", "ping"} {
		if harness.Run("sh", "-c", "command -v "+tool).Code != 0 {
			return fmt.Errorf("%s is not installed", tool)
		}
	}
	if err := harness.Modprobe("vduse", "vdpa", "virtio_vdpa", "virtio_net", "vfio-pci", "i40e"); err != nil {
		return err
	}
	if vdpa.Exists(netBenchName) {
		return fmt.Errorf("/dev/vduse/%s exists, perhaps from an earlier run, so remove it first", netBenchName)
	}
	if pids := harness.PCIHolders(netGuestPort); len(pids) > 0 {
		return fmt.Errorf("%s is in use by process %d, stop its guest first", netGuestPort, pids[0])
	}
	s.portDriver = harness.PCIDriver(netGuestPort)
	s.results.Host = netBenchHostInfo(s)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer cancel()
	s.ctx = ctx
	if s.far, err = netStartPeer(netPeerAddr, "iperf3", "netserver"); err != nil {
		return err
	}
	defer s.cleanup()

	for _, name := range targets {
		if ctx.Err() != nil {
			return errors.New("stopped by a signal")
		}
		fmt.Printf("== %s\n", name)
		t := NetBenchTarget{Name: name}
		if name == "native" {
			err = s.runNative(&t)
		} else {
			err = s.runGuest(&t)
		}
		if err != nil {
			return fmt.Errorf("%s: %v", name, err)
		}
		s.results.Targets = append(s.results.Targets, t)
		if err := s.save(); err != nil {
			return err
		}
	}
	summary := netBenchSummary(&s.results)
	fmt.Print(summary)
	return os.WriteFile(filepath.Join(s.out, "summary.txt"), []byte(summary), 0644)
}

func netBenchJobList() string {
	var b strings.Builder
	for _, j := range netBenchJobs {
		fmt.Fprintf(&b, "\n  %-10s %s", j.name, j.about)
	}
	return b.String()
}

func netBenchHostInfo(s *netBenchSuite) map[string]string {
	h := map[string]string{
		"uname":   strings.TrimSpace(harness.Run("uname", "-a").Stdout),
		"clk_tck": strings.TrimSpace(harness.Run("getconf", "CLK_TCK").Stdout),
		"reps":    strconv.Itoa(s.reps),
		"time":    strconv.Itoa(s.seconds),
		"iperf3":  strings.SplitN(harness.Run("iperf3", "--version").Stdout, "\n", 2)[0],
	}
	uml := filepath.Join(harness.RepoRoot(), "linux-uml/linux")
	h["linux-uml"] = strings.TrimSpace(harness.Run("git", "-C", uml, "log", "-1", "--format=%h %s").Stdout)
	if !harness.Run("git", "-C", uml, "diff", "--quiet", "HEAD").OK() {
		h["linux-uml"] += " (with uncommitted changes)"
	}
	return h
}

func (s *netBenchSuite) save() error {
	data, err := json.MarshalIndent(&s.results, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.out, "results.json"), append(data, '\n'), 0644)
}

func (s *netBenchSuite) cleanup() {
	harness.Cleanup(netBenchName)
	harness.Run("ip", "netns", "del", netNativeNS)
	s.far.stop()
	netRestorePort(s.portDriver)
}

// carrierIn waits for an interface in a namespace to have carrier.
func carrierIn(ns, net string) bool {
	return harness.Wait(func() bool {
		return strings.TrimSpace(harness.RunArgv(harness.InNS(ns, "cat", "/sys/class/net/"+net+"/carrier")).Stdout) == "1"
	}, 30*time.Second, 200*time.Millisecond)
}

// runNative runs the jobs with port 0 on the host's i40e, in a namespace of
// its own.
func (s *netBenchSuite) runNative(t *NetBenchTarget) error {
	if harness.PCIDriver(netGuestPort) != "i40e" {
		if err := pci.BindNative(netGuestPort); err != nil {
			return err
		}
	}
	port := harness.WaitNetdev(netGuestPort, 10*time.Second)
	if port == "" {
		return fmt.Errorf("%s has no interface on the host", netGuestPort)
	}
	harness.Release(port)
	harness.Run("ip", "netns", "del", netNativeNS)
	defer func() {
		harness.RunArgv(harness.InNS(netNativeNS, "ip", "link", "set", port, "netns", "1"))
		harness.Run("ip", "netns", "del", netNativeNS)
	}()
	for _, argv := range [][]string{
		{"ip", "netns", "add", netNativeNS},
		{"ip", "link", "set", port, "netns", netNativeNS},
		harness.InNS(netNativeNS, "ip", "link", "set", port, "mtu", fmt.Sprint(netMTU), "up"),
		harness.InNS(netNativeNS, "ip", "addr", "add", netHostAddr+"/24", "dev", port),
	} {
		if err := harness.MustRun(argv[0], argv[1:]...); err != nil {
			return err
		}
	}
	if !carrierIn(netNativeNS, port) {
		return fmt.Errorf("%s has no carrier", port)
	}
	return s.runJobs(t, netNativeNS)
}

// runGuest runs the jobs through a guest that drives port 0 and exports it.
func (s *netBenchSuite) runGuest(t *NetBenchTarget) error {
	if net := harness.PCINetdev(netGuestPort); net != "" {
		harness.Release(net)
	}
	logDir, err := harness.MakeLogDir(netBenchName)
	if err != nil {
		return err
	}
	defer os.RemoveAll(logDir)
	g, err := harness.LaunchGuest(filepath.Join(s.out, "guest.log"), harness.Launch{
		Build:   s.build,
		Type:    "net",
		Name:    netBenchName,
		Options: fmt.Sprintf("netdev=eth0,mtu=%d", netMTU),
		LogDir:  logDir,
		Devices: []string{netGuestPort},
		Args:    []string{"mem=512M"},
	})
	if err != nil {
		return err
	}
	defer func() {
		harness.Detach(netBenchName, !g.Running())
		g.Stop(30 * time.Second)
		g.Kill()
		harness.Cleanup(netBenchName)
		s.guest, s.uml = nil, 0
	}()
	if !g.WaitFor(netBenchName+": created", 2*time.Minute) {
		return errors.New("the guest did not create its device, see guest.log")
	}
	if err := harness.Attach(netBenchName); err != nil {
		return err
	}
	var net string
	harness.Wait(func() bool {
		nets := harness.VirtioNetdevs(netBenchName)
		if len(nets) > 0 {
			net = nets[0]
		}
		return net != ""
	}, 30*time.Second, 100*time.Millisecond)
	if net == "" {
		return errors.New("the host got no interface")
	}
	harness.Release(net)
	harness.PlaceIRQs(netBenchName, -1)
	for _, argv := range [][]string{
		{"ip", "link", "set", net, "mtu", fmt.Sprint(netMTU), "up"},
		{"ip", "addr", "replace", netHostAddr + "/24", "dev", net},
	} {
		if err := harness.MustRun(argv[0], argv[1:]...); err != nil {
			return err
		}
	}
	if !carrierIn("", net) {
		return fmt.Errorf("%s has no carrier", net)
	}
	s.guest, s.uml = g, harness.UMLPid(g.Pid())
	if s.uml == 0 {
		return errors.New("cannot find the guest's UML process")
	}
	if !strings.Contains(g.Output("echo shell-ready", 30*time.Second), "shell-ready") {
		return errors.New("the guest's shell does not answer, see guest.log")
	}
	if err := s.runJobs(t, ""); err != nil {
		return err
	}
	t.Counters = g.Counters()
	return nil
}

// A cpuMeter measures the host's busy CPUs over a job, and the CPUs that a
// guest's UML process took.
type cpuMeter struct {
	busy, total int64
	uml         int
	umlTicks    [2]int64
	start       time.Time
}

func startCPUMeter(uml int) cpuMeter {
	m := cpuMeter{uml: uml, start: time.Now()}
	m.busy, m.total = harness.HostCPU()
	if uml > 0 {
		m.umlTicks = harness.ProcessTicks(uml)
	}
	return m
}

// stop returns how many of the host's CPUs were busy, and how many the UML
// process took.
func (m cpuMeter) stop() (host, uml float64) {
	busy, total := harness.HostCPU()
	if total > m.total {
		host = float64(busy-m.busy) / float64(total-m.total) * float64(harness.OnlineCPUs())
	}
	if m.uml > 0 {
		t := harness.ProcessTicks(m.uml)
		uml = float64(t[0]+t[1]-m.umlTicks[0]-m.umlTicks[1]) / clockTicks / time.Since(m.start).Seconds()
	}
	return host, uml
}

// clockTicks is the rate of the ticks of /proc/stat.
var clockTicks = func() float64 {
	hz, err := strconv.ParseFloat(strings.TrimSpace(harness.Run("getconf", "CLK_TCK").Stdout), 64)
	if err != nil || hz == 0 {
		return 100
	}
	return hz
}()

// guestCPU returns the busy and total ticks of the guest's CPU.
func (s *netBenchSuite) guestCPU() (int64, int64) {
	return harness.ParseCPULine(strings.TrimSpace(s.guest.Output("head -1 /proc/stat", 10*time.Second)))
}

// runJobs runs each job -reps times from the namespace ns, with the CPU
// use of each run.
func (s *netBenchSuite) runJobs(t *NetBenchTarget, ns string) error {
	for _, j := range s.jobs {
		for rep := 1; rep <= s.reps; rep++ {
			if s.ctx.Err() != nil {
				return errors.New("stopped by a signal")
			}
			run := NetBenchRun{Job: j.name, Rep: rep}
			var gbusy0, gtotal0 int64
			if s.guest != nil {
				gbusy0, gtotal0 = s.guestCPU()
			}
			meter := startCPUMeter(s.uml)
			m, err := j.run(s, ns)
			run.HostCPUs, run.UMLCPUs = meter.stop()
			if s.guest != nil {
				gbusy1, gtotal1 := s.guestCPU()
				if gtotal1 > gtotal0 && gtotal0 > 0 {
					run.GuestBusy = float64(gbusy1-gbusy0) / float64(gtotal1-gtotal0)
				}
				run.HasGuest = true
			}
			if err != nil {
				return fmt.Errorf("%s: %v", j.name, err)
			}
			run.Metrics = m
			fmt.Printf("  %s %d: %s\n", j.name, rep, netBenchFormat(j, run))
			t.Runs = append(t.Runs, run)
		}
	}
	return nil
}

func netBenchFormat(j netBenchJob, run NetBenchRun) string {
	var parts []string
	for _, k := range j.metrics {
		parts = append(parts, fmt.Sprintf("%s %.2f", k, run.Metrics[k]))
	}
	parts = append(parts, fmt.Sprintf("host_cpus %.2f", run.HostCPUs))
	if run.HasGuest {
		parts = append(parts, fmt.Sprintf("uml_cpus %.2f", run.UMLCPUs), fmt.Sprintf("guest_busy %.2f", run.GuestBusy))
	}
	return strings.Join(parts, ", ")
}

// run runs a command in the namespace ns, which a signal stops.
func (s *netBenchSuite) run(ns string, timeout time.Duration, argv ...string) harness.Result {
	argv = harness.InNS(ns, argv...)
	return harness.RunContext(s.ctx, s.out, timeout, argv[0], argv[1:]...)
}

func (s *netBenchSuite) iperf(ns string, args ...string) (harness.Iperf, error) {
	argv := append([]string{"iperf3", "-c", netPeerAddr, "-J", "-t", fmt.Sprint(s.seconds)}, args...)
	return harness.ParseIperf(s.run(ns, time.Duration(s.seconds+60)*time.Second, argv...).Stdout)
}

func netBenchTCP(reverse bool, streams int) func(*netBenchSuite, string) (map[string]float64, error) {
	return func(s *netBenchSuite, ns string) (map[string]float64, error) {
		args := []string{"-P", fmt.Sprint(streams)}
		if reverse {
			args = append(args, "-R")
		}
		r, err := s.iperf(ns, args...)
		if err != nil {
			return nil, err
		}
		return map[string]float64{"gbps": r.ReceivedBPS / 1e9, "retransmits": float64(r.Retransmits)}, nil
	}
}

func netBenchUDP(reverse bool) func(*netBenchSuite, string) (map[string]float64, error) {
	return func(s *netBenchSuite, ns string) (map[string]float64, error) {
		args := []string{"-u", "-b", "0", "-l", "64"}
		if reverse {
			args = append(args, "-R")
		}
		r, err := s.iperf(ns, args...)
		if err != nil {
			return nil, err
		}
		if r.Seconds <= 0 || r.Packets <= 0 {
			return nil, errors.New("iperf3 reported no datagrams")
		}
		return map[string]float64{
			"sent_kpps":     float64(r.Packets) / r.Seconds / 1e3,
			"received_kpps": float64(r.Packets-r.LostPackets) / r.Seconds / 1e3,
			"loss_pct":      100 * float64(r.LostPackets) / float64(r.Packets),
		}, nil
	}
}

func netBenchRR(test string) func(*netBenchSuite, string) (map[string]float64, error) {
	return func(s *netBenchSuite, ns string) (map[string]float64, error) {
		r := s.run(ns, time.Duration(s.seconds+60)*time.Second, "netperf", "-H", netPeerAddr, "-t", test,
			"-l", fmt.Sprint(s.seconds), "-P", "0", "--", "-r", "1,1", "-o", "THROUGHPUT,MEAN_LATENCY,P99_LATENCY")
		return parseNetperf(r.Stdout, r.Output())
	}
}

// parseNetperf parses the last line of netperf's -o output.
func parseNetperf(stdout, all string) (map[string]float64, error) {
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	f := strings.Split(lines[len(lines)-1], ",")
	if len(f) != 3 {
		return nil, fmt.Errorf("netperf printed %q", strings.TrimSpace(all))
	}
	var v [3]float64
	for i := range f {
		var err error
		if v[i], err = strconv.ParseFloat(strings.TrimSpace(f[i]), 64); err != nil {
			return nil, fmt.Errorf("netperf printed %q", strings.TrimSpace(all))
		}
	}
	return map[string]float64{"tps": v[0], "mean_us": v[1], "p99_us": v[2]}, nil
}

func netBenchPing(load bool) func(*netBenchSuite, string) (map[string]float64, error) {
	return func(s *netBenchSuite, ns string) (map[string]float64, error) {
		if load {
			ctx, cancel := context.WithCancel(s.ctx)
			done := make(chan struct{})
			go func() {
				argv := harness.InNS(ns, "iperf3", "-c", netPeerAddr, "-t", "60")
				harness.RunContext(ctx, s.out, 2*time.Minute, argv[0], argv[1:]...)
				close(done)
			}()
			defer func() {
				cancel()
				<-done
			}()
			time.Sleep(2 * time.Second)
		}
		r := s.run(ns, time.Minute, "ping", "-q", "-c", "100", "-i", "0.05", netPeerAddr)
		p, ok := harness.ParsePing(r.Output())
		if !ok || p.Received == 0 {
			return nil, fmt.Errorf("ping printed %q", strings.TrimSpace(r.Output()))
		}
		return map[string]float64{"avg_us": p.Avg * 1000, "max_us": p.Max * 1000, "mdev_us": p.Mdev * 1000}, nil
	}
}

func cmdBenchNetSummary(args []string) error {
	fs := newFlags("bench-net-summary", `
usage: invirtio-test bench-net-summary <output dir>

Print the summary of a bench-net run from its results.json.`)
	fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		os.Exit(2)
	}
	data, err := os.ReadFile(filepath.Join(fs.Arg(0), "results.json"))
	if err != nil {
		return err
	}
	var r NetBenchResults
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}
	fmt.Print(netBenchSummary(&r))
	return nil
}

// netBenchSummary prints, for each job and target, the medians of the
// job's metrics and CPU use over the repetitions.
func netBenchSummary(r *NetBenchResults) string {
	var b strings.Builder
	for _, j := range netBenchJobs {
		var rows []string
		for _, t := range r.Targets {
			metrics := map[string][]float64{}
			var host, uml, guest []float64
			hasGuest := false
			for _, run := range t.Runs {
				if run.Job != j.name {
					continue
				}
				for _, k := range j.metrics {
					metrics[k] = append(metrics[k], run.Metrics[k])
				}
				host = append(host, run.HostCPUs)
				uml = append(uml, run.UMLCPUs)
				guest = append(guest, run.GuestBusy)
				hasGuest = hasGuest || run.HasGuest
			}
			if len(host) == 0 {
				continue
			}
			row := fmt.Sprintf("  %-8s", t.Name)
			for _, k := range j.metrics {
				row += fmt.Sprintf(" %13.2f", benchMedian(metrics[k]))
			}
			row += fmt.Sprintf(" %9.2f", benchMedian(host))
			if hasGuest {
				row += fmt.Sprintf(" %8.2f %10.2f", benchMedian(uml), benchMedian(guest))
			}
			rows = append(rows, row)
		}
		if len(rows) == 0 {
			continue
		}
		fmt.Fprintf(&b, "%s: %s\n  %-8s", j.name, j.about, "target")
		for _, k := range j.metrics {
			fmt.Fprintf(&b, " %13s", k)
		}
		fmt.Fprintf(&b, " %9s %8s %10s\n%s\n\n", "host_cpus", "uml_cpus", "guest_busy", strings.Join(rows, "\n"))
	}
	b.WriteString("Each value is the median over the repetitions. host_cpus is how many of\n" +
		"the host's CPUs were busy, uml_cpus the guest's UML process's share of\n" +
		"them, and guest_busy how busy the guest's own CPU was.\n")
	return b.String()
}
