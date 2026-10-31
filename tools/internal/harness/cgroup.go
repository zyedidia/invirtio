package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// GuestMemory is the memory that a guest took: what its cgroup was charged,
// as invirtio's services are, and what the guest itself used.
type GuestMemory struct {
	// Mem is the guest's mem= argument.
	Mem string `json:"mem"`
	// The cgroup's peak, and its charge and the parts of it at the end.
	PeakBytes    int64 `json:"peak_bytes"`
	CurrentBytes int64 `json:"current_bytes"`
	AnonBytes    int64 `json:"anon_bytes"`
	FileBytes    int64 `json:"file_bytes"`
	ShmemBytes   int64 `json:"shmem_bytes"`
	KernelBytes  int64 `json:"kernel_bytes"`
	// The guest's MemTotal, and the most of it that the samples found in
	// use, which is MemTotal less MemAvailable.
	GuestTotalBytes    int64 `json:"guest_total_bytes"`
	GuestPeakUsedBytes int64 `json:"guest_peak_used_bytes"`
}

// MakeCgroup makes a cgroup for a guest below the root of the cgroup v2
// hierarchy, in place of one that an earlier run left, and returns its
// directory.
func MakeCgroup(name string) (string, error) {
	dir := filepath.Join("/sys/fs/cgroup", name)
	RemoveCgroup(dir)
	if err := os.Mkdir(dir, 0755); err != nil {
		return "", err
	}
	if !Exists(filepath.Join(dir, "memory.peak")) {
		os.Remove(dir)
		return "", fmt.Errorf("the cgroup %s has no memory.peak, so the memory controller is off", dir)
	}
	return dir, nil
}

// RemoveCgroup removes a cgroup once its processes are gone, which can take
// a moment after a guest exits.
func RemoveCgroup(dir string) {
	if !Exists(dir) {
		return
	}
	Wait(func() bool { return os.Remove(dir) == nil || !Exists(dir) }, 10*time.Second, 100*time.Millisecond)
}

// ParseKeyed parses lines of a name and a number, as memory.stat and
// /proc/meminfo have them, with the numbers of /proc/meminfo in kB.
func ParseKeyed(text string) map[string]int64 {
	m := map[string]int64{}
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		n, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			continue
		}
		if len(f) == 3 && f[2] == "kB" {
			n <<= 10
		}
		m[strings.TrimSuffix(f[0], ":")] = n
	}
	return m
}

// SampleGuest records the guest's own use of its memory, from its
// /proc/meminfo, if it is the most so far.
func (m *GuestMemory) SampleGuest(meminfo string) {
	info := ParseKeyed(meminfo)
	total, avail := info["MemTotal"], info["MemAvailable"]
	if total == 0 {
		return
	}
	m.GuestTotalBytes = total
	m.GuestPeakUsedBytes = max(m.GuestPeakUsedBytes, total-avail)
}

// ReadCgroup records the cgroup's peak, and its charge and the parts of it
// now.
func (m *GuestMemory) ReadCgroup(dir string) error {
	read := func(file string) (int64, error) {
		return strconv.ParseInt(Read(filepath.Join(dir, file)), 10, 64)
	}
	var err error
	if m.PeakBytes, err = read("memory.peak"); err != nil {
		return err
	}
	if m.CurrentBytes, err = read("memory.current"); err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(dir, "memory.stat"))
	if err != nil {
		return err
	}
	st := ParseKeyed(string(data))
	m.AnonBytes, m.FileBytes, m.ShmemBytes, m.KernelBytes = st["anon"], st["file"], st["shmem"], st["kernel"]
	return nil
}

// Summary describes the memory in one line, in MiB.
func (m *GuestMemory) Summary() string {
	mib := func(n int64) float64 { return float64(n) / (1 << 20) }
	return fmt.Sprintf("mem=%s: cgroup peak %.0fM, at the end %.0fM (anon %.0fM, file %.0fM, shmem %.0fM, kernel %.0fM), "+
		"guest peak use %.0fM of %.0fM", m.Mem, mib(m.PeakBytes), mib(m.CurrentBytes), mib(m.AnonBytes),
		mib(m.FileBytes), mib(m.ShmemBytes), mib(m.KernelBytes), mib(m.GuestPeakUsedBytes), mib(m.GuestTotalBytes))
}
