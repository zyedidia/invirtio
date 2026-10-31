package main

import "testing"

func TestParseNetperf(t *testing.T) {
	m, err := parseNetperf("MIGRATED TCP REQUEST/RESPONSE TEST\n41234.56,23.95,31\n", "")
	if err != nil || m["tps"] != 41234.56 || m["mean_us"] != 23.95 || m["p99_us"] != 31 {
		t.Errorf("parseNetperf = %v, %v", m, err)
	}
	if _, err := parseNetperf("netperf: cannot connect\n", ""); err == nil {
		t.Error("parseNetperf accepts an error")
	}
}
