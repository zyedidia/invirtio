package main

import (
	"strings"
	"testing"
)

func TestBenchParseFio(t *testing.T) {
	data := []byte(`{"jobs": [{"job_runtime": 5000,
		"read": {"io_bytes": 4096000, "iops": 200000.5, "bw_bytes": 819200000, "total_ios": 1000000,
			"lat_ns": {"mean": 4800.25},
			"clat_ns": {"percentile": {"50.000000": 4500, "99.000000": 9000, "99.900000": 15000}}},
		"write": {"io_bytes": 0, "iops": 0}}]}`)
	r, err := benchParseFio(data)
	if err != nil {
		t.Fatal(err)
	}
	if r.IOPS != 200000.5 || r.BW != 819200000 || r.Mean != 4800.25 || r.P50 != 4500 ||
		r.P99 != 9000 || r.P999 != 15000 || r.IOs != 1000000 || r.TimeMS != 5000 {
		t.Errorf("parsed %+v", r)
	}
	w, err := benchParseFio([]byte(`{"jobs": [{"read": {"io_bytes": 0}, "write": {"io_bytes": 5, "iops": 7}}]}`))
	if err != nil || w.IOPS != 7 {
		t.Errorf("a write job parsed as %+v, %v", w, err)
	}
	if _, err := benchParseFio([]byte(`{"jobs": []}`)); err == nil {
		t.Error("output without jobs parsed")
	}
}

func TestBenchParseStat(t *testing.T) {
	stat := "1234 (linux (uml) x) S 1 1234 1234 0 -1 4194560 100 0 0 0 250 75 0 0 20 0 3 0"
	if got := benchParseStat(stat); got != [2]int64{250, 75} {
		t.Errorf("benchParseStat = %v", got)
	}
}

func TestBenchCounterDelta(t *testing.T) {
	before := "device vblk-bench\n  starts 1\n  queue 0\n    kicks 10\n    requests 100\n  queue 1\n    kicks 5\n    requests 50\n"
	after := "device vblk-bench\n  starts 1\n  queue 0\n    kicks 15\n    requests 300\n  queue 1\n    kicks 6\n    requests 80\n"
	d := benchCounterDelta(before, after)
	if d["kicks"] != 6 || d["requests"] != 230 || d["polled"] != 0 {
		t.Errorf("benchCounterDelta = %v", d)
	}
}

func TestBenchMasks(t *testing.T) {
	m, err := benchParseCPUs("0-3,8,40")
	if err != nil {
		t.Fatal(err)
	}
	if got := benchFormatMask(m); got != "00000100,0000010f" {
		t.Errorf("format = %q", got)
	}
	p, err := benchParseMask("00000100,0000010f")
	if err != nil || benchFormatMask(p) != "00000100,0000010f" {
		t.Errorf("parse = %v, %v", p, err)
	}
	if got := benchFormatMask(benchSetBit(nil, 3)); got != "00000008" {
		t.Errorf("one CPU = %q", got)
	}
	if !benchIntersects(m, benchSetBit(nil, 40)) || benchIntersects(m, benchSetBit(nil, 5)) {
		t.Error("benchIntersects is wrong")
	}
}

func TestBenchSummary(t *testing.T) {
	r := &BenchResults{
		Host: map[string]string{"clk_tck": "100", "ramp": "1"},
		Targets: []BenchTarget{
			{Name: "native", Runs: []BenchRun{
				{Job: "lat-qd1", Rep: 1, IOPS: 100, BW: 1 << 20, Mean: 2000},
				{Job: "lat-qd1", Rep: 2, IOPS: 120, BW: 1 << 20, Mean: 2000},
				{Job: "lat-qd1", Rep: 3, IOPS: 110, BW: 1 << 20, Mean: 2000},
			}},
			{Name: "uml", Runs: []BenchRun{
				{Job: "lat-qd1", Rep: 1, IOPS: 50, IOs: 400, TimeMS: 4000, HasTicks: true,
					UserTicks: 1, SysTicks: 4, Counters: map[string]int64{"kicks": 9}},
			}},
		},
	}
	s := benchSummary(r)
	for _, want := range []string{"native", "110", "18.2%", "uml", "uml counters", "kicks"} {
		if !strings.Contains(s, want) {
			t.Errorf("the summary lacks %q:\n%s", want, s)
		}
	}
	// 5 ticks at 100 Hz over 400 I/Os scaled to 5 seconds is 10 ms / 500.
	if !strings.Contains(s, "100.00") {
		t.Errorf("the summary lacks the CPU time per I/O:\n%s", s)
	}
}

func TestBlkReplays(t *testing.T) {
	log := "vduse-guest: vblk-test: reconnecting to the blk device\r\n" +
		"vduse-guest: vblk-test: queue 0 has 12 requests to replay\r\n" +
		"vduse-guest: vblk-test: queue 1 has 0 requests to replay\n" +
		"vduse-guest: other: queue 0 has 5 requests to replay\n"
	if n := blkReplays(log); n != 12 {
		t.Errorf("blkReplays = %d, want 12", n)
	}
}
