package main

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"invirtio/internal/pci"
)

func TestParseSize(t *testing.T) {
	good := map[string]int64{
		"768M": 768 << 20,
		"1G":   1 << 30,
		"64k":  64 << 10,
		"4096": 4096,
	}
	for s, want := range good {
		if got, err := parseSize(s); err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v, want %d", s, got, err, want)
		}
	}
	for _, s := range []string{"", "M", "-1G", "1.5G", "1GB", "0", "12x", "99999999999G"} {
		if _, err := parseSize(s); err == nil {
			t.Errorf("parseSize(%q) succeeded", s)
		}
	}
}

func TestNames(t *testing.T) {
	if got := defaultName("wlan", "0000:06:00.0"); got != "wlan-06-00-0" {
		t.Errorf("defaultName = %q", got)
	}
	if got := defaultName("net", "0001:05:00.1"); got != "net-0001-05-00-1" {
		t.Errorf("defaultName = %q", got)
	}
	for _, name := range []string{"wlan-06-00-0", "net0", "a"} {
		if err := checkName(name); err != nil {
			t.Errorf("checkName(%q): %v", name, err)
		}
	}
	for _, name := range []string{"", "control", "invirtio", "-x", "Wlan", "a/b", "a.b", "a,b",
		strings.Repeat("a", 33)} {
		if checkName(name) == nil {
			t.Errorf("checkName(%q) succeeded", name)
		}
	}
}

func TestMatchDriver(t *testing.T) {
	modinfo := []byte("mt7921e.firmware=mediatek/WIFI_RAM_CODE_MT7922_1.bin\x00" +
		"mt7921e.alias=pci:v000014C3d00000616sv*sd*bc*sc*i*\x00" +
		"i40e.alias=pci:v00008086d00001572sv*sd*bc*sc*i*\x00")
	cases := map[string]string{
		"pci:v000014C3d00000616sv000014C3sd00000616bc02sc80i00": "mt7921e",
		"pci:v00008086d00001572sv00008086sd00000000bc02sc00i00": "i40e",
		"pci:v000010ECd00008125sv000010ECsd00000123bc02sc00i00": "",
	}
	for alias, want := range cases {
		if got := pci.MatchDriver(alias, modinfo); got != want {
			t.Errorf("pci.MatchDriver(%q) = %q, want %q", alias, got, want)
		}
	}
}

func TestEscapeFilter(t *testing.T) {
	cases := map[string]string{
		"plain\r\n":                          "plain\r\n",
		"\x1b[1;31mred\x1b[0m":               "\x1b[1;31mred\x1b[0m",
		"a\x1b]0;title\x07b":                 "ab",
		"a\x1b]52;c;ZXZpbA==\x1b\\b":         "ab",
		"a\x1bPdcs\x1b\x1b\\b":               "ab",
		"a\x1b_apc\x1bxstill\x1b\\b":         "ab",
		"a\x1b^pm\x07b\x1bXsos\x1b\\c\x1b7":  "abc\x1b7",
		"a\x1b\x1b]52;c;ZXZpbA==\x07b":       "ab",
		"a\x9d0;title\x07b":                  "ab",
		"a\xc2\x9d0;title\x9cb":              "ab",
		"a\xc2\x9bc\x85d":                    "acd",
		"caf\xc3\xa9 \xe2\x82\xac \xc2\xa0x": "caf\xc3\xa9 \xe2\x82\xac \xc2\xa0x",
		"\xf0\x9f\x98\x80 ok":                "\xf0\x9f\x98\x80 ok",
		"a\x1b\x9b31mb":                      "a31mb",
		"a\x1b\x85b":                         "ab",
		"a\xe0\x82\x9d0;t\x07b":              "ab",
		"a\xf0\x80\x82\x9d0;t\x07b":          "ab",
		"a\x9dstill\x9cb":                    "ab",
		"a\x80b":                             "ab",
		"a\x1b\a]0;evil\ab":                  "a\ab",
		"a\x1b\x7f]0;evil\ab":                "ab",
		"a\x1b\x00P1$r\x1b\\b":               "a\x00b",
		"a\x1b\n_apc\x1b\\b":                 "a\nb",
		"a\x1b\x18]x":                        "a\x18]x",
		"a\x1bktitle\x1b\\b":                 "ab",
		"\x1b%@\xc3\x9d52;c;aGk=\x07":        "\xc3\x9d52;c;aGk=\x07",
		"a\x1b%Gb":                           "ab",
		"a\x1b Fb":                           "ab",
		"a\x1b#8b":                           "ab",
		"a\x1b%\a@b":                         "a\ab",
		"\x1b(Bx\x1b)0y":                     "\x1b(Bx\x1b)0y",
	}
	for in, want := range cases {
		var out bytes.Buffer
		f := &escapeFilter{w: &out}
		// One byte at a time, as a sequence can span reads.
		for i := 0; i < len(in); i++ {
			f.Write([]byte{in[i]})
		}
		if out.String() != want {
			t.Errorf("filter(%q) = %q, want %q", in, out.String(), want)
		}
	}
}

func TestLineLogger(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	l := newLineLogger("dev")
	os.Stdout = stdout
	l.Write([]byte("boot\r\n\x1b[1mbold\x1b[0m line\x07\n"))
	l.Write([]byte("a\xc2\x9d0;t\xc2\x9cb \x9b31m caf\xc3\xa9 \xe0\x82\x9d\n"))
	l.Write([]byte("split "))
	l.Write([]byte("line\n"))
	w.Close()
	var out bytes.Buffer
	out.ReadFrom(r)
	want := "dev: boot\ndev: bold line\ndev: a0;tb 31m caf\xc3\xa9 \ndev: split line\n"
	if out.String() != want {
		t.Errorf("logged %q, want %q", out.String(), want)
	}
}

func TestGuestArgs(t *testing.T) {
	dev := &Device{Address: "0000:06:00.0", Type: "wlan", Options: []string{"poll=50"},
		Args: []string{"loglevel=4"}}
	got := strings.Join(guestArgs("wlan-06-00-0", dev, "/sys/fs/cgroup/x/guest"), " ")
	want := "-u invirtio -b -d 0000:06:00.0 -i /usr/lib/invirtio/initramfs.cpio " +
		"-s /run/invirtio/wlan-06-00-0.mconsole -t 30 " +
		"-e wlan,wlan-06-00-0,log=/run/invirtio/wlan-06-00-0/inflight " +
		"-g /sys/fs/cgroup/x/guest /usr/lib/invirtio/linux mem=256M " +
		"vduse_guest.device=wlan,wlan-06-00-0,poll=50 loglevel=4"
	if got != want {
		t.Errorf("guestArgs =\n%s\nwant\n%s", got, want)
	}
}

func TestConsoleServer(t *testing.T) {
	master, slave, err := openPty()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()
	// The guest's pty is raw, as the launcher sets it.
	if err := withFd(slave, func(fd int) error {
		_, err := term.MakeRaw(fd)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	self, _ := os.FindProcess(os.Getpid())
	s := newConsoleServer(master, self)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				s.output(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	slave.Write([]byte("before\n"))
	time.Sleep(100 * time.Millisecond)

	sock := filepath.Join(t.TempDir(), "console")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go s.serve(ln)

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.Write([]byte{0, 40, 0, 120})

	read := func(want string) {
		t.Helper()
		var got []byte
		buf := make([]byte, 256)
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		for !bytes.Contains(got, []byte(want)) {
			n, err := conn.Read(buf)
			got = append(got, buf[:n]...)
			if err != nil {
				t.Fatalf("read %q, want %q: %v", got, want, err)
			}
		}
	}
	read("before")
	slave.Write([]byte("after\n"))
	read("after")

	conn.Write([]byte("input\n"))
	buf := make([]byte, 64)
	slave.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := slave.Read(buf)
	if err != nil || !strings.Contains(string(buf[:n]), "input") {
		t.Fatalf("the guest read %q: %v", buf[:n], err)
	}

	var ws *unix.Winsize
	err = withFd(slave, func(fd int) error {
		var err error
		ws, err = unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
		return err
	})
	if err != nil || ws.Row != 40 || ws.Col != 120 {
		t.Errorf("the guest's size is %+v, want 40x120: %v", ws, err)
	}
}

func TestDisableDryRun(t *testing.T) {
	wlan := &Device{Address: "0000:06:00.0", Type: "wlan"}
	cfg := &Config{Devices: map[string]*Device{
		"net-05-00-0":  {Address: "0000:05:00.0", Type: "net"},
		"wlan-06-00-0": wlan,
	}}
	stages, err := disablePlan(cfg, "wlan-06-00-0", wlan)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stages[0].actions[0].show, "0000:05:00.0") ||
		strings.Contains(stages[0].actions[0].show, "0000:06:00.0") {
		t.Errorf("the first stage writes the rules\n%s", stages[0].actions[0].show)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	err = runStages(stages, true, false)
	os.Stdout = stdout
	w.Close()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	out.ReadFrom(r)
	t.Log("\n" + out.String())
	for _, want := range []string{
		"$ systemctl disable --now invirtio@wlan-06-00-0.service",
		"$ echo 1 > /sys/class/vduse/wlan-06-00-0/msg_timeout",
		"$ vdpa dev del wlan-06-00-0",
		"$ echo > /sys/bus/pci/devices/0000:06:00.0/driver_override",
		"$ echo 0000:06:00.0 > /sys/bus/pci/devices/0000:06:00.0/driver/unbind",
		`KERNEL=="0000:05:00.0", ATTR{driver_override}="vfio-pci"`,
		"This was a dry run",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the dry run lacks %q", want)
		}
	}
}

func TestClassType(t *testing.T) {
	cases := map[string]string{"0x028000": "wlan", "0x020000": "net", "0x010802": "", "0x0280": "wlan"}
	for class, want := range cases {
		if got := pci.ClassType(class); got != want {
			t.Errorf("pci.ClassType(%q) = %q, want %q", class, got, want)
		}
	}
}

func TestParseLspci(t *testing.T) {
	addr, name, ok := parseLspci("0000:06:00.0 Network controller: MEDIATEK Corp. MT7922 802.11ax PCI Express Wireless Network Adapter")
	if !ok || addr != "0000:06:00.0" || name != "MEDIATEK Corp. MT7922 802.11ax PCI Express Wireless Network Adapter" {
		t.Errorf("parseLspci = %q, %q, %v", addr, name, ok)
	}
	if _, _, ok := parseLspci("06:00.0 Network controller: x"); ok {
		t.Error("parseLspci took an address without its domain")
	}
}

func TestPrintable(t *testing.T) {
	in := "ok\t1\n\x1b]52;c;x\x07\xc2\x9d\x9bcaf\xc3\xa9\x7f\n"
	want := "ok\t1\n]52;c;xcaf\xc3\xa9\n"
	if got := printable(in); got != want {
		t.Errorf("printable(%q) = %q, want %q", in, got, want)
	}
}

func TestLineLoggerSplits(t *testing.T) {
	var out bytes.Buffer
	l := &lineLogger{prefix: "dev: ", out: &out}
	l.Write([]byte(strings.Repeat("x", maxLogLine+5) + "\n"))
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 2 || lines[0] != "dev: "+strings.Repeat("x", maxLogLine) ||
		lines[1] != "dev: xxxxx" {
		t.Errorf("logged %d lines: %q", len(lines), out.String())
	}
}

func TestOneLine(t *testing.T) {
	if got := oneLine("vdpa: error\nsecond line\n"); got != "vdpa: error second line" {
		t.Errorf("oneLine = %q", got)
	}
}

func TestLineLoggerClose(t *testing.T) {
	var out bytes.Buffer
	l := &lineLogger{prefix: "dev: ", out: &out}
	l.Write([]byte("done\nlast words"))
	l.close()
	if out.String() != "dev: done\ndev: last words\n" {
		t.Errorf("logged %q", out.String())
	}
}

func TestMemory(t *testing.T) {
	for n, want := range map[int64]string{512: "512B", 1023: "1023B", 1024: "1K", 3 << 10: "3K", 412 << 20: "412M",
		1<<30 - 1<<10: "1.00G", 1280 << 20: "1.25G"} {
		if got := formatSize(n); got != want {
			t.Errorf("formatSize(%d) = %q, want %q", n, got, want)
		}
	}
	st := parseKeyed("anon 2097152\nfile 10485760\nshmem 8388608\n")
	if st["anon"] != 2<<20 || st["file"] != 10<<20 || st["shmem"] != 8<<20 {
		t.Errorf("parseKeyed(memory.stat) = %v", st)
	}
	got := guestMemory("MemTotal:       786432 kB\nMemFree:        524288 kB\nMemAvailable:   655360 kB\n")
	if want := "128M used of 768M, 512M free"; got != want {
		t.Errorf("guestMemory = %q, want %q", got, want)
	}
}
