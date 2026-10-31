package harness

import "testing"

func TestGuestMemory(t *testing.T) {
	var m GuestMemory
	m.SampleGuest("MemTotal:       786432 kB\nMemFree:        600000 kB\nMemAvailable:   655360 kB\n")
	m.SampleGuest("MemTotal:       786432 kB\nMemAvailable:   720896 kB\n")
	m.SampleGuest("not meminfo")
	if m.GuestTotalBytes != 768<<20 || m.GuestPeakUsedBytes != 128<<20 {
		t.Errorf("SampleGuest = %d of %d", m.GuestPeakUsedBytes, m.GuestTotalBytes)
	}
	st := ParseKeyed("anon 2097152\nshmem 8388608\n")
	if st["anon"] != 2<<20 || st["shmem"] != 8<<20 {
		t.Errorf("ParseKeyed = %v", st)
	}
}
