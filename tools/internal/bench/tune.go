package bench

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"invirtio/internal/harness"
	"invirtio/internal/pci"
)

// Options are the settings that every benchmark takes: how often it runs
// its jobs, where it places its work on host CPUs, and how it tunes the
// host.
type Options struct {
	Rounds int
	Reps   int
	// AppCPUs runs the benchmark's programs, DeviceCPU takes the device's
	// interrupts and runs the guest, and PeerCPUs runs the far end's
	// programs and takes its interrupts. They are "" without pinning.
	AppCPUs   string
	DeviceCPU string
	PeerCPUs  string
	Boost     string
	NoTune    bool
	noPin     bool
	sets      []Set
	otherCPUs string
	// stoppedIRQBalance is set when Tune stopped irqbalance.
	stoppedIRQBalance bool
}

// AddFlags adds the options' flags, with the defaults of a benchmark for
// the rounds and repetitions.
func AddFlags(fs *flag.FlagSet, rounds, reps int) *Options {
	o := &Options{}
	fs.IntVar(&o.Rounds, "rounds", rounds, "the rounds, each of which brings up every target once, in alternating order")
	fs.IntVar(&o.Reps, "reps", reps, "the repetitions of each job in each round")
	fs.StringVar(&o.AppCPUs, "app-cpus", "6-7", "the host CPUs of the benchmark's programs")
	fs.StringVar(&o.DeviceCPU, "device-cpu", "4", "the host CPU of the device's interrupts and of the guest")
	fs.StringVar(&o.PeerCPUs, "peer-cpus", "8-9", "the host CPUs of the far end's servers, and of its interrupts where the benchmark has them")
	fs.BoolVar(&o.noPin, "no-pin", false, "leave programs, interrupts and the guest unpinned")
	fs.StringVar(&o.Boost, "boost", "off", "the CPU boost during the run: on, off, or keep")
	fs.BoolVar(&o.NoTune, "no-tune", false, "leave the CPU governor, boost and irqbalance as they are")
	return o
}

// Check validates the options after the flags are parsed. The sets must
// be online CPUs, and no two may share a core.
func (o *Options) Check() error {
	if o.Rounds < 1 || o.Reps < 1 {
		return fmt.Errorf("-rounds and -reps must be at least 1")
	}
	if o.Boost != "on" && o.Boost != "off" && o.Boost != "keep" {
		return fmt.Errorf("-boost must be on, off or keep")
	}
	if o.noPin {
		o.AppCPUs, o.DeviceCPU, o.PeerCPUs = "", "", ""
		return nil
	}
	online, err := cpuList(harness.Read("/sys/devices/system/cpu/online"))
	if err != nil {
		return err
	}
	isOnline := map[int]bool{}
	for _, c := range online {
		isOnline[c] = true
	}
	// Each core is named by its first SMT thread.
	core := func(c int) int {
		sib, err := cpuList(harness.Read(fmt.Sprintf("/sys/devices/system/cpu/cpu%d/topology/thread_siblings_list", c)))
		if err != nil || len(sib) == 0 {
			return c
		}
		return sib[0]
	}
	used := map[int]string{}
	for _, s := range []struct{ name, list string }{{"app", o.AppCPUs}, {"device", o.DeviceCPU}, {"peer", o.PeerCPUs}} {
		cpus, err := cpuList(s.list)
		if err != nil {
			return fmt.Errorf("the %s CPUs: %v", s.name, err)
		}
		if s.name == "device" {
			if len(cpus) != 1 {
				return fmt.Errorf("-device-cpu must be one CPU")
			}
			o.DeviceCPU = strconv.Itoa(cpus[0])
		}
		for _, c := range cpus {
			if !isOnline[c] {
				return fmt.Errorf("the %s CPU %d is not online, choose others or give -no-pin", s.name, c)
			}
			if u, ok := used[core(c)]; ok && u != s.name {
				return fmt.Errorf("CPU %d of the %s CPUs shares a core with the %s CPUs", c, s.name, u)
			}
			used[core(c)] = s.name
		}
		o.sets = append(o.sets, Set{s.name, cpus})
	}
	var other []string
	for _, c := range online {
		if u := used[core(c)]; u != "app" && u != "peer" {
			other = append(other, strconv.Itoa(c))
		}
	}
	o.otherCPUs = strings.Join(other, ",")
	return nil
}

// OtherCPUs returns the online CPUs outside the cores of the app and peer
// sets, or "" without pinning.
func (o *Options) OtherCPUs() string { return o.otherCPUs }

// Sets returns the sets of CPUs whose use each run records.
func (o *Options) Sets() []Set { return o.sets }

// Describe returns the options for the results' description of the host.
func (o *Options) Describe() map[string]string {
	pin := "app " + o.AppCPUs + ", device " + o.DeviceCPU + ", peer " + o.PeerCPUs
	if o.noPin {
		pin = "none"
	}
	tune := "governor performance, boost " + o.Boost
	if o.stoppedIRQBalance {
		tune += ", irqbalance stopped"
	}
	if o.NoTune {
		tune = "none"
	}
	return map[string]string{
		"rounds":  strconv.Itoa(o.Rounds),
		"reps":    strconv.Itoa(o.Reps),
		"pinning": pin,
		"tuning":  tune,
	}
}

// cpuList parses a CPU list such as 4-5,8.
func cpuList(s string) ([]int, error) {
	mask, err := harness.ParseCPUList(strings.TrimSpace(s))
	if err != nil {
		return nil, err
	}
	var cpus []int
	for i, w := range mask {
		for bit := 0; bit < 64; bit++ {
			if w&(1<<bit) != 0 {
				cpus = append(cpus, 64*i+bit)
			}
		}
	}
	return cpus, nil
}

// Pin returns a command that runs on the CPUs, or the command itself for
// "".
func Pin(cpus string, argv ...string) []string {
	if cpus == "" {
		return argv
	}
	return append([]string{"taskset", "-c", cpus}, argv...)
}

// SteerIRQs sets the affinity of a PCI device's interrupts and returns a
// function that restores it. The interrupts, in the order of their
// numbers, take the CPU lists in turn, so that one list sets all of them
// and several spread them. A device without MSI has one interrupt, its
// INTx line.
func SteerIRQs(addr string, cpus ...string) (func(), error) {
	irqs := harness.Glob(pci.Path(addr, "msi_irqs/*"))
	sort.Slice(irqs, func(i, j int) bool {
		a, _ := strconv.Atoi(irqs[i])
		b, _ := strconv.Atoi(irqs[j])
		return a < b
	})
	if len(irqs) == 0 {
		if irq := harness.Read(pci.Path(addr, "irq")); irq != "" && irq != "0" {
			irqs = []string{irq}
		}
	}
	saved := map[string]string{}
	restore := func() {
		for p, v := range saved {
			harness.Write(p, v)
		}
	}
	for _, irq := range irqs {
		p := filepath.Join("/proc/irq", irq, "smp_affinity_list")
		if !harness.Exists(p) {
			continue
		}
		old := harness.Read(p)
		list := cpus[len(saved)%len(cpus)]
		if err := harness.Write(p, list); err != nil {
			restore()
			return nil, fmt.Errorf("cannot steer IRQ %s of %s to CPUs %s: %v", irq, addr, list, err)
		}
		saved[p] = old
	}
	if len(saved) == 0 {
		return nil, fmt.Errorf("%s has no interrupts to steer", addr)
	}
	return restore, nil
}

// Tuning holds what Tune changed, which Restore undoes.
type Tuning struct {
	undo []func()
}

func (t *Tuning) save(path string) {
	old := harness.Read(path)
	t.undo = append(t.undo, func() { harness.Write(path, old) })
}

// Tune sets every CPU's governor to performance and the boost as the
// options say, and stops irqbalance, which would move the interrupts that
// the benchmark places. With amd-pstate in active mode, the performance
// governor also sets the energy-performance preference.
func (o *Options) Tune() (*Tuning, error) {
	t := &Tuning{}
	irqbalance := harness.Run("systemctl", "is-active", "--quiet", "irqbalance").OK()
	if o.NoTune {
		if irqbalance && !o.noPin {
			return nil, fmt.Errorf("irqbalance would move the interrupts that the benchmark places, so stop it or give -no-pin")
		}
		return t, nil
	}
	policies, _ := filepath.Glob("/sys/devices/system/cpu/cpufreq/policy*")
	if len(policies) == 0 {
		return nil, fmt.Errorf("the host has no cpufreq policies, give -no-tune")
	}
	// The policy of an offline CPU refuses reads and writes.
	var active []string
	for _, p := range policies {
		if harness.Read(filepath.Join(p, "scaling_governor")) != "" {
			active = append(active, p)
		}
	}
	for _, p := range active {
		// The preference is restored after the governor, which resets it.
		if epp := filepath.Join(p, "energy_performance_preference"); harness.Exists(epp) {
			t.save(epp)
		}
		gov := filepath.Join(p, "scaling_governor")
		t.save(gov)
		if err := harness.Write(gov, "performance"); err != nil {
			t.Restore()
			return nil, fmt.Errorf("%s: %v", gov, err)
		}
	}
	if o.Boost != "keep" {
		// intel_pstate has no boost file, and its no_turbo is the inverse.
		on := o.Boost == "on"
		values := map[bool]string{true: "1", false: "0"}
		files, _ := filepath.Glob("/sys/devices/system/cpu/cpufreq/boost")
		if len(files) == 0 {
			for _, p := range active {
				if f := filepath.Join(p, "boost"); harness.Exists(f) {
					files = append(files, f)
				}
			}
		}
		value := values[on]
		if len(files) == 0 {
			files, _ = filepath.Glob("/sys/devices/system/cpu/intel_pstate/no_turbo")
			value = values[!on]
		}
		if len(files) == 0 {
			t.Restore()
			return nil, fmt.Errorf("the host cannot switch boost, give -boost keep")
		}
		for _, f := range files {
			t.save(f)
			if err := harness.Write(f, value); err != nil {
				t.Restore()
				return nil, fmt.Errorf("%s: %v", f, err)
			}
		}
	}
	if irqbalance {
		if err := harness.MustRun("systemctl", "stop", "irqbalance"); err != nil {
			t.Restore()
			return nil, err
		}
		t.undo = append(t.undo, func() { harness.Run("systemctl", "start", "irqbalance") })
		o.stoppedIRQBalance = true
	}
	return t, nil
}

// Restore undoes the tuning, last change first.
func (t *Tuning) Restore() {
	for i := len(t.undo) - 1; i >= 0; i-- {
		t.undo[i]()
	}
	t.undo = nil
}

// Environment describes the host as the benchmark runs: its kernel, CPU,
// frequency settings and mitigations, and the commits of the repository and
// both kernel trees.
func Environment() map[string]string {
	cpu := "/sys/devices/system/cpu"
	env := map[string]string{
		"uname":   strings.TrimSpace(harness.Run("uname", "-a").Stdout),
		"cmdline": harness.Read("/proc/cmdline"),
		"online":  harness.Read(cpu + "/online"),
		"smt":     harness.Read(cpu + "/smt/active"),
		"boost":   harness.Read(cpu + "/cpufreq/boost"),
	}
	if v := harness.Read(cpu + "/intel_pstate/no_turbo"); v != "" {
		env["no_turbo"] = v
	}
	if data, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "model name" {
				env["cpu_model"] = strings.TrimSpace(v)
				break
			}
		}
	}
	// The policies' settings, each with the CPUs that have it.
	for _, file := range []string{"scaling_driver", "scaling_governor", "energy_performance_preference"} {
		seen := map[string]bool{}
		var values []string
		files, _ := filepath.Glob(cpu + "/cpufreq/policy*/" + file)
		for _, f := range files {
			if v := harness.Read(f); v != "" && !seen[v] {
				seen[v] = true
				values = append(values, v)
			}
		}
		if len(values) > 0 {
			env[file] = strings.Join(values, ", ")
		}
	}
	var vulns []string
	files, _ := filepath.Glob(cpu + "/vulnerabilities/*")
	for _, f := range files {
		vulns = append(vulns, filepath.Base(f)+": "+harness.Read(f))
	}
	env["vulnerabilities"] = strings.Join(vulns, "\n")
	root := harness.RepoRoot()
	for name, dir := range map[string]string{
		"invirtio":   root,
		"linux-uml":  filepath.Join(root, "linux-uml/linux"),
		"linux-host": filepath.Join(root, "linux-host/linux"),
	} {
		if !harness.Exists(dir) {
			continue
		}
		v := strings.TrimSpace(harness.Run("git", "-C", dir, "log", "-1", "--format=%h %s").Stdout)
		if !harness.Run("git", "-C", dir, "diff", "--quiet", "HEAD").OK() {
			v += " (with uncommitted changes)"
		}
		env[name] = v
	}
	return env
}
