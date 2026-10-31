package harness

import "testing"

func TestParseIperfTCP(t *testing.T) {
	out := `{"intervals": [{"sum": {"bytes": 100}}, {"sum": {"bytes": 250}}],
		"end": {"sum_received": {"bits_per_second": 9.4e9},
			"sum_sent": {"bits_per_second": 9.5e9, "retransmits": 12}}}`
	r, err := ParseIperf(out)
	if err != nil || r.ReceivedBPS != 9.4e9 || r.SentBPS != 9.5e9 || r.Retransmits != 12 || r.LastBytes != 250 {
		t.Errorf("ParseIperf = %+v, %v", r, err)
	}
}

func TestParseIperfUDP(t *testing.T) {
	out := `{"end": {"sum": {"bits_per_second": 4e8, "packets": 1000000, "lost_packets": 2500, "seconds": 10}}}`
	r, err := ParseIperf(out)
	if err != nil || r.Packets != 1000000 || r.LostPackets != 2500 || r.Seconds != 10 || r.SentBPS != 4e8 {
		t.Errorf("ParseIperf = %+v, %v", r, err)
	}
}

func TestParseIperfError(t *testing.T) {
	if _, err := ParseIperf(`{"error": "unable to connect to server"}`); err == nil {
		t.Error("an iperf3 error parsed")
	}
	if _, err := ParseIperf(`not json`); err == nil {
		t.Error("garbage parsed")
	}
}

func TestParsePing(t *testing.T) {
	iputils := "5 packets transmitted, 5 received, 0% packet loss, time 804ms\n" +
		"rtt min/avg/max/mdev = 0.031/0.045/0.061/0.010 ms\n"
	p, ok := ParsePing(iputils)
	if !ok || p.Received != 5 || p.Loss != 0 || p.Avg != 0.045 || p.Mdev != 0.010 {
		t.Errorf("iputils ParsePing = %+v, %v", p, ok)
	}
	busybox := "3 packets transmitted, 2 packets received, 33% packet loss\n" +
		"round-trip min/avg/max = 0.101/0.150/0.200 ms\n"
	p, ok = ParsePing(busybox)
	if !ok || p.Received != 2 || p.Loss != 33 || p.Max != 0.2 {
		t.Errorf("busybox ParsePing = %+v, %v", p, ok)
	}
	if _, ok := ParsePing("ping: connect: Network is unreachable"); ok {
		t.Error("a failed ping parsed")
	}
}
