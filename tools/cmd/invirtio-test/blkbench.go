package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"invirtio/internal/harness"
	"invirtio/internal/vdpa"
)

func init() {
	register("bench-blk", "benchmark the VDUSE virtio-blk backend against null_blk", cmdBenchBlk)
	register("bench-blk-summary", "print the results of bench-blk again", cmdBenchBlkSummary)
}

const (
	benchName    = "vblk-bench"
	benchQSDName = "vblk-qsd"
)

// A benchJob is one fio job, run against every target.
type benchJob struct {
	name  string
	rw    string
	bs    string
	depth int
}

var benchJobs = []benchJob{
	{"lat-qd1", "randread", "4k", 1},
	{"iops-qd32", "randread", "4k", 32},
	{"iops-qd128", "randread", "4k", 128},
	{"seqread-128k", "read", "128k", 8},
	{"seqread-1m", "read", "1m", 8},
	{"seqwrite-128k", "write", "128k", 8},
	{"seqwrite-1m", "write", "1m", 8},
}

// benchCounters are the VDUSE counters that the summary shows, summed over
// the queues.
var benchCounters = []string{"kicks", "kick_irqs", "requests", "injected", "suppressed",
	"bad_requests", "inject_errors", "dropped_kicks", "polled", "poll_timeouts"}

// BenchRun is one repetition of one job against one target.
type BenchRun struct {
	Job    string  `json:"job"`
	Rep    int     `json:"rep"`
	IOPS   float64 `json:"iops"`
	BW     float64 `json:"bw_bytes"`
	Mean   float64 `json:"lat_mean_ns"`
	P50    float64 `json:"clat_p50_ns"`
	P99    float64 `json:"clat_p99_ns"`
	P999   float64 `json:"clat_p999_ns"`
	IOs    float64 `json:"total_ios"`
	TimeMS float64 `json:"job_runtime_ms"`
	// User and system ticks of the device process during the run, which
	// also cover fio's ramp.
	UserTicks int64 `json:"user_ticks,omitempty"`
	SysTicks  int64 `json:"sys_ticks,omitempty"`
	HasTicks  bool  `json:"has_ticks"`
	// The change of the guest's VDUSE counters, summed over the queues.
	Counters map[string]int64 `json:"counters,omitempty"`
}

type BenchTarget struct {
	Name    string     `json:"name"`
	Kind    string     `json:"kind"`
	Options string     `json:"options,omitempty"`
	Runs    []BenchRun `json:"runs"`
}

type BenchResults struct {
	Host    map[string]string `json:"host"`
	Targets []BenchTarget     `json:"targets"`
}

type benchSuite struct {
	out      string
	guestDir string
	fioCPU   string
	devCPUs  []string
	irqCPU   int
	reps     int
	runtime  int
	ramp     int
	jobs     []benchJob
	results  BenchResults
	// ctx ends on a signal, which kills a running fio, and the target that
	// runs then fails and cleans up.
	ctx context.Context
}

func cmdBenchBlk(args []string) error {
	root := harness.RepoRoot()
	fs := newFlags("bench-blk", `
usage: invirtio-test bench-blk [options] <target>...

Benchmark VirtIO block devices with fio against the host's null_blk. Each
target is one of:
  native                         null_blk on the host
  qsd                            qemu-storage-daemon exporting a null-co node
                                 with its vduse-blk export
  uml[:<name>[:<options>]]       a UML guest run as root, exporting its own
                                 null_blk through VDUSE. <options> are added
                                 to the device's, such as bounce=guest
  sandbox[:<name>[:<options>]]   the same guest under the launcher's sandbox

Every target serves a 1 GiB disk with 512-byte blocks and the none
scheduler, with one queue per guest CPU (one by default) of 256 entries.
fio runs on -fio-cpu, the guest's CPUs on -dev-cpus, and VDUSE injects each
queue's interrupts from a kworker on -irq-cpu, or, with -1, from where
VDUSE's default puts it. Each job runs -reps times for -runtime seconds after
a -ramp of one second. The guest is built with
scripts/invirtio-build-guest.sh -t blk-bench.

The output directory gets fio's JSON for each run, the guest's
/proc/vduse_guest before and after it, results.json and the summary, which
bench-blk-summary prints again. cpu/io is the device process's CPU time per
I/O: the UML process, the VMM's for qsd. Run as root on the linux-host
kernel, with fio.

Jobs:`+benchJobList())
	guest := fs.String("guest", filepath.Join(root, "linux-uml/build-blk-bench"), "the build directory of the guest")
	out := fs.String("out", filepath.Join(root, "build/bench-blk"), "the output directory")
	fioCPU := fs.String("fio-cpu", "2", "the host CPU that fio runs on")
	devCPUs := fs.String("dev-cpus", "4", "the host CPUs of the guest's CPUs, one per guest CPU, or of qsd")
	irqCPU := fs.Int("irq-cpu", 3, "the host CPU of VDUSE's interrupt injection, or -1 for VDUSE's default")
	reps := fs.Int("reps", 3, "the repetitions of each job")
	runtime := fs.Int("runtime", 5, "the seconds that each job runs")
	ramp := fs.Int("ramp", 1, "the seconds before fio measures")
	jobs := fs.String("jobs", "", "run only the jobs whose names contain this string")
	fs.Parse(args)
	if fs.NArg() == 0 {
		fs.Usage()
		os.Exit(2)
	}
	requireRoot()

	s := &benchSuite{guestDir: *guest, fioCPU: *fioCPU, irqCPU: *irqCPU,
		reps: *reps, runtime: *runtime, ramp: *ramp}
	s.devCPUs = strings.Split(*devCPUs, ",")
	for _, j := range benchJobs {
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
	if err := harness.Modprobe("vduse", "vdpa", "virtio_vdpa", "virtio_blk"); err != nil {
		return err
	}
	// The guest's bounce region in guest pages is pinned memory.
	unix.Setrlimit(unix.RLIMIT_MEMLOCK, &unix.Rlimit{Cur: unix.RLIM_INFINITY, Max: unix.RLIM_INFINITY})
	for _, name := range []string{benchName, benchQSDName} {
		if vdpa.Exists(name) {
			return fmt.Errorf("/dev/vduse/%s exists, perhaps from an earlier run, so remove it first", name)
		}
	}
	s.results.Host = benchHostInfo(s)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer cancel()
	s.ctx = ctx
	defer s.cleanup()

	for _, spec := range fs.Args() {
		if ctx.Err() != nil {
			return errors.New("stopped by a signal")
		}
		t, err := s.runTarget(spec)
		if err != nil {
			return fmt.Errorf("%s: %v", spec, err)
		}
		s.results.Targets = append(s.results.Targets, t)
		if err := s.save(); err != nil {
			return err
		}
	}
	summary := benchSummary(&s.results)
	fmt.Print(summary)
	return os.WriteFile(filepath.Join(s.out, "summary.txt"), []byte(summary), 0644)
}

func benchJobList() string {
	var b strings.Builder
	for _, j := range benchJobs {
		fmt.Fprintf(&b, "\n  %-14s %s of %s blocks at depth %d", j.name, j.rw, j.bs, j.depth)
	}
	return b.String()
}

func (s *benchSuite) save() error {
	data, err := json.MarshalIndent(&s.results, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.out, "results.json"), append(data, '\n'), 0644)
}

func (s *benchSuite) cleanup() {
	harness.Cleanup(benchName)
	harness.Cleanup(benchQSDName)
	harness.Run("modprobe", "-r", "null_blk")
}

func benchHostInfo(s *benchSuite) map[string]string {
	h := map[string]string{
		"uname":    strings.TrimSpace(harness.Run("uname", "-a").Stdout),
		"governor": harness.Read("/sys/devices/system/cpu/cpu" + s.devCPUs[0] + "/cpufreq/scaling_governor"),
		"boost":    harness.Read("/sys/devices/system/cpu/cpufreq/boost"),
		"clk_tck":  strings.TrimSpace(harness.Run("getconf", "CLK_TCK").Stdout),
		"fio_cpu":  s.fioCPU,
		"dev_cpus": strings.Join(s.devCPUs, ","),
		"irq_cpu":  strconv.Itoa(s.irqCPU),
		"reps":     strconv.Itoa(s.reps),
		"runtime":  strconv.Itoa(s.runtime),
		"ramp":     strconv.Itoa(s.ramp),
		"fio":      strings.TrimSpace(harness.Run("fio", "--version").Stdout),
	}
	uml := filepath.Join(harness.RepoRoot(), "linux-uml/linux")
	h["linux-uml"] = strings.TrimSpace(harness.Run("git", "-C", uml, "log", "-1", "--format=%h %s").Stdout)
	if !harness.Run("git", "-C", uml, "diff", "--quiet", "HEAD").OK() {
		h["linux-uml"] += " (with uncommitted changes)"
	}
	if data, err := os.ReadFile(filepath.Join(s.guestDir, ".config")); err == nil {
		var opts []string
		for _, l := range strings.Split(string(data), "\n") {
			for _, o := range []string{"CONFIG_SMP=", "CONFIG_NR_CPUS=", "CONFIG_PROVE_LOCKING=", "CONFIG_LOCKDEP="} {
				if strings.HasPrefix(l, o) {
					opts = append(opts, l)
				}
			}
		}
		h["guest_config"] = strings.Join(opts, " ")
	}
	return h
}

// runTarget parses a target and runs every job against it.
func (s *benchSuite) runTarget(spec string) (BenchTarget, error) {
	parts := strings.SplitN(spec, ":", 3)
	t := BenchTarget{Kind: parts[0], Name: parts[0]}
	if len(parts) > 1 && parts[1] != "" {
		t.Name = parts[1]
	}
	if len(parts) > 2 {
		t.Options = parts[2]
	}
	if strings.ContainsAny(t.Name, "/ ") {
		return t, fmt.Errorf("the name %q has a slash or a space", t.Name)
	}
	for _, other := range s.results.Targets {
		if other.Name == t.Name {
			return t, fmt.Errorf("two targets are named %s", t.Name)
		}
	}
	fmt.Printf("== %s\n", t.Name)
	switch t.Kind {
	case "native":
		return t, s.runNative(&t)
	case "qsd":
		return t, s.runQSD(&t)
	case "uml", "sandbox":
		return t, s.runGuest(&t)
	}
	return t, fmt.Errorf("unknown target kind %q", t.Kind)
}

func (s *benchSuite) runNative(t *BenchTarget) error {
	harness.Run("modprobe", "-r", "null_blk")
	err := harness.MustRun("modprobe", "null_blk", "nr_devices=1", "gb=1", "hw_queue_depth=256",
		fmt.Sprintf("submit_queues=%d", len(s.devCPUs)), "memory_backed=0")
	if err != nil {
		return err
	}
	defer harness.Run("modprobe", "-r", "null_blk")
	if !harness.Wait(func() bool { return harness.Exists("/dev/nullb0") }, 30*time.Second, 200*time.Millisecond) {
		return errors.New("null_blk made no /dev/nullb0")
	}
	return s.runJobs(t, "/dev/nullb0", 0, nil)
}

func (s *benchSuite) runQSD(t *BenchTarget) error {
	if _, err := exec.LookPath("qemu-storage-daemon"); err != nil {
		fmt.Println("skipped: qemu-storage-daemon is not installed")
		return nil
	}
	logf, err := os.Create(filepath.Join(s.out, t.Name+".log"))
	if err != nil {
		return err
	}
	defer logf.Close()
	qsd := exec.Command("taskset", "-c", strings.Join(s.devCPUs, ","), "qemu-storage-daemon",
		"--blockdev", "driver=null-co,node-name=null0,size=1073741824,read-zeroes=off",
		"--export", fmt.Sprintf("type=vduse-blk,id=exp0,node-name=null0,name=%s,writable=on,num-queues=%d,queue-size=256",
			benchQSDName, len(s.devCPUs)))
	qsd.Stdout = logf
	qsd.Stderr = logf
	// qsd runs in a process group of its own, so that a terminal's signals
	// reach only bench-blk, which stops it after removing its device.
	qsd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := qsd.Start(); err != nil {
		return err
	}
	defer func() {
		harness.Detach(benchQSDName, false)
		qsd.Process.Signal(syscall.SIGTERM)
		qsd.Wait()
		harness.Cleanup(benchQSDName)
	}()
	if !harness.Wait(func() bool { return vdpa.Exists(benchQSDName) }, 30*time.Second, 200*time.Millisecond) {
		return errors.New("qemu-storage-daemon made no VDUSE device")
	}
	dev, err := s.attach(benchQSDName)
	if err != nil {
		return err
	}
	return s.runJobs(t, dev, qsd.Process.Pid, nil)
}

func (s *benchSuite) runGuest(t *BenchTarget) error {
	n := len(s.devCPUs)
	initrd := filepath.Join(s.guestDir, "initramfs/initramfs.cpio")
	opts := ""
	if t.Options != "" {
		opts = "," + t.Options
	}
	uml := []string{
		filepath.Join(s.guestDir, "linux"), "mem=512M", fmt.Sprintf("ncpus=%d", n),
		"cpu_pin=" + strings.Join(s.devCPUs, ","),
		"null_blk.nr_devices=1", "null_blk.gb=1", "null_blk.hw_queue_depth=256",
		fmt.Sprintf("null_blk.submit_queues=%d", n), "null_blk.memory_backed=0",
	}
	device := "vduse_guest.device=blk," + benchName + ",disk=/dev/nullb0"
	var argv []string
	if t.Kind == "uml" {
		log := filepath.Join(s.out, t.Name+".inflight")
		defer os.Remove(log)
		argv = append(uml, "umid="+benchName, "seccomp=on", "initrd="+initrd, device+",log="+log+opts)
	} else {
		logs, err := os.MkdirTemp("/dev/shm", benchName+"-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(logs)
		launcher := filepath.Join(harness.RepoRoot(), "build/invirtio-launch")
		argv = append([]string{launcher, "-u", "nobody", "-t", "20", "-l", "1024", "-i", initrd,
			"-e", "blk," + benchName + ",log=" + filepath.Join(logs, "inflight")}, uml...)
		argv = append(argv, device+opts)
	}
	g, err := harness.StartGuest(filepath.Join(s.out, t.Name+".guest.log"), argv...)
	if err != nil {
		return err
	}
	defer func() {
		harness.Detach(benchName, false)
		g.Stop(30 * time.Second)
		g.Kill()
		harness.Cleanup(benchName)
	}()
	if !g.WaitFor(benchName+": created", 60*time.Second) {
		return fmt.Errorf("the guest did not create its device, see %s", g.Path)
	}
	dev, err := s.attach(benchName)
	if err != nil {
		return err
	}
	pid := g.Pid()
	if t.Kind == "sandbox" {
		if pid = harness.UMLPid(pid); pid == 0 {
			return errors.New("cannot find the guest's UML process")
		}
	}
	return s.runJobs(t, dev, pid, g)
}

// attach adds the device to the vDPA bus, places its interrupt injection,
// and returns its host disk.
func (s *benchSuite) attach(name string) (string, error) {
	if err := harness.Attach(name); err != nil {
		return "", err
	}
	var dev string
	ok := harness.Wait(func() bool {
		blocks := harness.Glob(filepath.Join("/sys/bus/vdpa/devices", name, "virtio*/block/*"))
		if len(blocks) == 0 {
			return false
		}
		dev = "/dev/" + blocks[0]
		return harness.Exists(dev)
	}, 30*time.Second, 200*time.Millisecond)
	if !ok {
		return "", errors.New("the host made no block device")
	}
	harness.Run("udevadm", "settle")
	if err := harness.PlaceIRQs(name, s.irqCPU); err != nil {
		return "", err
	}
	return dev, nil
}

// runJobs runs every job against a disk. With a device process, it records
// the process's CPU time, and with a guest, the guest's VDUSE counters.
func (s *benchSuite) runJobs(t *BenchTarget, dev string, pid int, g *harness.Guest) error {
	dir := filepath.Join(s.out, t.Name)
	os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	harness.Write(filepath.Join("/sys/block", filepath.Base(dev), "queue/scheduler"), "none")
	for rep := 1; rep <= s.reps; rep++ {
		for _, j := range s.jobs {
			run := fmt.Sprintf("%s-%d", j.name, rep)
			var before string
			if g != nil {
				before = g.Output("cat /proc/vduse_guest", 10*time.Second)
				os.WriteFile(filepath.Join(dir, run+".before"), []byte(before), 0644)
			}
			var t0 [2]int64
			if pid > 0 {
				t0 = harness.ProcessTicks(pid)
			}
			out := filepath.Join(dir, run+".json")
			r := harness.RunContext(s.ctx, dir, time.Duration(s.runtime+s.ramp+60)*time.Second, "taskset", "-c", s.fioCPU,
				"fio", "--name="+j.name, "--filename="+dev, "--rw="+j.rw, "--bs="+j.bs,
				fmt.Sprintf("--iodepth=%d", j.depth), "--direct=1", "--ioengine=io_uring",
				"--time_based", fmt.Sprintf("--runtime=%d", s.runtime), fmt.Sprintf("--ramp_time=%d", s.ramp),
				"--norandommap", "--randrepeat=0", "--output-format=json", "--output="+out)
			if !r.OK() {
				return fmt.Errorf("fio %s: %s", run, strings.TrimSpace(r.Output()))
			}
			data, err := os.ReadFile(out)
			if err != nil {
				return err
			}
			br, err := benchParseFio(data)
			if err != nil {
				return fmt.Errorf("fio %s: %v", run, err)
			}
			br.Job, br.Rep = j.name, rep
			if pid > 0 {
				t1 := harness.ProcessTicks(pid)
				br.UserTicks, br.SysTicks, br.HasTicks = t1[0]-t0[0], t1[1]-t0[1], true
			}
			if g != nil {
				after := g.Output("cat /proc/vduse_guest", 10*time.Second)
				os.WriteFile(filepath.Join(dir, run+".after"), []byte(after), 0644)
				// A dump that did not arrive leaves the run without
				// counters, rather than with wrong ones.
				if before != "" && after != "" {
					br.Counters = benchCounterDelta(before, after)
				}
			}
			t.Runs = append(t.Runs, br)
			fmt.Printf("  %-14s rep %d: %9.0f IOPS, %8.1f MiB/s\n", j.name, rep, br.IOPS, br.BW/(1<<20))
		}
	}
	return nil
}

// benchCounterDelta returns the change of each counter between two dumps of
// /proc/vduse_guest, summed over the queues.
func benchCounterDelta(before, after string) map[string]int64 {
	sum := func(text string) map[string]int64 {
		out := map[string]int64{}
		for k, v := range harness.ParseCounters(text) {
			if i := strings.IndexByte(k, '_'); i > 0 && k[0] == 'q' {
				if _, err := strconv.Atoi(k[1:i]); err == nil {
					k = k[i+1:]
				}
			}
			out[k] += v
		}
		return out
	}
	a, b := sum(after), sum(before)
	d := map[string]int64{}
	for _, c := range benchCounters {
		d[c] = a[c] - b[c]
	}
	return d
}

type benchFioSide struct {
	IOBytes  float64 `json:"io_bytes"`
	IOPS     float64 `json:"iops"`
	BW       float64 `json:"bw_bytes"`
	TotalIOs float64 `json:"total_ios"`
	Lat      struct {
		Mean float64 `json:"mean"`
	} `json:"lat_ns"`
	Clat struct {
		Percentile map[string]float64 `json:"percentile"`
	} `json:"clat_ns"`
}

// benchParseFio takes the numbers of a run from fio's JSON output, from the
// direction that did I/O.
func benchParseFio(data []byte) (BenchRun, error) {
	var out struct {
		Jobs []struct {
			Read    benchFioSide `json:"read"`
			Write   benchFioSide `json:"write"`
			Runtime float64      `json:"job_runtime"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return BenchRun{}, err
	}
	if len(out.Jobs) == 0 {
		return BenchRun{}, errors.New("no jobs in fio's output")
	}
	j := out.Jobs[0]
	side := j.Read
	if side.IOBytes == 0 {
		side = j.Write
	}
	p := side.Clat.Percentile
	return BenchRun{
		IOPS: side.IOPS, BW: side.BW, Mean: side.Lat.Mean,
		P50: p["50.000000"], P99: p["99.000000"], P999: p["99.900000"],
		IOs: side.TotalIOs, TimeMS: j.Runtime,
	}, nil
}

func cmdBenchBlkSummary(args []string) error {
	fs := newFlags("bench-blk-summary", `
usage: invirtio-test bench-blk-summary <output dir>

Print the summary of a bench-blk run from its results.json.`)
	fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		os.Exit(2)
	}
	data, err := os.ReadFile(filepath.Join(fs.Arg(0), "results.json"))
	if err != nil {
		return err
	}
	var r BenchResults
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}
	fmt.Print(benchSummary(&r))
	return nil
}

func benchMedian(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	if n := len(s); n%2 == 1 {
		return s[n/2]
	} else {
		return (s[n/2-1] + s[n/2]) / 2
	}
}

// benchSummary prints, for each job and target, the medians over the
// repetitions and the spread of the IOPS, and for each guest target the
// medians of its VDUSE counters.
func benchSummary(r *BenchResults) string {
	var b strings.Builder
	hz, _ := strconv.ParseFloat(r.Host["clk_tck"], 64)
	if hz == 0 {
		hz = 100
	}
	ramp, _ := strconv.ParseFloat(r.Host["ramp"], 64)
	var jobs []string
	seen := map[string]bool{}
	for _, t := range r.Targets {
		for _, run := range t.Runs {
			if !seen[run.Job] {
				seen[run.Job] = true
				jobs = append(jobs, run.Job)
			}
		}
	}
	for _, job := range jobs {
		fmt.Fprintf(&b, "%s\n  %-12s %9s %7s %8s %8s %8s %8s %8s %7s %7s\n", job,
			"target", "IOPS", "spread", "MiB/s", "mean", "p50", "p99", "cpu/io", "usr", "sys")
		for _, t := range r.Targets {
			var iops, bw, mean, p50, p99, usr, sys []float64
			for _, run := range t.Runs {
				if run.Job != job {
					continue
				}
				iops = append(iops, run.IOPS)
				bw = append(bw, run.BW/(1<<20))
				mean = append(mean, run.Mean/1000)
				p50 = append(p50, run.P50/1000)
				p99 = append(p99, run.P99/1000)
				if run.HasTicks && run.IOs > 0 && run.TimeMS > 0 {
					// The ticks also cover the ramp, so the I/Os are scaled
					// to the same time.
					ios := run.IOs * (run.TimeMS + 1000*ramp) / run.TimeMS
					usr = append(usr, 1e6*float64(run.UserTicks)/hz/ios)
					sys = append(sys, 1e6*float64(run.SysTicks)/hz/ios)
				}
			}
			if len(iops) == 0 {
				continue
			}
			spread := 0.0
			if m := benchMedian(iops); m > 0 {
				lo, hi := math.Inf(1), math.Inf(-1)
				for _, v := range iops {
					lo, hi = math.Min(lo, v), math.Max(hi, v)
				}
				spread = 100 * (hi - lo) / m
			}
			fmt.Fprintf(&b, "  %-12s %9.0f %6.1f%% %8.0f %8.1f %8.1f %8.1f", t.Name,
				benchMedian(iops), spread, benchMedian(bw), benchMedian(mean), benchMedian(p50), benchMedian(p99))
			if len(usr) > 0 {
				cpu := make([]float64, len(usr))
				for i := range usr {
					cpu[i] = usr[i] + sys[i]
				}
				fmt.Fprintf(&b, " %8.2f %7.2f %7.2f", benchMedian(cpu), benchMedian(usr), benchMedian(sys))
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("Times are in microseconds. cpu/io is the device process's CPU time per\nI/O, split into user and system time.\n")
	for _, t := range r.Targets {
		if len(t.Runs) == 0 || t.Runs[0].Counters == nil {
			continue
		}
		fmt.Fprintf(&b, "\n%s counters summed over the queues, median over the repetitions\n  %-14s", t.Name, "job")
		for _, c := range benchCounters {
			fmt.Fprintf(&b, " %13s", c)
		}
		b.WriteString("\n")
		for _, job := range jobs {
			fmt.Fprintf(&b, "  %-14s", job)
			for _, c := range benchCounters {
				var xs []float64
				for _, run := range t.Runs {
					if run.Job == job {
						xs = append(xs, float64(run.Counters[c]))
					}
				}
				fmt.Fprintf(&b, " %13.0f", benchMedian(xs))
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}
