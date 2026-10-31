package harness

import (
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// InNS returns a command that runs in a network namespace, or the command
// itself for the host's namespace "".
func InNS(ns string, argv ...string) []string {
	if ns == "" {
		return argv
	}
	return append([]string{"ip", "netns", "exec", ns}, argv...)
}

// systemSupplicant calls the system wpa_supplicant over D-Bus, without
// starting it.
var systemSupplicant = []string{"busctl", "--auto-start=no", "call", "fi.w1.wpa_supplicant1",
	"/fi/w1/wpa_supplicant1", "fi.w1.wpa_supplicant1"}

var supplicantPathRE = regexp.MustCompile(`^o "(\S+)"`)

// Release takes an interface from NetworkManager and the system
// wpa_supplicant, which would otherwise configure it or end connections that
// they did not make. NetworkManager can still be handing the interface to
// wpa_supplicant, so wpa_supplicant must have been without it for a second.
func Release(net string) {
	Wait(func() bool {
		Run("nmcli", "device", "set", net, "managed", "no")
		r := Run("nmcli", "-g", "GENERAL.STATE", "device", "show", net)
		return strings.Contains(r.Stdout, "unmanaged") || !r.OK()
	}, 5*time.Second, 100*time.Millisecond)
	since := time.Now()
	Wait(func() bool {
		get := append(append([]string(nil), systemSupplicant...), "GetInterface", "s", net)
		if m := supplicantPathRE.FindStringSubmatch(RunArgv(get).Stdout); m != nil {
			RunArgv(append(append([]string(nil), systemSupplicant...), "RemoveInterface", "o", m[1]))
			since = time.Now()
		}
		return time.Since(since) >= time.Second
	}, 10*time.Second, 200*time.Millisecond)
}

// Iperf is the outcome of an iperf3 run with -J.
type Iperf struct {
	// ReceivedBPS and SentBPS are the rates of TCP data, or UDP's sent rate
	// in SentBPS.
	ReceivedBPS float64
	SentBPS     float64
	Retransmits int64
	// LastBytes are the bytes of the last interval.
	LastBytes float64
	// The UDP figures: datagrams sent, lost, and the time of the run.
	Packets     int64
	LostPackets int64
	Seconds     float64
}

// ParseIperf parses iperf3's JSON output.
func ParseIperf(out string) (Iperf, error) {
	var j struct {
		Error     string `json:"error"`
		Intervals []struct {
			Sum struct {
				Bytes float64 `json:"bytes"`
			} `json:"sum"`
		} `json:"intervals"`
		End struct {
			SumReceived *struct {
				BPS float64 `json:"bits_per_second"`
			} `json:"sum_received"`
			SumSent *struct {
				BPS         float64 `json:"bits_per_second"`
				Retransmits int64   `json:"retransmits"`
			} `json:"sum_sent"`
			Sum *struct {
				BPS         float64 `json:"bits_per_second"`
				Packets     int64   `json:"packets"`
				LostPackets int64   `json:"lost_packets"`
				Seconds     float64 `json:"seconds"`
			} `json:"sum"`
		} `json:"end"`
	}
	if err := json.Unmarshal([]byte(out), &j); err != nil {
		return Iperf{}, err
	}
	if j.Error != "" {
		return Iperf{}, errors.New(j.Error)
	}
	var r Iperf
	if j.End.SumReceived != nil {
		r.ReceivedBPS = j.End.SumReceived.BPS
	}
	if j.End.SumSent != nil {
		r.SentBPS, r.Retransmits = j.End.SumSent.BPS, j.End.SumSent.Retransmits
	}
	if s := j.End.Sum; s != nil {
		if r.SentBPS == 0 {
			r.SentBPS = s.BPS
		}
		r.Packets, r.LostPackets, r.Seconds = s.Packets, s.LostPackets, s.Seconds
	}
	if n := len(j.Intervals); n > 0 {
		r.LastBytes = j.Intervals[n-1].Sum.Bytes
	}
	if r.ReceivedBPS == 0 && r.SentBPS == 0 && r.Packets == 0 {
		return r, errors.New("iperf3 reported no transfer")
	}
	return r, nil
}

// Ping is the summary of a ping run, with times in milliseconds.
type Ping struct {
	Received int
	Loss     float64
	Min      float64
	Avg      float64
	Max      float64
	Mdev     float64
}

var (
	pingRecvRE = regexp.MustCompile(`(\d+) (?:packets )?received`)
	pingLossRE = regexp.MustCompile(`([\d.]+)% packet loss`)
	pingRTTRE  = regexp.MustCompile(`= ([\d.]+)/([\d.]+)/([\d.]+)(?:/([\d.]+))? ms`)
)

// ParsePing parses ping's summary, from iputils or busybox, and reports
// whether it had round trip times.
func ParsePing(out string) (Ping, bool) {
	var p Ping
	if m := pingRecvRE.FindStringSubmatch(out); m != nil {
		p.Received, _ = strconv.Atoi(m[1])
	}
	if m := pingLossRE.FindStringSubmatch(out); m != nil {
		p.Loss, _ = strconv.ParseFloat(m[1], 64)
	}
	m := pingRTTRE.FindStringSubmatch(out)
	if m == nil {
		return p, false
	}
	p.Min, _ = strconv.ParseFloat(m[1], 64)
	p.Avg, _ = strconv.ParseFloat(m[2], 64)
	p.Max, _ = strconv.ParseFloat(m[3], 64)
	if m[4] != "" {
		p.Mdev, _ = strconv.ParseFloat(m[4], 64)
	}
	return p, true
}
