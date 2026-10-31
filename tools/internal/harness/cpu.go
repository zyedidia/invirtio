package harness

import (
	"fmt"
	"math/bits"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// HostCPU returns the host's busy and total time, in ticks, from the first
// line of /proc/stat. Busy time is everything but idle and I/O wait.
func HostCPU() (busy, total int64) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0
	}
	line, _, _ := strings.Cut(string(data), "\n")
	return ParseCPULine(line)
}

// ParseCPULine returns the busy and total ticks of a cpu line of /proc/stat.
func ParseCPULine(line string) (busy, total int64) {
	f := strings.Fields(line)
	if len(f) < 8 || !strings.HasPrefix(f[0], "cpu") {
		return 0, 0
	}
	// The guest and guest_nice fields after the eighth are already counted
	// in user and nice.
	var v []int64
	for _, x := range f[1:min(len(f), 9)] {
		n, err := strconv.ParseInt(x, 10, 64)
		if err != nil {
			return 0, 0
		}
		v = append(v, n)
	}
	for _, n := range v {
		total += n
	}
	return total - v[3] - v[4], total
}

// Parent returns a process's parent, from /proc/<pid>/stat.
func Parent(pid int) int {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	stat := string(data)
	i := strings.LastIndexByte(stat, ')')
	if i < 0 {
		return 0
	}
	f := strings.Fields(stat[i+1:])
	if len(f) < 2 {
		return 0
	}
	ppid, _ := strconv.Atoi(f[1])
	return ppid
}

// PlaceIRQs sets the CPUs that VDUSE injects each queue's interrupts
// from. VDUSE loops in the host kernel when it injects on a queue whose
// affinity has no online CPU, which virtio_vdpa's spread over the present
// CPUs can leave, so such a queue first gets every online CPU.
func PlaceIRQs(name string, cpu int) error {
	online, err := ParseCPUList(Read("/sys/devices/system/cpu/online"))
	if err != nil {
		return err
	}
	files, _ := filepath.Glob(filepath.Join("/sys/class/vduse", name, "vq*/irq_cb_affinity"))
	for _, f := range files {
		mask, err := ParseMask(Read(f))
		if err != nil {
			return err
		}
		if cpu >= 0 {
			mask = SetBit(nil, cpu)
		} else if !Intersects(mask, online) {
			mask = online
		}
		if err := Write(f, FormatMask(mask)); err != nil {
			return fmt.Errorf("%s: %v", f, err)
		}
	}
	return nil
}

// ProcessTicks returns a process's user and system ticks, over all its
// threads.
func ProcessTicks(pid int) [2]int64 {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return [2]int64{}
	}
	return ParseStat(string(data))
}

// ParseStat returns the utime and stime fields of /proc/<pid>/stat.
// The command name in parentheses can hold spaces, so the fields are counted
// from after it.
func ParseStat(stat string) [2]int64 {
	i := strings.LastIndexByte(stat, ')')
	if i < 0 {
		return [2]int64{}
	}
	f := strings.Fields(stat[i+1:])
	if len(f) < 13 {
		return [2]int64{}
	}
	u, _ := strconv.ParseInt(f[11], 10, 64)
	st, _ := strconv.ParseInt(f[12], 10, 64)
	return [2]int64{u, st}
}

// ParseCPUList parses a CPU list such as 0-3,8 into a mask.
func ParseCPUList(list string) ([]uint64, error) {
	var mask []uint64
	for _, r := range strings.Split(list, ",") {
		lo, hi, found := strings.Cut(r, "-")
		a, err := strconv.Atoi(lo)
		if err != nil {
			return nil, fmt.Errorf("cannot parse the CPU list %q", list)
		}
		z := a
		if found {
			if z, err = strconv.Atoi(hi); err != nil {
				return nil, fmt.Errorf("cannot parse the CPU list %q", list)
			}
		}
		for c := a; c <= z; c++ {
			mask = SetBit(mask, c)
		}
	}
	return mask, nil
}

// ParseMask parses a CPU mask as sysfs prints it, in comma-separated
// groups of 32 bits, most significant first.
func ParseMask(s string) ([]uint64, error) {
	groups := strings.Split(strings.TrimSpace(s), ",")
	var mask []uint64
	for i := range groups {
		g := groups[len(groups)-1-i]
		v, err := strconv.ParseUint(g, 16, 32)
		if err != nil {
			return nil, fmt.Errorf("cannot parse the CPU mask %q", s)
		}
		for bit := 0; bit < 32; bit++ {
			if v&(1<<bit) != 0 {
				mask = SetBit(mask, 32*i+bit)
			}
		}
	}
	return mask, nil
}

// FormatMask formats a CPU mask as sysfs takes it.
func FormatMask(mask []uint64) string {
	var groups []string
	for i := len(mask) - 1; i >= 0; i-- {
		groups = append(groups, fmt.Sprintf("%08x", mask[i]>>32), fmt.Sprintf("%08x", mask[i]&0xffffffff))
	}
	for len(groups) > 1 && groups[0] == "00000000" {
		groups = groups[1:]
	}
	if len(groups) == 0 {
		return "00000000"
	}
	return strings.Join(groups, ",")
}

// SetBit sets a CPU in a mask, growing it as needed.
func SetBit(mask []uint64, bit int) []uint64 {
	for len(mask) <= bit/64 {
		mask = append(mask, 0)
	}
	mask[bit/64] |= 1 << (bit % 64)
	return mask
}

// Intersects reports whether two masks share a CPU.
func Intersects(a, b []uint64) bool {
	for i := range a {
		if i < len(b) && a[i]&b[i] != 0 {
			return true
		}
	}
	return false
}

// OnlineCPUs returns how many of the host's CPUs are online, which the
// first line of /proc/stat covers.
func OnlineCPUs() int {
	mask, err := ParseCPUList(Read("/sys/devices/system/cpu/online"))
	if err != nil {
		return 0
	}
	n := 0
	for _, w := range mask {
		n += bits.OnesCount64(w)
	}
	return n
}
