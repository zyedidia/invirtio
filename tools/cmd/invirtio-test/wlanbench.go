package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

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

// WlanBenchRun is one iperf3 run, an upload to the server or a download
// from it.
type WlanBenchRun struct {
	Dir         string  `json:"dir"`
	Rep         int     `json:"rep"`
	Mbps        float64 `json:"mbps"`
	Retransmits int64   `json:"retransmits"`
	HostCPUs    float64 `json:"host_cpus"`
	UMLCPUs     float64 `json:"uml_cpus,omitempty"`
}

type WlanBenchTarget struct {
	Name string            `json:"name"`
	Link map[string]string `json:"link"`
	Runs []WlanBenchRun    `json:"runs"`
	// The round trips to the server, idle and during a download.
	PingIdle   harness.Ping `json:"ping_idle"`
	PingLoaded harness.Ping `json:"ping_loaded"`
	// The guest's memory over the target's runs.
	Memory *harness.GuestMemory `json:"memory,omitempty"`
}

type WlanBenchResults struct {
	Host    map[string]string `json:"host"`
	Targets []WlanBenchTarget `json:"targets"`
}

type wlanBenchSuite struct {
	out      string
	build    string
	card     string
	conf     string
	server   string
	noServer bool
	reps     int
	seconds  int
	mem      string
	results  WlanBenchResults
	// The guest of the guest target, whose memory is sampled between runs.
	guest  *harness.Guest
	memory *harness.GuestMemory
	// ctx ends on a signal, which stops a running iperf3, and the target
	// that runs then fails and cleans up.
	ctx context.Context
}

func cmdBenchWlan(args []string) error {
	root := harness.RepoRoot()
	fs := newFlags("bench-wlan", `
usage: invirtio-test bench-wlan [options] -conf <wpa_supplicant.conf> -server <address> [<target>...]

Measure a WiFi card's throughput and latency against an iperf3 server that
is reached through the access point. Each target is one of:
  native   the card on its host driver
  guest    the card driven by the invirtio guest under the launcher and
           exported to the host's virtio_wlan

The default runs both. For each, the interface is taken from
NetworkManager and the system wpa_supplicant, and its wiphy moves into the
network namespace wlan-bench, so that traffic to a server on this host
crosses the radio instead of being delivered locally. There it joins the
network of -conf with wpa_supplicant and gets a lease with busybox udhcpc.
The server is one of this host's addresses on the access point's uplink,
where bench-wlan runs an iperf3 server on port 5299 unless -no-server is
given, or a host behind the access point that runs one. Each target runs
iperf3 TCP to the server and from it, -reps times for -time seconds, and
pings the server idle and during one more download. A run's CPU use is how
many of the host's CPUs were busy, and with the guest, how many its UML
process took. The guest runs with -mem in a cgroup of its own, as
invirtio's guests do, and the results record the cgroup's peak memory and
the most memory that the guest itself used between runs. The card must not
be in use, so stop its invirtio service
first, and it is returned to its driver afterwards. Build the guest with
scripts/invirtio-build-guest.sh. The output directory gets results.json
and the summary, which bench-wlan-summary prints again, and the logs. Run
as root on the linux-host kernel, with iw, wpa_supplicant, iperf3 and
busybox.`)
	build := fs.String("guest", filepath.Join(root, "linux-uml/build-invirtio"), "the build directory of the guest")
	out := fs.String("out", filepath.Join(root, "build/bench-wlan"), "the output directory")
	card := fs.String("device", "0000:06:00.0", "the PCI address of the card")
	conf := fs.String("conf", "", "the wpa_supplicant configuration of the network")
	server := fs.String("server", "", "the address of the iperf3 server")
	noServer := fs.Bool("no-server", false, "use an iperf3 server that already runs")
	reps := fs.Int("reps", 3, "the repetitions of each direction")
	seconds := fs.Int("time", 10, "the seconds that each iperf3 run takes")
	mem := fs.String("mem", "768M", "the guest's memory, as UML's mem= takes it")
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
	if *conf == "" || *server == "" {
		fs.Usage()
		os.Exit(2)
	}
	requireRoot()

	s := &wlanBenchSuite{build: *build, conf: *conf, server: *server, noServer: *noServer, mem: *mem,
		reps: *reps, seconds: *seconds}
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
	for _, tool := range []string{"iw", "wpa_supplicant", "wpa_cli", "iperf3", "busybox", "vdpa", "ping"} {
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
	s.results.Host = map[string]string{
		"uname":  strings.TrimSpace(harness.Run("uname", "-a").Stdout),
		"card":   strings.TrimSpace(harness.Run("lspci", "-s", s.card).Stdout),
		"server": s.server,
		"reps":   fmt.Sprint(s.reps),
		"time":   fmt.Sprint(s.seconds),
	}
	if err := os.WriteFile(filepath.Join(s.out, "udhcpc.sh"), []byte(wlanBenchUdhcpc), 0755); err != nil {
		return err
	}

	// The card goes back to the driver it had.
	before := harness.PCIDriver(s.card)
	defer func() {
		harness.Cleanup(wlanBenchName)
		if harness.PCIDriver(s.card) == before {
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
	for _, name := range targets {
		if ctx.Err() != nil {
			return errors.New("stopped by a signal")
		}
		fmt.Printf("== %s\n", name)
		t := WlanBenchTarget{Name: name}
		if name == "native" {
			err = s.runNative(&t)
		} else {
			err = s.runGuest(&t)
		}
		if err != nil {
			return fmt.Errorf("%s: %v", name, err)
		}
		s.results.Targets = append(s.results.Targets, t)
		data, _ := json.MarshalIndent(&s.results, "", "  ")
		if err := os.WriteFile(filepath.Join(s.out, "results.json"), append(data, '\n'), 0644); err != nil {
			return err
		}
	}
	summary := wlanBenchSummary(&s.results)
	fmt.Print(summary)
	return os.WriteFile(filepath.Join(s.out, "summary.txt"), []byte(summary), 0644)
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

func (s *wlanBenchSuite) runNative(t *WlanBenchTarget) error {
	if err := pci.BindNative(s.card); err != nil {
		return err
	}
	phy, net := wiphy(pci.Path(s.card, ""))
	if net == "" {
		return fmt.Errorf("%s's driver %q made no wiphy", s.card, harness.PCIDriver(s.card))
	}
	return s.measure(t, phy, net, 0)
}

func (s *wlanBenchSuite) runGuest(t *WlanBenchTarget) error {
	logDir, err := harness.MakeLogDir(wlanBenchName)
	if err != nil {
		return err
	}
	defer os.RemoveAll(logDir)
	// The guest runs in a cgroup of its own, as invirtio's guests do, which
	// records its peak memory.
	cg, err := harness.MakeCgroup(wlanBenchName)
	if err != nil {
		return err
	}
	defer harness.RemoveCgroup(cg)
	g, err := harness.LaunchGuest(filepath.Join(s.out, "guest.log"), harness.Launch{
		Build:   s.build,
		Type:    "wlan",
		Name:    wlanBenchName,
		LogDir:  logDir,
		Devices: []string{s.card},
		Args:    []string{"mem=" + s.mem},
		Cgroup:  cg,
	})
	if err != nil {
		return err
	}
	defer func() {
		harness.Detach(wlanBenchName, !g.Running())
		g.Stop(30 * time.Second)
		g.Kill()
		harness.Cleanup(wlanBenchName)
		s.guest, s.memory = nil, nil
	}()
	if !g.WaitFor(wlanBenchName+": created", 2*time.Minute) {
		return errors.New("the guest did not create its device, see guest.log")
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
	uml := harness.UMLPid(g.Pid())
	if uml == 0 {
		return errors.New("cannot find the guest's UML process")
	}
	s.guest, s.memory = g, &harness.GuestMemory{Mem: s.mem}
	s.memory.SampleGuest(g.Output("cat /proc/meminfo", 10*time.Second))
	if err := s.measure(t, phy, net, uml); err != nil {
		return err
	}
	s.memory.SampleGuest(g.Output("cat /proc/meminfo", 10*time.Second))
	if err := s.memory.ReadCgroup(cg); err != nil {
		return err
	}
	t.Memory = s.memory
	fmt.Println("  memory: " + t.Memory.Summary())
	return nil
}

// measure joins the network in the namespace and runs the iperf3 runs and
// pings. uml is the guest's UML process, or 0.
func (s *wlanBenchSuite) measure(t *WlanBenchTarget, phy, net string, uml int) error {
	dir := filepath.Join(s.out, t.Name)
	os.MkdirAll(dir, 0755)
	ns := func(argv ...string) []string { return harness.InNS(wlanBenchNS, argv...) }
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
		server, err := start("iperf3-server.log", "iperf3", "-s", "-B", s.server, "-p", fmt.Sprint(wlanBenchPort))
		if err != nil {
			return err
		}
		defer stop(server)
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
	wpa, err := start("wpa_supplicant.log", ns("wpa_supplicant", "-i", net, "-D", "nl80211", "-c", conf)...)
	if err != nil {
		return err
	}
	defer stop(wpa)
	joined := harness.Wait(func() bool {
		return strings.Contains(harness.RunArgv(ns("wpa_cli", "-p", ctrl, "-i", net, "status")).Stdout, "wpa_state=COMPLETED")
	}, 30*time.Second, 200*time.Millisecond)
	if !joined {
		return fmt.Errorf("wpa_supplicant did not join the network, see %s", filepath.Join(dir, "wpa_supplicant.log"))
	}
	r := harness.RunArgv(ns("busybox", "udhcpc", "-i", net, "-f", "-n", "-q", "-t", "5", "-T", "2",
		"-s", filepath.Join(s.out, "udhcpc.sh")))
	if !r.OK() {
		return fmt.Errorf("no DHCP lease: %s", strings.TrimSpace(r.Output()))
	}
	t.Link = map[string]string{}
	for _, line := range strings.Split(harness.RunArgv(ns("iw", "dev", net, "link")).Stdout, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), ": "); ok {
			t.Link[k] = v
		}
	}

	ping := func(count int) (harness.Ping, error) {
		argv := ns("ping", "-q", "-c", fmt.Sprint(count), "-i", "0.2", "-W", "2", s.server)
		r := harness.RunContext(s.ctx, dir, time.Duration(count)*time.Second+time.Minute, argv[0], argv[1:]...)
		p, ok := harness.ParsePing(r.Output())
		if !ok {
			return p, fmt.Errorf("ping printed %q", strings.TrimSpace(r.Output()))
		}
		return p, nil
	}
	if t.PingIdle, err = ping(50); err != nil {
		return err
	}
	iperf := func(ctx context.Context, reverse bool) (harness.Iperf, error) {
		argv := ns("iperf3", "-c", s.server, "-p", fmt.Sprint(wlanBenchPort), "-t", fmt.Sprint(s.seconds), "-J")
		if reverse {
			argv = append(argv, "-R")
		}
		var res harness.Iperf
		var err error
		// The server stops listening for a moment between runs.
		for i := 0; i < 5; i++ {
			r := harness.RunContext(ctx, dir, time.Duration(s.seconds+60)*time.Second, argv[0], argv[1:]...)
			if res, err = harness.ParseIperf(r.Stdout); err == nil || !strings.Contains(err.Error(), "refused") {
				break
			}
			time.Sleep(time.Second)
		}
		return res, err
	}
	for rep := 1; rep <= s.reps; rep++ {
		for _, reverse := range []bool{false, true} {
			if s.ctx.Err() != nil {
				return errors.New("stopped by a signal")
			}
			run := WlanBenchRun{Dir: "upload", Rep: rep}
			if reverse {
				run.Dir = "download"
			}
			meter := startCPUMeter(uml)
			res, err := iperf(s.ctx, reverse)
			run.HostCPUs, run.UMLCPUs = meter.stop()
			if err != nil {
				return fmt.Errorf("%s: %v", run.Dir, err)
			}
			run.Mbps, run.Retransmits = res.ReceivedBPS/1e6, res.Retransmits
			fmt.Printf("  %s %d: %.1f Mb/s, host_cpus %.2f, uml_cpus %.2f\n", run.Dir, rep, run.Mbps, run.HostCPUs, run.UMLCPUs)
			t.Runs = append(t.Runs, run)
			if s.guest != nil {
				s.memory.SampleGuest(s.guest.Output("cat /proc/meminfo", 10*time.Second))
			}
		}
	}
	// The round trips under load are measured during one more download.
	ctx, cancel := context.WithCancel(s.ctx)
	done := make(chan struct{})
	go func() {
		iperf(ctx, true)
		close(done)
	}()
	time.Sleep(time.Second)
	t.PingLoaded, err = ping(max(1, (s.seconds-2)*5))
	cancel()
	<-done
	return err
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
	data, err := os.ReadFile(filepath.Join(fs.Arg(0), "results.json"))
	if err != nil {
		return err
	}
	var r WlanBenchResults
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}
	fmt.Print(wlanBenchSummary(&r))
	return nil
}

func wlanBenchSummary(r *WlanBenchResults) string {
	var b strings.Builder
	for _, t := range r.Targets {
		fmt.Fprintf(&b, "%s: %s MHz, %s, tx %s, rx %s\n", t.Name, t.Link["freq"], t.Link["SSID"],
			t.Link["tx bitrate"], t.Link["rx bitrate"])
		fmt.Fprintf(&b, "  %-9s %8s %8s %8s %9s %8s\n", "", "Mb/s", "min", "max", "host_cpus", "uml_cpus")
		for _, dir := range []string{"upload", "download"} {
			var mbps, host, uml []float64
			for _, run := range t.Runs {
				if run.Dir == dir {
					mbps = append(mbps, run.Mbps)
					host = append(host, run.HostCPUs)
					uml = append(uml, run.UMLCPUs)
				}
			}
			if len(mbps) == 0 {
				continue
			}
			lo, hi := mbps[0], mbps[0]
			for _, v := range mbps {
				lo, hi = min(lo, v), max(hi, v)
			}
			fmt.Fprintf(&b, "  %-9s %8.1f %8.1f %8.1f %9.2f %8.2f\n", dir, benchMedian(mbps), lo, hi,
				benchMedian(host), benchMedian(uml))
		}
		for _, p := range []struct {
			name string
			p    harness.Ping
		}{{"idle", t.PingIdle}, {"loaded", t.PingLoaded}} {
			fmt.Fprintf(&b, "  ping %-6s avg %.2f ms, max %.2f ms, mdev %.2f ms, loss %.0f%%\n",
				p.name, p.p.Avg, p.p.Max, p.p.Mdev, p.p.Loss)
		}
		b.WriteString("\n")
	}
	for _, t := range r.Targets {
		if t.Memory != nil {
			fmt.Fprintf(&b, "%s memory, %s\n\n", t.Name, t.Memory.Summary())
		}
	}
	b.WriteString("Mb/s is the median over the repetitions. host_cpus is how many of the\n" +
		"host's CPUs were busy, and uml_cpus how many the guest's UML process took.\n")
	return b.String()
}
