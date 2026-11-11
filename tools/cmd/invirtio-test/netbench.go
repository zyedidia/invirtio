package main

import (
	"context"
	"errors"
	"fmt"
	"os"
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
	register("bench-net", "benchmark the VDUSE virtio-net backend against the host's i40e", cmdBenchNet)
	register("bench-net-summary", "print the results of bench-net again", cmdBenchNetSummary)
}

const (
	netBenchName = "vnet-bench"
	// The namespace of the tested interface, on the host's i40e or
	// virtio_net.
	netBenchNS  = "vnet-bench"
	netHostAddr = "10.71.0.1"
	netPeerAddr = "10.71.0.2"
)

var netBenchTargets = []string{"native", "native-1q", "guest"}

// A netBenchJob is one measurement, run against every target. Omits is set
// for the jobs that leave out their first -omit seconds.
type netBenchJob struct {
	bench.Job
	run   func(s *netBenchSuite, ns string) (map[string]float64, error)
	omits bool
}

var netBenchJobs = []netBenchJob{
	{bench.Job{Name: "tcp-tx-1", About: "TCP to the far end, one stream", Metrics: []string{"gbps", "retransmits"}}, netBenchTCP(false, 1), true},
	{bench.Job{Name: "tcp-tx-4", About: "TCP to the far end, four streams", Metrics: []string{"gbps", "retransmits"}}, netBenchTCP(false, 4), true},
	{bench.Job{Name: "tcp-rx-1", About: "TCP from the far end, one stream", Metrics: []string{"gbps", "retransmits"}}, netBenchTCP(true, 1), true},
	{bench.Job{Name: "tcp-rx-4", About: "TCP from the far end, four streams", Metrics: []string{"gbps", "retransmits"}}, netBenchTCP(true, 4), true},
	{bench.Job{Name: "udp64-tx", About: "64-byte UDP datagrams to the far end at an unlimited rate", Metrics: []string{"sent_kpps", "received_kpps", "loss_pct"}}, netBenchUDP(false), true},
	{bench.Job{Name: "udp64-rx", About: "64-byte UDP datagrams from the far end at an unlimited rate", Metrics: []string{"sent_kpps", "received_kpps", "loss_pct"}}, netBenchUDP(true), true},
	{bench.Job{Name: "tcp-rr", About: "netperf TCP_RR with 1-byte messages", Metrics: []string{"tps", "mean_us", "p99_us"}}, netBenchRR("TCP_RR"), false},
	{bench.Job{Name: "udp-rr", About: "netperf UDP_RR with 1-byte messages", Metrics: []string{"tps", "mean_us", "p99_us"}}, netBenchRR("UDP_RR"), false},
	{bench.Job{Name: "ping-idle", About: "ping round trips on an idle link", Metrics: []string{"avg_us", "max_us", "mdev_us"}}, netBenchPing(false), false},
	{bench.Job{Name: "ping-load", About: "ping round trips during a TCP stream", Metrics: []string{"avg_us", "max_us", "mdev_us"}}, netBenchPing(true), false},
}

type netBenchSuite struct {
	out string
	mem string
	// guestArgs are added to the guest's command line, and guestSetup
	// runs in its shell once it boots.
	guestArgs  []string
	guestSetup string
	// perfJob selects the jobs that are profiled, or is "". Their
	// profiles are reported once the guests are gone.
	perfJob  string
	profiles []*bench.Profile
	build    string
	mtu      int
	seconds  int
	omit     int
	opts     *bench.Options
	jobs     []netBenchJob
	far      *netFarEnd
	// portDriver is port 0's driver before the run, which it restores.
	portDriver string
	results    bench.Results
	// The guest of the guest target and its cgroup, whose CPU use and
	// memory are measured.
	guest  *harness.Guest
	cgroup string
	memory *harness.GuestMemory
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
  native     port 0 on the host's i40e, with the queues that i40e chooses
             and their interrupts spread in turn over the CPUs outside
             the cores of -app-cpus and -peer-cpus
  native-1q  the same with one queue pair, as the guest's i40e has, and its
             interrupts on the device CPU
  guest      port 0 driven by the invirtio guest under the launcher through
             VFIO and exported to the host's virtio_net

Each target's interface is in the namespace vnet-bench, where the jobs run.

The default runs all three. Each round brings up every target once and runs
its jobs -reps times, and every other round runs the targets in the reverse
order. Both ports have an MTU of -mtu. Each throughput job runs for -time
seconds after -omit seconds that it leaves out.

The benchmark's programs run on -app-cpus, and the far end's programs and
port 1's interrupts on -peer-cpus. With native-1q and the guest, the
device's interrupts go to -device-cpu, as do the guest's CPU, its helper
threads and its VDUSE interrupts. Each run records how many of the host's
CPUs were busy and how many of each set's, and with the guest, how many
CPUs the guest's cgroup took, how busy the guest's own CPU was, and how
many DMA buffers it mapped each second. During the run, every CPU uses the
performance governor, boost is as -boost says, and irqbalance is stopped.
A summary's values are means over the rounds, since each round brings a
target up again. -guest-args adds to the guest's command line, so that
vfio_uml.eager_dma=1 maps all of its RAM for DMA once.

The guest runs with -mem in a cgroup of its own, as invirtio's guests do,
and the results record the cgroup's peak memory and the most memory that
the guest itself used between jobs. Build the guest with
scripts/invirtio-build-guest.sh. -guest-setup runs shell commands in the
guest once it boots, and fails the run if they do not finish. With -perf,
perf records the device CPU, or every CPU without pinning, during each run
of the matching jobs, after the omitted seconds of iperf3, into
perf-<job>-<target>-<round>-<rep>.data in the output directory. Its
messages go to .data.log. Once the bench ends and the guests have exited,
which perf needs to find their symbols, reports by symbol go to .data.txt
and by command and object to .data.dso.txt. The output directory gets
results.json, runs.csv with one metric of one run on each line, and the
summary, which bench-net-summary prints again. Run as root on the
linux-host kernel, with iperf3, netperf and ethtool, and perf for -perf.

Jobs:`+netBenchJobList())
	build := fs.String("guest", filepath.Join(root, "linux-uml/build-invirtio"), "the build directory of the guest")
	out := fs.String("out", filepath.Join(root, "build/bench-net"), "the output directory")
	seconds := fs.Int("time", 10, "the seconds that each throughput job measures")
	omit := fs.Int("omit", 2, "the seconds at the start of each iperf3 run that it leaves out")
	mtu := fs.Int("mtu", 1500, "the MTU of both ports")
	mem := fs.String("mem", "256M", "the guest's memory, as UML's mem= takes it")
	guestArgs := fs.String("guest-args", "", "more arguments for the guest's command line, such as vfio_uml.eager_dma=1")
	guestSetup := fs.String("guest-setup", "", "shell commands to run in the guest once it boots")
	perfJob := fs.String("perf", "", "profile the device CPU with perf during the jobs whose names contain this string")
	jobs := fs.String("jobs", "", "run only the jobs whose names contain this string")
	opts := bench.AddFlags(fs, 5, 1)
	fs.Parse(args)
	targets := fs.Args()
	if len(targets) == 0 {
		targets = netBenchTargets
	}
	for _, t := range targets {
		if !slices.Contains(netBenchTargets, t) {
			fs.Usage()
			os.Exit(2)
		}
	}
	if err := opts.Check(); err != nil {
		return err
	}
	requireRoot()

	s := &netBenchSuite{build: *build, seconds: *seconds, omit: *omit, mtu: *mtu, mem: *mem, opts: opts,
		guestArgs: strings.Fields(*guestArgs), guestSetup: *guestSetup, perfJob: *perfJob}
	for _, j := range netBenchJobs {
		if strings.Contains(j.Name, *jobs) {
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
	tools := []string{"iperf3", "netperf", "netserver", "ethtool", "vdpa", "ping", "taskset"}
	if s.perfJob != "" {
		tools = append(tools, "perf")
	}
	for _, tool := range tools {
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
	if s.far, err = netStartPeer(netPeerAddr, s.mtu, opts.PeerCPUs, "iperf3", "netserver"); err != nil {
		return err
	}
	defer s.cleanup()
	defer s.report()

	for round := 1; round <= opts.Rounds; round++ {
		for _, name := range bench.Order(targets, round) {
			if ctx.Err() != nil {
				return errors.New("stopped by a signal")
			}
			fmt.Printf("== round %d: %s\n", round, name)
			in := bench.Instance{Target: name, Round: round}
			if name == "guest" {
				err = s.runGuest(&in)
			} else {
				err = s.runNative(&in, name == "native-1q")
			}
			if err != nil {
				return fmt.Errorf("round %d: %s: %v", round, name, err)
			}
			s.results.Instances = append(s.results.Instances, in)
			if err := s.results.Save(s.out); err != nil {
				return err
			}
		}
	}
	summary := s.results.Summary(netBenchJobDefs())
	fmt.Print(summary)
	return os.WriteFile(filepath.Join(s.out, "summary.txt"), []byte(summary), 0644)
}

func netBenchJobList() string {
	var b strings.Builder
	for _, j := range netBenchJobs {
		fmt.Fprintf(&b, "\n  %-10s %s", j.Name, j.About)
	}
	return b.String()
}

func netBenchJobDefs() []bench.Job {
	var jobs []bench.Job
	for _, j := range netBenchJobs {
		jobs = append(jobs, j.Job)
	}
	return jobs
}

func (s *netBenchSuite) env() map[string]string {
	env := bench.Environment()
	for k, v := range s.opts.Describe() {
		env[k] = v
	}
	env["time"] = strconv.Itoa(s.seconds)
	env["omit"] = strconv.Itoa(s.omit)
	env["mtu"] = strconv.Itoa(s.mtu)
	env["mem"] = s.mem
	env["guest_build"] = s.build
	env["guest_args"] = strings.Join(s.guestArgs, " ")
	env["guest_setup"] = s.guestSetup
	env["perf"] = s.perfJob
	env["iperf3"] = strings.SplitN(harness.Run("iperf3", "--version").Stdout, "\n", 2)[0]
	env["netperf"] = strings.TrimSpace(harness.Run("netperf", "-V").Stdout)
	env["nic"] = strings.TrimSpace(harness.Run("lspci", "-s", netGuestPort).Stdout)
	return env
}

// report writes the reports of the profiles, once every guest has exited.
func (s *netBenchSuite) report() {
	for _, p := range s.profiles {
		if err := p.Report(); err != nil {
			fmt.Println("warning:", err)
		}
	}
}

func (s *netBenchSuite) cleanup() {
	harness.Cleanup(netBenchName)
	harness.Run("ip", "netns", "del", netBenchNS)
	s.far.stop()
	netRestorePort(s.portDriver)
}

// carrierIn waits for an interface in a namespace to have carrier.
func carrierIn(ns, net string) bool {
	return harness.Wait(func() bool {
		return strings.TrimSpace(harness.RunArgv(harness.InNS(ns, "cat", "/sys/class/net/"+net+"/carrier")).Stdout) == "1"
	}, 30*time.Second, 200*time.Millisecond)
}

// netInfo describes an interface's driver, queues, rings and offloads.
func netInfo(ns, net string) map[string]string {
	info := map[string]string{}
	for _, opt := range []string{"-i", "-l", "-g", "-k"} {
		info["ethtool "+opt] = strings.TrimSpace(harness.RunArgv(harness.InNS(ns, "ethtool", opt, net)).Stdout)
	}
	return info
}

var ethtoolCombinedRE = regexp.MustCompile(`(?s)Current hardware settings:.*?Combined:\s*(\d+)`)

// enterNS moves an interface into netBenchNS and brings it up with the
// host's address, and returns a function that moves it back and removes
// the namespace.
func (s *netBenchSuite) enterNS(net string) (func(), error) {
	harness.Run("ip", "netns", "del", netBenchNS)
	leave := func() {
		harness.RunArgv(harness.InNS(netBenchNS, "ip", "link", "set", net, "netns", "1"))
		harness.Run("ip", "netns", "del", netBenchNS)
	}
	for _, argv := range [][]string{
		{"ip", "netns", "add", netBenchNS},
		{"ip", "link", "set", net, "netns", netBenchNS},
		harness.InNS(netBenchNS, "ip", "link", "set", net, "mtu", fmt.Sprint(s.mtu), "up"),
		harness.InNS(netBenchNS, "ip", "addr", "add", netHostAddr+"/24", "dev", net),
	} {
		if err := harness.MustRun(argv[0], argv[1:]...); err != nil {
			leave()
			return nil, err
		}
	}
	return leave, nil
}

// runNative runs the jobs with port 0 on a fresh binding of the host's
// i40e, in netBenchNS. With oneQueue, the port has one queue pair, and its
// interrupts go to the device CPU, as the guest's do.
func (s *netBenchSuite) runNative(in *bench.Instance, oneQueue bool) error {
	if err := pci.BindNative(netGuestPort); err != nil {
		return err
	}
	port := harness.WaitNetdev(netGuestPort, 10*time.Second)
	if port == "" {
		return fmt.Errorf("%s has no interface on the host", netGuestPort)
	}
	harness.Release(port)
	leave, err := s.enterNS(port)
	if err != nil {
		return err
	}
	defer leave()
	ns := func(argv ...string) []string { return harness.InNS(netBenchNS, argv...) }
	if oneQueue {
		m := ethtoolCombinedRE.FindStringSubmatch(harness.RunArgv(ns("ethtool", "-l", port)).Stdout)
		if m == nil {
			return fmt.Errorf("ethtool shows no combined queues for %s", port)
		}
		argv := ns("ethtool", "-L", port, "combined", "1")
		if err := harness.MustRun(argv[0], argv[1:]...); err != nil {
			return err
		}
		defer harness.RunArgv(ns("ethtool", "-L", port, "combined", m[1]))
	}
	// The port's interrupts stay off the cores of the benchmark's programs
	// and the far end, as the guest's do.
	if other := s.opts.OtherCPUs(); other != "" {
		cpus := strings.Split(other, ",")
		if oneQueue {
			cpus = []string{s.opts.DeviceCPU}
		}
		restore, err := bench.SteerIRQs(netGuestPort, cpus...)
		if err != nil {
			return err
		}
		defer restore()
	}
	if !carrierIn(netBenchNS, port) {
		return fmt.Errorf("%s has no carrier", port)
	}
	in.Info = netInfo(netBenchNS, port)
	return s.runJobs(in, netBenchNS)
}

// runGuest runs the jobs through a guest that drives port 0 and exports it.
func (s *netBenchSuite) runGuest(in *bench.Instance) error {
	if net := harness.PCINetdev(netGuestPort); net != "" {
		harness.Release(net)
	}
	logDir, err := harness.MakeLogDir(netBenchName)
	if err != nil {
		return err
	}
	defer os.RemoveAll(logDir)
	// The guest runs in a cgroup of its own, as invirtio's guests do, which
	// records its peak memory and its CPU time.
	cg, err := harness.MakeCgroup(netBenchName)
	if err != nil {
		return err
	}
	defer harness.RemoveCgroup(cg)
	l := harness.Launch{
		Build:   s.build,
		Type:    "net",
		Name:    netBenchName,
		Options: fmt.Sprintf("netdev=eth0,mtu=%d", s.mtu),
		LogDir:  logDir,
		Devices: []string{netGuestPort},
		Args:    append([]string{"mem=" + s.mem}, s.guestArgs...),
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
		harness.Detach(netBenchName, !g.Running())
		g.Stop(30 * time.Second)
		g.Kill()
		harness.Cleanup(netBenchName)
		s.guest, s.cgroup, s.memory = nil, "", nil
	}()
	if !g.WaitFor(netBenchName+": created", 2*time.Minute) {
		return errors.New("the guest did not create its device, see " + g.Path)
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
	cpu := -1
	if s.opts.DeviceCPU != "" {
		cpu, _ = strconv.Atoi(s.opts.DeviceCPU)
	}
	if err := harness.PlaceIRQs(netBenchName, cpu); err != nil {
		return err
	}
	leave, err := s.enterNS(net)
	if err != nil {
		return err
	}
	defer leave()
	if !carrierIn(netBenchNS, net) {
		return fmt.Errorf("%s has no carrier", net)
	}
	if !strings.Contains(g.Output("echo shell-ready", 30*time.Second), "shell-ready") {
		return errors.New("the guest's shell does not answer, see " + g.Path)
	}
	if s.guestSetup != "" {
		const done = "guest-setup-done"
		// A subshell takes commands that end in a separator.
		out := g.Output("("+s.guestSetup+"); echo "+done, time.Minute)
		if !strings.Contains(out, done) {
			return errors.New("the guest setup did not finish, see " + g.Path)
		}
		fmt.Print(strings.Replace(out, done+"\n", "", 1))
	}
	in.Info = netInfo(netBenchNS, net)
	s.guest, s.cgroup = g, cg
	s.memory = &harness.GuestMemory{Mem: s.mem}
	s.memory.SampleGuest(g.Output("cat /proc/meminfo", 10*time.Second))
	if err := s.runJobs(in, netBenchNS); err != nil {
		return err
	}
	in.Counters = g.Counters()
	for k, v := range g.DMACounters() {
		in.Counters["dma_"+k] = v
	}
	if err := s.memory.ReadCgroup(cg); err != nil {
		return err
	}
	in.Memory = s.memory
	fmt.Println("  memory: " + in.Memory.Summary())
	return nil
}

// guestCPU returns the busy and total ticks of the guest's CPU.
func (s *netBenchSuite) guestCPU() (int64, int64) {
	return harness.ParseCPULine(strings.TrimSpace(s.guest.Output("head -1 /proc/stat", 10*time.Second)))
}

// runJobs runs each job -reps times from the namespace ns, with the CPU
// use of each run and the guest's DMA mappings.
func (s *netBenchSuite) runJobs(in *bench.Instance, ns string) error {
	for _, j := range s.jobs {
		for rep := 1; rep <= s.opts.Reps; rep++ {
			if s.ctx.Err() != nil {
				return errors.New("stopped by a signal")
			}
			var gbusy0, gtotal0 int64
			var dma0 map[string]int64
			if s.guest != nil {
				dma0 = s.guest.DMACounters()
				gbusy0, gtotal0 = s.guestCPU()
			}
			var prof *bench.Profile
			if s.perfJob != "" && strings.Contains(j.Name, s.perfJob) {
				data := filepath.Join(s.out, fmt.Sprintf("perf-%s-%s-%d-%d.data", j.Name, in.Target, in.Round, rep))
				var delay time.Duration
				if j.omits {
					delay = time.Duration(s.omit) * time.Second
				}
				var err error
				prof, err = bench.StartProfile(s.ctx, s.opts.DeviceCPU, data, delay,
					time.Duration(s.seconds+60)*time.Second)
				if err != nil {
					return fmt.Errorf("%s: %v", j.Name, err)
				}
			}
			start := time.Now()
			meter := bench.StartMeter(s.opts.Sets(), s.cgroup)
			m, err := j.run(s, ns)
			cpu := meter.Stop()
			elapsed := time.Since(start)
			if prof != nil {
				if perr := prof.Stop(); perr != nil && err == nil {
					err = perr
				}
				s.profiles = append(s.profiles, prof)
			}
			if err != nil {
				return fmt.Errorf("%s: %v", j.Name, err)
			}
			for k, v := range cpu {
				m[k] = v
			}
			if s.guest != nil {
				gbusy1, gtotal1 := s.guestCPU()
				if gtotal1 > gtotal0 && gtotal0 > 0 {
					m["guest_busy"] = float64(gbusy1-gbusy0) / float64(gtotal1-gtotal0)
				}
				for k, v := range bench.DMARates(dma0, s.guest.DMACounters(), elapsed) {
					m[k] = v
				}
				s.memory.SampleGuest(s.guest.Output("cat /proc/meminfo", 10*time.Second))
			}
			fmt.Printf("  %s %d: %s\n", j.Name, rep, bench.Format(j.Job, m))
			s.results.Runs = append(s.results.Runs, bench.Run{Job: j.Name, Target: in.Target, Round: in.Round, Rep: rep, Metrics: m})
		}
	}
	return nil
}

// run runs a command on the application CPUs in the namespace ns, which a
// signal stops.
func (s *netBenchSuite) run(ns string, timeout time.Duration, argv ...string) harness.Result {
	argv = harness.InNS(ns, bench.Pin(s.opts.AppCPUs, argv...)...)
	return harness.RunContext(s.ctx, s.out, timeout, argv[0], argv[1:]...)
}

func (s *netBenchSuite) iperf(ns string, args ...string) (harness.Iperf, error) {
	argv := append([]string{"iperf3", "-c", netPeerAddr, "-J", "-t", fmt.Sprint(s.seconds), "-O", fmt.Sprint(s.omit)}, args...)
	return harness.ParseIperf(s.run(ns, time.Duration(s.seconds+s.omit+60)*time.Second, argv...).Stdout)
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
				argv := harness.InNS(ns, bench.Pin(s.opts.AppCPUs, "iperf3", "-c", netPeerAddr, "-t", "60")...)
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
	r, err := bench.Load(fs.Arg(0))
	if err != nil {
		return err
	}
	fmt.Print(r.Summary(netBenchJobDefs()))
	return nil
}
