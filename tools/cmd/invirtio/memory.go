package main

import (
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"invirtio/internal/sysfs"
)

// guestCgroup returns the cgroup that the supervisor gave the guest of a
// running service, or "".
func guestCgroup(unit string) string {
	out, err := exec.Command("systemctl", "show", "-p", "ControlGroup", "--value", unit).Output()
	rel := strings.TrimSpace(string(out))
	if err != nil || rel == "" {
		return ""
	}
	dir := filepath.Join("/sys/fs/cgroup", rel, "guest")
	if _, err := os.Stat(filepath.Join(dir, "memory.current")); err != nil {
		return ""
	}
	return dir
}

// formatSize formats a size in bytes with the largest unit of K, M and G
// in which it rounds to one or more.
func formatSize(n int64) string {
	switch {
	case math.Round(float64(n)/(1<<20)) >= 1024:
		return fmt.Sprintf("%.2fG", float64(n)/(1<<30))
	case math.Round(float64(n)/(1<<10)) >= 1024:
		return fmt.Sprintf("%.0fM", float64(n)/(1<<20))
	case n >= 1024:
		return fmt.Sprintf("%.0fK", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%dB", n)
}

// readBytes reads a cgroup file that holds a size or max, and returns the
// size, or -1 for max.
func readBytes(path string) (int64, error) {
	s, err := sysfs.Read(path)
	if err != nil {
		return 0, err
	}
	if s == "max" {
		return -1, nil
	}
	return strconv.ParseInt(s, 10, 64)
}

// parseKeyed parses lines of a name and a number, as memory.stat and
// /proc/meminfo have them, with the numbers of /proc/meminfo in kB.
func parseKeyed(text string) map[string]int64 {
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

// memoryUse returns the memory row of a running guest: what its cgroup
// holds, its peak and its limit.
func memoryUse(cg string) string {
	cur, err := readBytes(filepath.Join(cg, "memory.current"))
	if err != nil {
		return "not available"
	}
	s := formatSize(cur)
	if peak, err := readBytes(filepath.Join(cg, "memory.peak")); err == nil {
		s += ", peak " + formatSize(peak)
	}
	if limit, err := readBytes(filepath.Join(cg, "memory.max")); err == nil && limit >= 0 {
		s += ", limit " + formatSize(limit)
	} else if err == nil {
		s += ", no limit"
	}
	return s
}

// memoryDetail returns the guest cgroup's anonymous memory, its file pages
// with the shared memory that holds the guest's RAM among them, and the
// kernel's memory for it.
func memoryDetail(cg string) string {
	data, err := os.ReadFile(filepath.Join(cg, "memory.stat"))
	if err != nil {
		return "not available"
	}
	st := parseKeyed(string(data))
	return fmt.Sprintf("anon %s, file %s (shmem %s), kernel %s", formatSize(st["anon"]),
		formatSize(st["file"]), formatSize(st["shmem"]), formatSize(st["kernel"]))
}

// pinnedMemory returns the memory that the guest's processes lock or pin
// for DMA, which is part of the cgroup's memory.
func pinnedMemory(cg string) (int64, error) {
	data, err := os.ReadFile(filepath.Join(cg, "cgroup.procs"))
	if err != nil {
		return 0, err
	}
	var total int64
	for _, pid := range strings.Fields(string(data)) {
		status, err := os.ReadFile(filepath.Join("/proc", pid, "status"))
		if err != nil {
			continue
		}
		st := parseKeyed(string(status))
		total += st["VmPin"] + st["VmLck"]
	}
	return total, nil
}

// guestMemory returns the guest's own view of its memory from its
// /proc/meminfo.
func guestMemory(meminfo string) string {
	m := parseKeyed(meminfo)
	total, avail := m["MemTotal"], m["MemAvailable"]
	if total == 0 {
		return "not available"
	}
	return fmt.Sprintf("%s used of %s, %s free", formatSize(total-avail), formatSize(total), formatSize(m["MemFree"]))
}
