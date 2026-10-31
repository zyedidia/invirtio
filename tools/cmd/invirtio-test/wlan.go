package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"invirtio/internal/harness"
)

func init() {
	register("test-wlan", "test the host's virtio_wlan against the hwsim guest", cmdTestWlan)
}

const (
	wlanName       = "vwlan-test"
	wlanCountry    = "DE"
	wlanServer     = "192.168.100.1"
	wlanPassphrase = "invirtio-hwsim"
)

// wlanNetworks are the guest's networks.
var wlanNetworks = func() []string {
	var n []string
	for _, band := range []string{"2g", "5g"} {
		for _, kind := range []string{"open", "wpa2", "wpa2-mfp", "wpa3-transition"} {
			n = append(n, "hwsim-"+band+"-"+kind)
		}
	}
	return append(n, "hwsim-6g-wpa3")
}()

// wlanCounters are the driver's error counters, which stay at zero.
// stale_events, which counts the events of connections that already ended,
// is checked by the tests that end connections.
var wlanCounters = []string{"cmd_timeouts", "bad_responses", "bad_events", "bad_bss",
	"bad_connect_results", "bad_rates", "scan_timeouts", "connect_timeouts", "bad_rx_frames",
	"tx_timeouts", "rx_post_errors", "eapol_timeouts"}

// wlanGuestCounters are the backend's error counters, which stay at zero.
var wlanGuestCounters = []string{"tx_invalid", "tx_spoofed", "tx_dropped", "rx_oversized", "rx_bad_buffers"}

// wlanWPA are the networks that wpa_supplicant joins, with its MFP setting
// for each. The WPA2-PSK networks renew their pairwise key every 6 seconds
// and their group key every 4.
var wlanWPA = []struct {
	ssid   string
	mfp    int
	rekeys bool
}{
	{"hwsim-2g-wpa2", 0, true}, {"hwsim-2g-wpa2-mfp", 2, false}, {"hwsim-5g-wpa2", 0, true},
	{"hwsim-5g-wpa2-mfp", 2, false}, {"hwsim-2g-wpa3-transition", 1, false},
}

// wlanUdhcpc is the script of the host's DHCP client. The lease adds only
// the address and its subnet's route.
const wlanUdhcpc = `#!/bin/sh
[ "$1" = bound ] && ip addr add "$ip/$mask" dev "$interface"
exit 0
`

type wlanTest struct {
	name  string
	about string
	run   func(*wlanSuite)
}

var wlanTests = []wlanTest{
	{"wiphy", "the wiphy has the guest's bands and ciphers, and the interface's state and settings reach the guest", wlanTestWiphy},
	{"connect", "a scan finds every network on its band, and open networks pass traffic", wlanTestConnect},
	{"wpa", "wpa_supplicant joins WPA2 and WPA3 networks, renews keys and passes traffic", wlanTestWPA},
	{"reset", "wpa_supplicant joins again after the guest's radio restarts", wlanTestReset},
	{"remove", "a connected device is removed and attached again", wlanTestRemove},
	{"kill", "a guest killed under traffic ends the connection, and the broken device fails fast", wlanTestKill},
	{"gone", "removing the guest's interface ends the connection, and the device fails without a timeout", wlanTestGone},
}

type wlanSuite struct {
	harness.Checker
	build string
	out   string
	// country is the host's country before the test, which it restores.
	country string
	// The test that runs, its directory, the directory of its in-flight
	// log, and the guests and host processes it started, which a second
	// signal kills.
	dir    string
	logDir string
	mu     sync.Mutex
	guests []*harness.Guest
	procs  []*exec.Cmd
	// ctx ends on a signal, which stops the commands that send traffic.
	ctx context.Context
}

func cmdTestWlan(args []string) error {
	root := harness.RepoRoot()
	fs := newFlags("test-wlan", `
usage: invirtio-test test-wlan [options]

Test the host's virtio_wlan against a guest that exports a mac80211_hwsim
radio and runs access points on three others, with a DHCP server and an
iperf3 server behind them. Each test starts its own guest under the
launcher and leaves no device behind. The host's country is set to DE for
the 6 GHz band and restored afterwards. Build the guest with
scripts/invirtio-build-guest.sh -t wlan-test. Every test checks that the
driver's and the guest's error counters stay at zero and that neither
kernel logs a warning. The guests' consoles go to <test>/guest-<n>.log in
the output directory. Run as root on the linux-host kernel, with iw,
wpa_supplicant, iperf3 and busybox, and with debugfs mounted.

Tests:`+wlanTestList())
	build := fs.String("guest", filepath.Join(root, "linux-uml/build-wlan-test"), "the build directory of the guest")
	out := fs.String("out", filepath.Join(root, "build/test-wlan"), "the output directory")
	runRE := fs.String("run", "", "run only the tests whose names match this regular expression")
	fs.Parse(args)
	if fs.NArg() != 0 {
		fs.Usage()
		os.Exit(2)
	}
	match, err := regexp.Compile(*runRE)
	if err != nil {
		return err
	}
	requireRoot()

	s := &wlanSuite{build: *build}
	if s.out, err = filepath.Abs(*out); err != nil {
		return err
	}
	if err := s.setup(); err != nil {
		return err
	}
	defer s.teardown()

	// A signal stops the traffic, and the test that runs then fails and
	// cleans up, and no other test runs. A second signal kills the guests
	// and exits.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.ctx = ctx
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	go func() {
		<-sigs
		fmt.Println("stopping after a signal, a second one kills the guests")
		cancel()
		<-sigs
		s.mu.Lock()
		for _, g := range s.guests {
			g.Kill()
		}
		for _, cmd := range s.procs {
			cmd.Process.Kill()
		}
		s.mu.Unlock()
		harness.Cleanup(wlanName)
		s.teardown()
		harness.CloseOutput()
		os.Exit(1)
	}()

	mark := harness.KernelMark()
	for _, t := range wlanTests {
		if !match.MatchString(t.name) || ctx.Err() != nil {
			continue
		}
		fmt.Printf("== %s: %s\n", t.name, t.about)
		s.runTest(t)
	}
	if ctx.Err() != nil {
		return fmt.Errorf("stopped by a signal, %d check(s) failed", len(s.Failures()))
	}
	warnings := harness.Warnings(harness.KernelLogSince(mark))
	for _, w := range warnings {
		fmt.Println("warning: " + w)
	}
	s.Check(len(warnings) == 0, "the host's kernel logs no warning")
	return s.Report()
}

func wlanTestList() string {
	var b strings.Builder
	for _, t := range wlanTests {
		fmt.Fprintf(&b, "\n  %-8s %s", t.name, t.about)
	}
	return b.String()
}

var regCountryRE = regexp.MustCompile(`^global\ncountry ([A-Z]{2}|00):`)

// globalRegdom returns the global part of iw reg get.
func globalRegdom() string {
	out := harness.Run("iw", "reg", "get").Stdout
	if !strings.HasPrefix(out, "global") {
		return ""
	}
	if i := strings.Index(out, "\n\n"); i >= 0 {
		return out[:i]
	}
	return out
}

func (s *wlanSuite) setup() error {
	for _, tool := range []string{"iw", "wpa_supplicant", "wpa_cli", "iperf3", "busybox", "vdpa", "ip", "ping"} {
		if harness.Run("sh", "-c", "command -v "+tool).Code != 0 {
			return fmt.Errorf("%s is not installed", tool)
		}
	}
	if err := harness.Modprobe("cfg80211", "vduse", "vdpa", "virtio_vdpa", "virtio_wlan"); err != nil {
		return err
	}
	if !harness.Exists("/sys/kernel/debug/ieee80211") {
		return fmt.Errorf("debugfs is not mounted at /sys/kernel/debug")
	}
	if err := os.MkdirAll(s.out, 0755); err != nil {
		return err
	}
	if err := harness.TeeOutput(filepath.Join(s.out, "test.log")); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(s.out, "udhcpc.sh"), []byte(wlanUdhcpc), 0755); err != nil {
		return err
	}
	// A country that cannot be set again, such as the intersection 98, is
	// cleared to the world domain afterwards.
	s.country = "00"
	if m := regCountryRE.FindStringSubmatch(globalRegdom()); m != nil {
		s.country = m[1]
	}
	// An intersection with another hint shows as country 98, so the test
	// waits for the 6 GHz rule.
	harness.Run("iw", "reg", "set", wlanCountry)
	if !harness.Wait(func() bool { return strings.Contains(globalRegdom(), "(5945 - ") },
		5*time.Second, 100*time.Millisecond) {
		s.teardown()
		return fmt.Errorf("setting the host's country to %s does not permit 6 GHz", wlanCountry)
	}
	return nil
}

// teardown restores the host's country.
func (s *wlanSuite) teardown() {
	if s.country != "" {
		harness.Run("iw", "reg", "set", s.country)
	}
}

// runTest runs a test from a clean host, and afterwards removes what its
// guests left and checks their logs.
func (s *wlanSuite) runTest(t wlanTest) {
	harness.Cleanup(wlanName)
	s.dir = filepath.Join(s.out, t.name)
	os.RemoveAll(s.dir)
	os.MkdirAll(s.dir, 0755)
	var err error
	s.logDir, err = harness.MakeLogDir(wlanName)
	if !s.Check(err == nil, "%s: a directory for the in-flight log is made%s", t.name, errSuffix(err)) {
		return
	}
	s.mu.Lock()
	s.guests, s.procs = nil, nil
	s.mu.Unlock()

	t.run(s)

	harness.Detach(wlanName, true)
	for _, g := range s.guests {
		g.Stop(30 * time.Second)
		g.Kill()
	}
	harness.Cleanup(wlanName)
	os.RemoveAll(s.logDir)
	var warnings []string
	for _, g := range s.guests {
		warnings = append(warnings, harness.Warnings(strings.Split(g.Log(), "\n"))...)
	}
	for _, w := range warnings {
		fmt.Println("warning: " + w)
	}
	s.Check(len(warnings) == 0, "%s: no guest logs a kernel warning", t.name)
}

// start starts a guest under the launcher and waits until it has created
// the device and its access points beacon.
func (s *wlanSuite) start() *harness.Guest {
	n := len(s.guests) + 1
	if s.ctx.Err() != nil {
		return nil
	}
	g, err := harness.LaunchGuest(filepath.Join(s.dir, fmt.Sprintf("guest-%d.log", n)), harness.Launch{
		Build:   s.build,
		Type:    "wlan",
		Name:    wlanName,
		Options: "netdev=wlan0",
		LogDir:  s.logDir,
		Args: []string{"mem=768M", "mac80211_hwsim.radios=4",
			"cfg80211.ieee80211_regdom=" + wlanCountry, "init_shell"},
	})
	if !s.Check(err == nil, "guest %d starts%s", n, errSuffix(err)) {
		return nil
	}
	s.mu.Lock()
	s.guests = append(s.guests, g)
	s.mu.Unlock()
	ready := harness.Wait(func() bool {
		log := g.Log()
		return strings.Contains(log, wlanName+": created wlan device") &&
			strings.Contains(log, "wlan-hwsim-init: ready") || !g.Running()
	}, 2*time.Minute, 200*time.Millisecond)
	if !s.Check(ready && g.Running(), "guest %d creates the device and starts its access points", n) {
		return nil
	}
	return g
}

// attach adds the device to the vDPA bus and returns its wiphy and its
// interface, which is down.
func (s *wlanSuite) attach() (phy, net string) {
	err := harness.Attach(wlanName)
	if !s.Check(err == nil, "vdpa dev add attaches the device%s", errSuffix(err)) {
		return "", ""
	}
	dev := filepath.Join("/sys/bus/vdpa/devices", wlanName)
	harness.Wait(func() bool {
		phys := harness.Glob(filepath.Join(dev, "virtio*/ieee80211/*"))
		nets := harness.Glob(filepath.Join(dev, "virtio*/net/*"))
		if len(phys) > 0 && len(nets) > 0 {
			phy, net = filepath.Base(phys[0]), filepath.Base(nets[0])
		}
		return net != ""
	}, 10*time.Second, 100*time.Millisecond)
	if !s.Check(net != "", "virtio_wlan registers a wiphy and an interface") {
		return "", ""
	}
	harness.Release(net)
	// A new wiphy starts blocked while the host's WiFi is switched off.
	softs, _ := filepath.Glob(filepath.Join("/sys/class/ieee80211", phy, "rfkill*/soft"))
	for _, soft := range softs {
		harness.Write(soft, "0")
	}
	harness.Run("ip", "link", "set", net, "down")
	return phy, net
}

// up starts a guest, attaches its device and brings the interface up.
func (s *wlanSuite) up() (g *harness.Guest, phy, net string) {
	if g = s.start(); g == nil {
		return nil, "", ""
	}
	if phy, net = s.attach(); net == "" {
		return nil, "", ""
	}
	if err := harness.MustRun("ip", "link", "set", net, "up"); !s.Check(err == nil, "%s comes up%s", net, errSuffix(err)) {
		return nil, "", ""
	}
	return g, phy, net
}

// wlanCounter returns one of the driver's counters, or -1.
func wlanCounter(phy, name string) int64 {
	v, err := strconv.ParseInt(harness.Read(filepath.Join("/sys/kernel/debug/ieee80211", phy, name)), 10, 64)
	if err != nil {
		return -1
	}
	return v
}

// wlanSettledCounter returns a counter once it has not changed for a second.
// The guest reports a disconnection after it has answered the command that
// asked for it.
func wlanSettledCounter(phy, name string) int64 {
	v := wlanCounter(phy, name)
	since, end := time.Now(), time.Now().Add(10*time.Second)
	for time.Since(since) < time.Second && time.Now().Before(end) {
		time.Sleep(200 * time.Millisecond)
		if now := wlanCounter(phy, name); now != v {
			v, since = now, time.Now()
		}
	}
	return v
}

// healthy checks the driver's and the backend's error counters.
func (s *wlanSuite) healthy(g *harness.Guest, phy string) {
	var bad []string
	for _, name := range wlanCounters {
		if v := wlanCounter(phy, name); v != 0 {
			bad = append(bad, fmt.Sprintf("%s %d", name, v))
		}
	}
	s.Check(len(bad) == 0, "the driver's error counters are zero%s", listSuffix("but", bad))
	c := g.Counters()
	bad = nil
	for _, name := range wlanGuestCounters {
		if v, ok := c[name]; !ok || v != 0 {
			bad = append(bad, fmt.Sprintf("%s %d", name, v))
		}
	}
	s.Check(len(c) > 0 && len(bad) == 0, "the backend's error counters are zero%s", listSuffix("but", bad))
}

// guestSays reports whether a guest command's output comes to hold text.
func guestSays(g *harness.Guest, cmd, text string) bool {
	return harness.Wait(func() bool {
		return strings.Contains(g.Output(cmd, 10*time.Second), text)
	}, 5*time.Second, 500*time.Millisecond)
}

func wlanCarrier(net string) bool {
	return harness.Read(filepath.Join("/sys/class/net", net, "carrier")) == "1"
}

// wlanEvents collects the output of iw event.
type wlanEvents struct {
	cmd  *exec.Cmd
	mu   sync.Mutex
	text string
}

func startEvents() (*wlanEvents, error) {
	e := &wlanEvents{cmd: exec.Command("iw", "event")}
	out, err := e.cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := e.cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			e.mu.Lock()
			e.text += sc.Text() + "\n"
			e.mu.Unlock()
		}
	}()
	return e, nil
}

// wait waits for an event that matches the pattern, and consumes the events
// up to it.
func (e *wlanEvents) wait(pattern string, timeout time.Duration) string {
	re := regexp.MustCompile(pattern)
	var found string
	harness.Wait(func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		if loc := re.FindStringIndex(e.text); loc != nil {
			found = e.text[loc[0]:loc[1]]
			e.text = e.text[loc[1]:]
			return true
		}
		return false
	}, timeout, 100*time.Millisecond)
	return found
}

// count returns how many events so far match, without consuming them.
func (e *wlanEvents) count(pattern string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(regexp.MustCompile(pattern).FindAllStringIndex(e.text, -1))
}

func (e *wlanEvents) close() {
	e.cmd.Process.Kill()
	e.cmd.Wait()
}

func (s *wlanSuite) events() *wlanEvents {
	e, err := startEvents()
	if !s.Check(err == nil, "iw event starts%s", errSuffix(err)) {
		return nil
	}
	s.track(e.cmd)
	return e
}

// track records a host process, which a second signal kills.
func (s *wlanSuite) track(cmd *exec.Cmd) {
	s.mu.Lock()
	s.procs = append(s.procs, cmd)
	s.mu.Unlock()
}

// connect connects to a network with iw and returns the event that ends
// the attempt.
func (s *wlanSuite) connect(e *wlanEvents, net, ssid string) string {
	if r := harness.Run("iw", "dev", net, "connect", ssid); !r.OK() {
		return "iw connect failed: " + strings.TrimSpace(r.Output())
	}
	ev := e.wait(regexp.QuoteMeta(net)+` \(phy #\d+\): (connected to|failed to connect|timed out)[^\n]*`, 40*time.Second)
	if ev == "" {
		return "no connection event"
	}
	return ev
}

func disconnected(net string) string {
	return regexp.QuoteMeta(net) + ` \(phy #\d+\): disconnected`
}

// lease gets a lease from the guest's DHCP server.
func (s *wlanSuite) lease(net string) bool {
	r := harness.RunContext(s.ctx, s.dir, time.Minute, "busybox", "udhcpc", "-i", net, "-f", "-n", "-q",
		"-t", "5", "-T", "2", "-s", filepath.Join(s.out, "udhcpc.sh"))
	addr := strings.Fields(harness.Run("ip", "-4", "-br", "addr", "show", net).Stdout)
	if !s.Check(r.OK() && len(addr) > 2, "%s gets a DHCP lease%s", net, listSuffix("of", addr[min(2, len(addr)):])) {
		fmt.Print(r.Output())
		return false
	}
	return true
}

// traffic gets a lease, then pings the access points' bridge and runs
// iperf3 against it both ways.
func (s *wlanSuite) traffic(g *harness.Guest, net string) {
	s.Check(wlanCarrier(net), "%s has carrier while connected", net)
	if !s.lease(net) {
		return
	}
	defer harness.Run("ip", "-4", "addr", "flush", "dev", net)
	r := harness.RunContext(s.ctx, s.dir, time.Minute, "ping", "-c", "5", "-i", "0.2", "-W", "2", "-I", net, wlanServer)
	p, _ := harness.ParsePing(r.Output())
	s.Check(r.OK() && p.Received == 5, "ping %s gets 5 replies: %d", wlanServer, p.Received)
	for _, reverse := range []bool{false, true} {
		argv := []string{"iperf3", "-c", wlanServer, "--bind-dev", net, "-t", "3", "-J"}
		what := "to"
		if reverse {
			argv = append(argv, "-R")
			what = "from"
		}
		var res harness.Iperf
		var err error
		// The server stops listening for a moment between runs, or drops a
		// control connection that arrives then.
		for i := 0; i < 5; i++ {
			r = harness.RunContext(s.ctx, s.dir, 30*time.Second, argv[0], argv[1:]...)
			res, err = harness.ParseIperf(r.Stdout)
			if err == nil || !strings.Contains(err.Error(), "refused") && !strings.Contains(err.Error(), "reset") {
				break
			}
			time.Sleep(time.Second)
		}
		if !s.Check(err == nil && res.ReceivedBPS > 0, "iperf3 %s the guest: %.0f Mb/s%s", what, res.ReceivedBPS/1e6, errSuffix(err)) {
			fmt.Println(g.Output("pidof iperf3; cat /tmp/iperf3.log", 10*time.Second))
		}
	}
}

// wlanBand is a band of iw phy info.
type wlanBand struct {
	freqs []int
	ht    string
	vht   string
	he    bool
}

var (
	phyBandRE   = regexp.MustCompile(`^\s*Band (\d+):`)
	phyFreqRE   = regexp.MustCompile(`\* (\d+)(?:\.\d+)? MHz \[\d+\]`)
	phyHTRE     = regexp.MustCompile(`^\s*Capabilities: (0x[0-9a-f]+)`)
	phyVHTRE    = regexp.MustCompile(`VHT Capabilities \((0x[0-9a-f]+)\)`)
	phyHERE     = regexp.MustCompile(`(^|[^E])HE Iftypes:.*\bmanaged\b`)
	phyCipherRE = regexp.MustCompile(`Supported Ciphers:\n((?:\s+\* .*\n)+)`)
)

// parsePhy returns the channels and capabilities of each band in iw phy
// info, and the ciphers.
func parsePhy(text string) (map[int]*wlanBand, []string) {
	bands := map[int]*wlanBand{}
	var band *wlanBand
	for _, line := range strings.Split(text, "\n") {
		if m := phyBandRE.FindStringSubmatch(line); m != nil {
			n, _ := strconv.Atoi(m[1])
			band = &wlanBand{}
			bands[n] = band
			continue
		}
		if strings.HasPrefix(line, "\t") && !strings.HasPrefix(line, "\t\t") {
			band = nil
		}
		if band == nil {
			continue
		}
		if m := phyFreqRE.FindStringSubmatch(line); m != nil {
			f, _ := strconv.Atoi(m[1])
			band.freqs = append(band.freqs, f)
		}
		if m := phyHTRE.FindStringSubmatch(line); m != nil && band.ht == "" {
			band.ht = m[1]
		}
		if m := phyVHTRE.FindStringSubmatch(line); m != nil {
			band.vht = m[1]
		}
		if phyHERE.MatchString(line) {
			band.he = true
		}
	}
	var ciphers []string
	if m := phyCipherRE.FindStringSubmatch(text); m != nil {
		for _, l := range strings.Split(strings.TrimSpace(m[1]), "\n") {
			ciphers = append(ciphers, strings.Trim(l, "\t *"))
		}
	}
	sort.Strings(ciphers)
	return bands, ciphers
}

var (
	scanBSSRE  = regexp.MustCompile(`^BSS ([0-9a-f:]{17})`)
	scanFreqRE = regexp.MustCompile(`^\s+freq: (\d+)`)
	scanSSIDRE = regexp.MustCompile(`^\s+SSID: (.*)`)
)

// parseScan returns the frequency of each SSID that iw scan found.
func parseScan(text string) map[string]int {
	found := map[string]int{}
	freq := 0
	for _, line := range strings.Split(text, "\n") {
		if scanBSSRE.MatchString(line) {
			freq = 0
		} else if m := scanFreqRE.FindStringSubmatch(line); m != nil {
			freq, _ = strconv.Atoi(m[1])
		} else if m := scanSSIDRE.FindStringSubmatch(line); m != nil {
			found[m[1]] = freq
		}
	}
	return found
}

func bandOf(freq int) string {
	switch {
	case freq < 3000:
		return "2g"
	case freq < 5935:
		return "5g"
	}
	return "6g"
}

func wlanTestWiphy(s *wlanSuite) {
	g := s.start()
	if g == nil {
		return
	}
	phy, net := s.attach()
	if net == "" {
		return
	}
	host, hostCiphers := parsePhy(harness.Run("iw", "phy", phy, "info").Stdout)
	guest, guestCiphers := parsePhy(g.Output("iw phy $(cat /sys/class/net/wlan0/phy80211/name) info", 10*time.Second))
	var nums []int
	for n := range host {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	s.Check(fmt.Sprint(nums) == "[1 2 4]", "the wiphy has the 2.4, 5 and 6 GHz bands: %v", nums)
	for _, n := range []int{1, 2, 4} {
		gb, hb := guest[n], host[n]
		if !s.Check(gb != nil && hb != nil, "band %d is on both wiphys", n) {
			continue
		}
		s.Check(fmt.Sprint(hb.freqs) == fmt.Sprint(gb.freqs), "band %d has the guest's %d channels", n, len(gb.freqs))
		s.Check(hb.ht == gb.ht && hb.vht == gb.vht && hb.he == gb.he,
			"band %d has the guest's HT %s, VHT %s and HE %v", n, gb.ht, gb.vht, gb.he)
	}
	s.Check(len(hostCiphers) > 0 && fmt.Sprint(hostCiphers) == fmt.Sprint(guestCiphers),
		"the wiphy has the guest's ciphers: %v", hostCiphers)

	isUp := func() bool {
		out := g.Output("cat /sys/class/net/wlan0/flags", 10*time.Second)
		m := regexp.MustCompile(`0x[0-9a-f]+`).FindString(out)
		v, err := strconv.ParseUint(strings.TrimPrefix(m, "0x"), 16, 32)
		return err == nil && v&1 != 0
	}
	r := harness.Run("ip", "link", "set", net, "up")
	s.Check(r.OK() && isUp(), "bringing %s up brings the guest's wlan0 up %s", net, r.Stderr)
	r = harness.Run("ip", "link", "set", net, "down")
	s.Check(r.OK() && !isUp(), "bringing %s down brings the guest's wlan0 down %s", net, r.Stderr)

	orig := harness.Read(filepath.Join("/sys/class/net", net, "address"))
	const addr = "02:11:22:33:44:55"
	r = harness.Run("ip", "link", "set", net, "address", addr)
	s.Check(r.OK() && guestSays(g, "cat /sys/class/net/wlan0/address", addr),
		"setting %s's address sets the guest's %s", net, r.Stderr)
	harness.Run("ip", "link", "set", net, "address", orig)

	harness.Run("ip", "link", "set", net, "up")
	// Setting the current state sends nothing, so power save is turned off
	// first.
	ok := true
	for _, state := range []string{"off", "on"} {
		r = harness.Run("iw", "dev", net, "set", "power_save", state)
		ok = ok && r.OK() && guestSays(g, "iw dev wlan0 get power_save", "Power save: "+state)
	}
	s.Check(ok, "power save on the host turns it off and on in the guest")
	harness.Run("ip", "link", "set", net, "down")

	ok = true
	for _, country := range []string{"GB", wlanCountry} {
		harness.Run("iw", "reg", "set", country)
		ok = ok && guestSays(g, "iw reg get", "country "+country+":")
	}
	s.Check(ok, "the host's country reaches the guest")
	s.healthy(g, phy)
	s.Check(wlanCounter(phy, "stale_events") == 0, "the guest reports no stale event")
}

// scan returns the frequency of each SSID that a scan finds. A scan of the
// system wpa_supplicant, which can start before the interface is released,
// makes the first attempts fail with EBUSY or end without results, so a scan
// that finds nothing is tried again.
func (s *wlanSuite) scan(net string) map[string]int {
	empty := 0
	var r harness.Result
	for i := 0; i < 20 && s.ctx.Err() == nil; i++ {
		r = harness.RunTimeout(40*time.Second, "iw", "dev", net, "scan")
		if found := parseScan(r.Stdout); len(found) > 0 {
			return found
		}
		if !strings.Contains(r.Stderr, "(-16)") {
			if empty++; empty == 3 {
				break
			}
		}
		time.Sleep(time.Second)
	}
	fmt.Printf("iw scan returned %d: %s\n", r.Code, strings.TrimSpace(r.Stderr))
	return nil
}

func wlanTestConnect(s *wlanSuite) {
	g, phy, net := s.up()
	if g == nil {
		return
	}
	found := s.scan(net)
	var missing, wrong []string
	for _, ssid := range wlanNetworks {
		freq, ok := found[ssid]
		if !ok {
			missing = append(missing, ssid)
		} else if bandOf(freq) != strings.Split(ssid, "-")[1] {
			wrong = append(wrong, fmt.Sprintf("%s %d MHz", ssid, freq))
		}
	}
	s.Check(len(missing) == 0, "a scan finds the guest's networks%s", listSuffix("but", missing))
	s.Check(len(wrong) == 0, "each network is on its band%s", listSuffix("but", wrong))

	e := s.events()
	if e == nil {
		return
	}
	defer e.close()
	disconnects := int64(0)
	for _, ssid := range []string{"hwsim-2g-open", "hwsim-5g-open"} {
		ev := s.connect(e, net, ssid)
		link := harness.Run("iw", "dev", net, "link").Stdout
		if !s.Check(strings.Contains(ev, "connected to") && strings.Contains(link, "SSID: "+ssid),
			"connecting to %s: %s", ssid, ev) {
			continue
		}
		sta := harness.Run("iw", "dev", net, "station", "dump").Stdout
		s.Check(strings.Contains(sta, "signal:"), "station dump shows the access point's signal")
		s.traffic(g, net)
		harness.Run("iw", "dev", net, "disconnect")
		disconnects++
		ev = e.wait(disconnected(net), 10*time.Second)
		s.Check(ev != "" && !wlanCarrier(net) && strings.Contains(harness.Run("iw", "dev", net, "link").Stdout, "Not connected"),
			"disconnecting from %s drops the carrier", ssid)
	}
	ev := s.connect(e, net, "hwsim-missing")
	s.Check(strings.Contains(ev, "failed to connect") || strings.Contains(ev, "timed out"),
		"connecting to a missing network fails: %s", ev)
	ev = s.connect(e, net, "hwsim-2g-open")
	s.Check(strings.Contains(ev, "connected to"), "connecting to hwsim-2g-open again: %s", ev)
	s.healthy(g, phy)
	// The guest reports each disconnection that the host asked for with the
	// id of a connection that has already ended, which the driver ignores.
	stale := wlanSettledCounter(phy, "stale_events")
	s.Check(stale == disconnects, "the guest reports the %d disconnections as stale events: %d", disconnects, stale)
}

// wpaState returns wpa_supplicant's state.
func wpaState(net, ctrl string) string {
	out := harness.Run("wpa_cli", "-p", ctrl, "-i", net, "status").Stdout
	m := regexp.MustCompile(`(?m)^wpa_state=(\w+)`).FindStringSubmatch(out)
	if m == nil {
		return ""
	}
	return m[1]
}

// wpa joins a network with wpa_supplicant and passes traffic. On a network
// that renews its keys, it waits for both to be renewed first. With reset,
// it restarts the guest's radio as a firmware recovery does, and checks
// that wpa_supplicant joins again.
func (s *wlanSuite) wpa(g *harness.Guest, net, ssid string, mfp int, rekeys, reset bool) {
	ctrl := filepath.Join(s.dir, "wpa")
	conf := filepath.Join(s.dir, "wpa_supplicant.conf")
	logPath := filepath.Join(s.dir, "wpa-"+ssid+".log")
	// A network with required MFP also offers the SHA-256 PSK.
	keyMgmt := "WPA-PSK"
	if mfp == 2 {
		keyMgmt = "WPA-PSK WPA-PSK-SHA256"
	}
	text := fmt.Sprintf("ctrl_interface=%s\nnetwork={\n\tssid=\"%s\"\n\tpsk=\"%s\"\n\tkey_mgmt=%s\n\tieee80211w=%d\n}\n",
		ctrl, ssid, wlanPassphrase, keyMgmt, mfp)
	if !s.Check(os.WriteFile(conf, []byte(text), 0600) == nil, "the wpa_supplicant configuration is written") {
		return
	}
	logf, err := os.Create(logPath)
	if !s.Check(err == nil, "the wpa_supplicant log is made%s", errSuffix(err)) {
		return
	}
	cmd := exec.Command("wpa_supplicant", "-i", net, "-D", "nl80211", "-c", conf)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	err = cmd.Start()
	logf.Close()
	if !s.Check(err == nil, "wpa_supplicant starts%s", errSuffix(err)) {
		return
	}
	s.track(cmd)
	readLog := func() string { return harness.Read(logPath) }
	completed := func() bool { return wpaState(net, ctrl) == "COMPLETED" }
	defer func() {
		cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			cmd.Process.Kill()
			<-done
		}
		harness.Wait(func() bool {
			return strings.Contains(harness.Run("iw", "dev", net, "link").Stdout, "Not connected")
		}, 10*time.Second, 200*time.Millisecond)
		s.Check(!wlanCarrier(net) && strings.Contains(harness.Run("iw", "dev", net, "link").Stdout, "Not connected"),
			"stopping wpa_supplicant disconnects from %s", ssid)
	}()

	if !s.Check(harness.Wait(completed, 30*time.Second, 200*time.Millisecond), "wpa_supplicant joins %s", ssid) {
		log := readLog()
		fmt.Println(log[max(0, len(log)-3000):])
		return
	}
	if reset {
		said := g.Output("mount -t debugfs none /sys/kernel/debug; echo 1 > "+
			"/sys/kernel/debug/ieee80211/$(cat /sys/class/net/wlan0/phy80211/name)/reset && echo reset-''done",
			10*time.Second)
		// The restart ends the connection, and wpa_supplicant joins again.
		again := harness.Wait(func() bool {
			return strings.Count(readLog(), "CTRL-EVENT-CONNECTED") >= 2 && completed()
		}, 30*time.Second, 200*time.Millisecond)
		s.Check(strings.Contains(said, "reset-done") && again,
			"wpa_supplicant joins %s again after the guest's radio restarts", ssid)
	}
	negotiated := 0
	if rekeys {
		time.Sleep(8 * time.Second)
		log := readLog()
		// A rekey changes the state for a moment.
		harness.Wait(completed, 5*time.Second, 200*time.Millisecond)
		negotiated = strings.Count(log, "WPA: Key negotiation completed")
		s.Check(negotiated >= 2 && strings.Contains(log, "WPA: Group rekeying completed") && completed(),
			"%s renews its pairwise and group keys", ssid)
	}
	s.traffic(g, net)
	if rekeys {
		s.Check(strings.Count(readLog(), "WPA: Key negotiation completed") > negotiated,
			"%s renews its pairwise key during the traffic", ssid)
		// A message 4 that the access point does not accept still logs a
		// completed negotiation, and ends in a deauthentication once the
		// access point has given up resending message 3.
		time.Sleep(5 * time.Second)
	}
	if !reset {
		log := readLog()
		s.Check(strings.Count(log, "CTRL-EVENT-CONNECTED") == 1 && !strings.Contains(log, "CTRL-EVENT-DISCONNECTED"),
			"%s stays connected throughout", ssid)
	}
}

func wlanTestWPA(s *wlanSuite) {
	g, phy, net := s.up()
	if g == nil {
		return
	}
	e := s.events()
	if e == nil {
		return
	}
	defer e.close()
	before := wlanSettledCounter(phy, "stale_events")
	for _, w := range wlanWPA {
		if s.ctx.Err() != nil {
			return
		}
		s.wpa(g, net, w.ssid, w.mfp, w.rekeys, false)
	}
	s.Check(wlanCounter(phy, "control_port_frames") > 0 && wlanCounter(phy, "control_port_rx_frames") > 0,
		"wpa_supplicant sends and receives EAPOL frames through the control port")
	// wpa_supplicant decides how often it disconnects, so the disconnections
	// that it asked for are counted from its events. It can also end a
	// connection attempt, which the host reports with no disconnection
	// event, and whose later events from the guest are stale as well.
	after := wlanSettledCounter(phy, "stale_events")
	local := int64(e.count(disconnected(net) + ` \(local request\)`))
	s.Check(after-before >= local, "the guest reports the %d disconnections that wpa_supplicant asked for", local)
	s.healthy(g, phy)
	c := g.Counters()
	// The hwsim guest sends a frame before dev_queue_xmit() returns, so only
	// this counter shows that the backend completes control port frames from
	// their destructor.
	s.Check(c["tx_chained"] > 0, "the backend completes control port frames once they are sent")
}

func wlanTestReset(s *wlanSuite) {
	g, phy, net := s.up()
	if g == nil {
		return
	}
	s.wpa(g, net, "hwsim-5g-wpa2-mfp", 2, false, true)
	s.healthy(g, phy)
}

func wlanTestRemove(s *wlanSuite) {
	g, phy, net := s.up()
	if g == nil {
		return
	}
	e := s.events()
	if e == nil {
		return
	}
	ev := s.connect(e, net, "hwsim-2g-open")
	e.close()
	if !s.Check(strings.Contains(ev, "connected to"), "connecting to hwsim-2g-open: %s", ev) {
		return
	}
	s.healthy(g, phy)
	if err := harness.Detach(wlanName, false); !s.Check(err == nil, "vdpa dev del removes the device%s", errSuffix(err)) {
		return
	}
	s.Check(!harness.Exists(filepath.Join("/sys/class/ieee80211", phy)) && !harness.Exists(filepath.Join("/sys/class/net", net)),
		"removing a connected device removes the wiphy and the interface")
	if phy, net = s.attach(); net == "" {
		return
	}
	harness.Run("ip", "link", "set", net, "up")
	if e = s.events(); e == nil {
		return
	}
	defer e.close()
	ev = s.connect(e, net, "hwsim-2g-open")
	if s.Check(strings.Contains(ev, "connected to"), "connecting to hwsim-2g-open after attaching again: %s", ev) {
		s.traffic(g, net)
	}
	s.healthy(g, phy)
}

func wlanTestKill(s *wlanSuite) {
	g, phy, net := s.up()
	if g == nil {
		return
	}
	e := s.events()
	if e == nil {
		return
	}
	defer e.close()
	defer harness.Run("ip", "-4", "addr", "flush", "dev", net)
	ev := s.connect(e, net, "hwsim-2g-open")
	if !s.Check(strings.Contains(ev, "connected to") && s.lease(net), "connecting to hwsim-2g-open: %s", ev) {
		return
	}
	s.healthy(g, phy)
	ctx, cancel := context.WithCancel(s.ctx)
	done := make(chan struct{})
	go func() {
		harness.RunContext(ctx, s.dir, 2*time.Minute, "iperf3", "-c", wlanServer, "--bind-dev", net, "-t", "60")
		close(done)
	}()
	time.Sleep(3 * time.Second)
	uml := harness.UMLPid(g.Pid())
	if !s.Check(uml > 0, "the guest's UML process is found") {
		cancel()
		<-done
		return
	}
	// The host finds the dead device once the frames it sent stop being
	// reclaimed and a probe of the connection times out.
	syscall.Kill(uml, syscall.SIGKILL)
	start := time.Now()
	ev = e.wait(disconnected(net), 30*time.Second)
	took := time.Since(start)
	cancel()
	<-done
	s.Check(ev != "" && !wlanCarrier(net) && wlanCounter(phy, "cmd_timeouts") == 1,
		"killing the guest during traffic ends the connection after %.1f s", took.Seconds())
	g.Wait(30 * time.Second)
	harness.Run("ip", "link", "set", net, "down")
	start = time.Now()
	r := harness.Run("ip", "link", "set", net, "up")
	took = time.Since(start)
	s.Check(!r.OK() && took < time.Second && wlanCounter(phy, "cmd_timeouts") == 1,
		"the broken device fails commands at once")
	harness.Detach(wlanName, true)
	s.Check(!harness.Exists(filepath.Join("/sys/class/ieee80211", phy)) && !harness.Exists(filepath.Join("/sys/class/net", net)),
		"the broken device can be removed")
}

func wlanTestGone(s *wlanSuite) {
	g, phy, net := s.up()
	if g == nil {
		return
	}
	e := s.events()
	if e == nil {
		return
	}
	defer e.close()
	ev := s.connect(e, net, "hwsim-2g-open")
	if !s.Check(strings.Contains(ev, "connected to"), "connecting to hwsim-2g-open: %s", ev) {
		return
	}
	g.Output("iw dev wlan0 del", 10*time.Second)
	ev = e.wait(disconnected(net), 5*time.Second)
	s.Check(ev != "" && !wlanCarrier(net) && wlanCounter(phy, "cmd_timeouts") == 0,
		"removing the guest's interface ends the host's connection at once")
	harness.Run("ip", "link", "set", net, "down")
	start := time.Now()
	r := harness.Run("ip", "link", "set", net, "up")
	took := time.Since(start)
	s.Check(!r.OK() && took < 2*time.Second && wlanCounter(phy, "cmd_timeouts") == 0,
		"the device then fails commands without a timeout")
	start = time.Now()
	harness.Detach(wlanName, false)
	took = time.Since(start)
	s.Check(!harness.Exists(filepath.Join("/sys/class/net", net)) && took < 5*time.Second,
		"the device is removed in %.1f s", took.Seconds())
}
