package bench

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"invirtio/internal/harness"
)

// CPUKeys are the metrics of a run's CPU use, in the order that summaries
// show them. Each set of host CPUs has the key cpu_<set>, and guest_busy is
// recorded by benchmarks that read the guest's own CPU time.
var CPUKeys = []string{"cpu_host", "cpu_app", "cpu_device", "cpu_peer", "cpu_guest", "guest_busy"}

// DMAKeys are the metrics of a run's DMA mappings in the guest.
var DMAKeys = []string{"dma_maps_per_s", "dma_premapped_per_s"}

// DMARates returns the rates of the guest's DMA mappings between two
// readings of the dma counters of its /proc/vfio_uml.
func DMARates(before, after map[string]int64, elapsed time.Duration) map[string]float64 {
	out := map[string]float64{}
	if elapsed <= 0 || len(before) == 0 || len(after) == 0 {
		return out
	}
	for _, k := range []string{"maps", "premapped"} {
		if b, ok := before[k]; ok {
			out["dma_"+k+"_per_s"] = float64(after[k]-b) / elapsed.Seconds()
		}
	}
	return out
}

// A Set is a named list of host CPUs whose use a Meter measures.
type Set struct {
	Name string
	CPUs []int
}

// A Meter measures the CPU use of the host, of sets of its CPUs, and of a
// cgroup over an interval.
type Meter struct {
	sets   []Set
	cgroup string
	start  time.Time
	host   [2]int64
	cpus   map[int][2]int64
	usage  int64
}

// StartMeter starts measuring. The cgroup is the guest's, or "".
func StartMeter(sets []Set, cgroup string) *Meter {
	m := &Meter{sets: sets, cgroup: cgroup, start: time.Now()}
	m.host, m.cpus = readStat()
	if cgroup != "" {
		m.usage = CgroupUsage(cgroup)
	}
	return m
}

// Stop returns the CPU use since the start, each as a number of CPUs: how
// many of the host's CPUs were busy, how many of each set's, and how many
// the cgroup took.
func (m *Meter) Stop() map[string]float64 {
	host, cpus := readStat()
	elapsed := time.Since(m.start).Seconds()
	out := map[string]float64{}
	if total := host[1] - m.host[1]; total > 0 {
		out["cpu_host"] = float64(host[0]-m.host[0]) / float64(total) * float64(len(cpus))
	}
	for _, s := range m.sets {
		var busy float64
		for _, c := range s.CPUs {
			a, b := m.cpus[c], cpus[c]
			if b[1] > a[1] {
				busy += float64(b[0]-a[0]) / float64(b[1]-a[1])
			}
		}
		out["cpu_"+s.Name] = busy
	}
	if m.cgroup != "" && elapsed > 0 {
		out["cpu_guest"] = float64(CgroupUsage(m.cgroup)-m.usage) / 1e6 / elapsed
	}
	return out
}

// readStat returns the busy and total ticks of the host and of each CPU
// from /proc/stat.
func readStat() ([2]int64, map[int][2]int64) {
	data, _ := os.ReadFile("/proc/stat")
	return parseStat(string(data))
}

func parseStat(text string) ([2]int64, map[int][2]int64) {
	var host [2]int64
	cpus := map[int][2]int64{}
	for _, line := range strings.Split(text, "\n") {
		name, _, _ := strings.Cut(line, " ")
		if !strings.HasPrefix(name, "cpu") {
			continue
		}
		busy, total := harness.ParseCPULine(line)
		if name == "cpu" {
			host = [2]int64{busy, total}
		} else if n, err := strconv.Atoi(name[3:]); err == nil {
			cpus[n] = [2]int64{busy, total}
		}
	}
	return host, cpus
}

// CgroupUsage returns the CPU time that a cgroup's processes took, in
// microseconds, from its cpu.stat, which every cgroup has.
func CgroupUsage(dir string) int64 {
	data, err := os.ReadFile(filepath.Join(dir, "cpu.stat"))
	if err != nil {
		return 0
	}
	return harness.ParseKeyed(string(data))["usage_usec"]
}
