package bench

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-3 }

func TestSummarize(t *testing.T) {
	st := Summarize([]float64{4, 1, 3, 2})
	if st.N != 4 || st.Mean != 2.5 || st.Median != 2.5 || st.Min != 1 || st.Max != 4 {
		t.Errorf("Summarize = %+v", st)
	}
	// The sample deviation of 1 to 4 is sqrt(5/3), and t for 3 degrees of
	// freedom is 3.182.
	if !near(st.Stdev, 1.291) || !near(st.CI, 3.182*1.291/2) {
		t.Errorf("Stdev %v, CI %v", st.Stdev, st.CI)
	}
	if one := Summarize([]float64{7}); one.Mean != 7 || one.CI != 0 || one.Median != 7 {
		t.Errorf("Summarize one = %+v", one)
	}
	if tCritical(35) != 2.042 || tCritical(100) != 2.000 || tCritical(1000) != 1.980 {
		t.Error("tCritical")
	}
}

func TestOrder(t *testing.T) {
	ts := []string{"a", "b", "c"}
	if got := strings.Join(Order(ts, 1), ""); got != "abc" {
		t.Errorf("round 1 = %s", got)
	}
	if got := strings.Join(Order(ts, 2), ""); got != "cba" {
		t.Errorf("round 2 = %s", got)
	}
	if ts[0] != "a" {
		t.Error("Order changed its argument")
	}
}

func TestParseStat(t *testing.T) {
	host, cpus := parseStat("cpu  10 0 10 70 10 0 0 0 0 0\n" +
		"cpu0 5 0 5 30 10 0 0 0 0 0\ncpu3 5 0 5 40 0 0 0 0 0 0\nintr 1 2\n")
	if host != [2]int64{20, 100} {
		t.Errorf("host = %v", host)
	}
	if cpus[0] != [2]int64{10, 50} || cpus[3] != [2]int64{10, 50} || len(cpus) != 2 {
		t.Errorf("cpus = %v", cpus)
	}
}

func TestCPUList(t *testing.T) {
	cpus, err := cpuList("1,4-6,70")
	if err != nil || len(cpus) != 5 || cpus[0] != 1 || cpus[3] != 6 || cpus[4] != 70 {
		t.Errorf("cpuList = %v, %v", cpus, err)
	}
	if strings.Join(Pin("4", "ping", "x"), " ") != "taskset -c 4 ping x" || len(Pin("", "ping")) != 1 {
		t.Error("Pin")
	}
}

func TestResults(t *testing.T) {
	r := &Results{Targets: []string{"native", "guest"}}
	for i, v := range []float64{9, 11, 5, 5} {
		target := "native"
		if i >= 2 {
			target = "guest"
		}
		r.Runs = append(r.Runs, Run{Job: "tcp", Target: target, Round: i%2 + 1, Rep: 1,
			Metrics: map[string]float64{"gbps": v, "cpu_host": 1}})
	}
	sum := r.Summary([]Job{{Name: "tcp", About: "TCP", Metrics: []string{"gbps"}}, {Name: "none"}})
	for _, want := range []string{"tcp: TCP", "10.0 +-12.7", "5.00 +-0.00 (0.50x)", "cpu_host"} {
		if !strings.Contains(sum, want) {
			t.Errorf("the summary has no %q:\n%s", want, sum)
		}
	}
	if strings.Contains(sum, "none:") {
		t.Errorf("the summary shows a job without runs:\n%s", sum)
	}
	dir := t.TempDir()
	if err := r.Save(dir); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "runs.csv"))
	if !strings.HasPrefix(string(data), "job,target,round,rep,metric,value\ntcp,native,1,1,cpu_host,1\ntcp,native,1,1,gbps,9\n") {
		t.Errorf("runs.csv = %s", data)
	}
	back, err := Load(dir)
	if err != nil || len(back.Runs) != 4 || back.Runs[3].Metrics["gbps"] != 5 {
		t.Errorf("Load = %+v, %v", back, err)
	}
}

func TestDMARates(t *testing.T) {
	r := DMARates(map[string]int64{"maps": 100, "cached": 10, "premapped": 0},
		map[string]int64{"maps": 300, "cached": 30, "premapped": 50}, 2*time.Second)
	if r["dma_maps_per_s"] != 100 || r["dma_cached_per_s"] != 10 || r["dma_premapped_per_s"] != 25 {
		t.Errorf("DMARates = %v", r)
	}
	if len(DMARates(nil, map[string]int64{"maps": 1}, time.Second)) != 0 {
		t.Error("DMARates without a first reading")
	}
}
