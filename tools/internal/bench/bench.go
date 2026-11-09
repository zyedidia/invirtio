// Package bench holds what the benchmarks of invirtio-test share: their
// results and statistics, the CPU use of each run, the placement of the
// benchmark's programs and interrupts on host CPUs, and the tuning and
// description of the host.
package bench

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"invirtio/internal/harness"
)

// A Job is one measurement that a benchmark runs on each target. Metrics
// are the keys of a run's metrics that the summary shows before its CPU
// use.
type Job struct {
	Name    string
	About   string
	Metrics []string
}

// Run is one repetition of one job on one target in one round.
type Run struct {
	Job     string             `json:"job"`
	Target  string             `json:"target"`
	Round   int                `json:"round"`
	Rep     int                `json:"rep"`
	Metrics map[string]float64 `json:"metrics"`
}

// Instance is one bring-up of a target in a round, with what describes the
// device and the guest's state after its jobs.
type Instance struct {
	Target   string               `json:"target"`
	Round    int                  `json:"round"`
	Info     map[string]string    `json:"info,omitempty"`
	Counters map[string]int64     `json:"counters,omitempty"`
	Memory   *harness.GuestMemory `json:"memory,omitempty"`
}

// Results are a benchmark's output. Targets are in the order of the first
// round, and the first one is the baseline of the summary's ratios.
type Results struct {
	Env       map[string]string `json:"env"`
	Targets   []string          `json:"targets"`
	Instances []Instance        `json:"instances"`
	Runs      []Run             `json:"runs"`
}

// Save writes the results to results.json in dir, and the runs to runs.csv
// with one metric of one run on each line.
func (r *Results) Save(dir string) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "results.json"), append(data, '\n'), 0644); err != nil {
		return err
	}
	f, err := os.Create(filepath.Join(dir, "runs.csv"))
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)
	w.Write([]string{"job", "target", "round", "rep", "metric", "value"})
	for _, run := range r.Runs {
		keys := make([]string, 0, len(run.Metrics))
		for k := range run.Metrics {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			w.Write([]string{run.Job, run.Target, strconv.Itoa(run.Round), strconv.Itoa(run.Rep), k,
				strconv.FormatFloat(run.Metrics[k], 'g', -1, 64)})
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Load reads the results.json of an output directory.
func Load(dir string) (*Results, error) {
	data, err := os.ReadFile(filepath.Join(dir, "results.json"))
	if err != nil {
		return nil, err
	}
	var r Results
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// Order returns the targets in the order that a round runs them. Every
// other round reverses the order, so that drift over the run affects each
// target alike.
func Order(targets []string, round int) []string {
	o := append([]string(nil), targets...)
	if round%2 == 0 {
		for i, j := 0, len(o)-1; i < j; i, j = i+1, j-1 {
			o[i], o[j] = o[j], o[i]
		}
	}
	return o
}

// Stats summarize the values of one metric. CI is the half-width of the 95%
// confidence interval of the mean, or 0 for fewer than two values.
type Stats struct {
	N      int
	Mean   float64
	Median float64
	Stdev  float64
	CI     float64
	Min    float64
	Max    float64
}

// Summarize computes the statistics of a list of values.
func Summarize(xs []float64) Stats {
	st := Stats{N: len(xs)}
	if st.N == 0 {
		return st
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	st.Min, st.Max = s[0], s[st.N-1]
	if st.N%2 == 1 {
		st.Median = s[st.N/2]
	} else {
		st.Median = (s[st.N/2-1] + s[st.N/2]) / 2
	}
	for _, x := range s {
		st.Mean += x
	}
	st.Mean /= float64(st.N)
	if st.N < 2 {
		return st
	}
	var ss float64
	for _, x := range s {
		ss += (x - st.Mean) * (x - st.Mean)
	}
	st.Stdev = math.Sqrt(ss / float64(st.N-1))
	st.CI = tCritical(st.N-1) * st.Stdev / math.Sqrt(float64(st.N))
	return st
}

// tTable holds the two-sided 95% critical values of Student's t
// distribution for 1 to 30 degrees of freedom.
var tTable = []float64{12.706, 4.303, 3.182, 2.776, 2.571, 2.447, 2.365, 2.306, 2.262, 2.228,
	2.201, 2.179, 2.160, 2.145, 2.131, 2.120, 2.110, 2.101, 2.093, 2.086,
	2.080, 2.074, 2.069, 2.064, 2.060, 2.056, 2.052, 2.048, 2.045, 2.042}

// tCritical returns the critical value for df degrees of freedom. Beyond
// the table, it takes the value of the next smaller tabulated df, which
// errs toward a wider interval.
func tCritical(df int) float64 {
	switch {
	case df < 1:
		return math.NaN()
	case df <= len(tTable):
		return tTable[df-1]
	case df < 40:
		return 2.042
	case df < 60:
		return 2.021
	case df < 120:
		return 2.000
	}
	return 1.980
}

// FormatValue formats a value with about three significant digits and at
// most two decimals.
func FormatValue(v float64) string {
	switch a := math.Abs(v); {
	case a >= 100:
		return strconv.FormatFloat(v, 'f', 0, 64)
	case a >= 10:
		return strconv.FormatFloat(v, 'f', 1, 64)
	}
	return strconv.FormatFloat(v, 'f', 2, 64)
}

// Format describes a run's metrics in one line, the job's metrics first and
// then its CPU use and the guest's DMA mappings.
func Format(j Job, m map[string]float64) string {
	var parts []string
	keys := append(append([]string(nil), j.Metrics...), CPUKeys...)
	for _, k := range append(keys, DMAKeys...) {
		if v, ok := m[k]; ok {
			parts = append(parts, k+" "+FormatValue(v))
		}
	}
	return strings.Join(parts, ", ")
}

// table formats rows of cells in columns, the first aligned to the left and
// the others to the right, each line indented by two spaces.
func table(rows [][]string) string {
	var widths []int
	for _, row := range rows {
		for i, c := range row {
			if i >= len(widths) {
				widths = append(widths, 0)
			}
			widths[i] = max(widths[i], len(c))
		}
	}
	var b strings.Builder
	for _, row := range rows {
		b.WriteString(" ")
		for i, c := range row {
			if i == 0 {
				fmt.Fprintf(&b, " %-*s", widths[i], c)
			} else {
				fmt.Fprintf(&b, "  %*s", widths[i], c)
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

// values returns a metric's value in each round of a job on a target, the
// mean of the round's repetitions, and whether any run had it. Rounds are
// the independent samples, since each brings the target up again.
func (r *Results) values(job, target, metric string) ([]float64, bool) {
	sums := map[int]float64{}
	counts := map[int]int{}
	var rounds []int
	for _, run := range r.Runs {
		if run.Job != job || run.Target != target {
			continue
		}
		if v, ok := run.Metrics[metric]; ok {
			if counts[run.Round] == 0 {
				rounds = append(rounds, run.Round)
			}
			sums[run.Round] += v
			counts[run.Round]++
		}
	}
	var xs []float64
	for _, n := range rounds {
		xs = append(xs, sums[n]/float64(counts[n]))
	}
	return xs, len(xs) > 0
}

// means formats a table of the mean of each of the keys that a job's runs
// on the targets have, or "" if they have none.
func (r *Results) means(job string, targets []string, label string, keys []string) string {
	var have []string
	for _, k := range keys {
		for _, t := range targets {
			if _, ok := r.values(job, t, k); ok {
				have = append(have, k)
				break
			}
		}
	}
	if len(have) == 0 {
		return ""
	}
	rows := [][]string{append([]string{label}, have...)}
	for _, t := range targets {
		row := []string{t}
		for _, k := range have {
			cell := "-"
			if xs, ok := r.values(job, t, k); ok {
				cell = FormatValue(Summarize(xs).Mean)
			}
			row = append(row, cell)
		}
		rows = append(rows, row)
	}
	return table(rows)
}

// Summary shows, for each job, the mean over the rounds of each metric on
// each target with the half-width of its confidence interval and its ratio
// to the first target's mean, and then the mean CPU use and the guest's DMA
// mappings. It ends with the largest memory of each target's guest.
func (r *Results) Summary(jobs []Job) string {
	var b strings.Builder
	for _, j := range jobs {
		var targets []string
		for _, t := range r.Targets {
			for _, run := range r.Runs {
				if run.Job == j.Name && run.Target == t {
					targets = append(targets, t)
					break
				}
			}
		}
		if len(targets) == 0 {
			continue
		}
		fmt.Fprintf(&b, "%s: %s\n", j.Name, j.About)
		rows := [][]string{append([]string{"target"}, j.Metrics...)}
		for _, t := range targets {
			row := []string{t}
			for _, k := range j.Metrics {
				xs, _ := r.values(j.Name, t, k)
				st := Summarize(xs)
				cell := FormatValue(st.Mean)
				if st.N > 1 {
					cell += " +-" + FormatValue(st.CI)
				}
				if t != targets[0] {
					base, _ := r.values(j.Name, targets[0], k)
					if bm := Summarize(base).Mean; bm != 0 {
						cell += fmt.Sprintf(" (%.2fx)", st.Mean/bm)
					}
				}
				row = append(row, cell)
			}
			rows = append(rows, row)
		}
		b.WriteString(table(rows))
		b.WriteString(r.means(j.Name, targets, "cpu", CPUKeys))
		b.WriteString(r.means(j.Name, targets, "dma", DMAKeys))
		b.WriteString("\n")
	}
	b.WriteString("Each value is the mean over the rounds of each round's mean, with the\n" +
		"half-width of its 95% confidence interval and its ratio to the first\n" +
		"target. cpu_host is how many of the host's CPUs were busy, and cpu_app,\n" +
		"cpu_device and cpu_peer how many of the CPUs of each set. cpu_guest is\n" +
		"how many CPUs the guest's cgroup took, and guest_busy how busy the\n" +
		"guest's own CPU was. dma_maps_per_s is how many buffers the guest\n" +
		"mapped into the host's IOMMU each second, and dma_premapped_per_s how\n" +
		"many the eagerly mapped RAM served.\n")
	for _, t := range r.Targets {
		var top *harness.GuestMemory
		n := 0
		for _, in := range r.Instances {
			if in.Target == t && in.Memory != nil {
				n++
				if top == nil || in.Memory.PeakBytes > top.PeakBytes {
					top = in.Memory
				}
			}
		}
		if top != nil {
			fmt.Fprintf(&b, "\n%s memory, the largest of %d rounds, %s\n", t, n, top.Summary())
		}
	}
	return b.String()
}
