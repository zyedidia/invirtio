package harness

import "testing"

func TestParseStat(t *testing.T) {
	stat := "1234 (linux (uml) x) S 1 1234 1234 0 -1 4194560 100 0 0 0 250 75 0 0 20 0 3 0"
	if got := ParseStat(stat); got != [2]int64{250, 75} {
		t.Errorf("ParseStat = %v", got)
	}
}

func TestMasks(t *testing.T) {
	m, err := ParseCPUList("0-3,8,40")
	if err != nil {
		t.Fatal(err)
	}
	if got := FormatMask(m); got != "00000100,0000010f" {
		t.Errorf("format = %q", got)
	}
	p, err := ParseMask("00000100,0000010f")
	if err != nil || FormatMask(p) != "00000100,0000010f" {
		t.Errorf("parse = %v, %v", p, err)
	}
	if got := FormatMask(SetBit(nil, 3)); got != "00000008" {
		t.Errorf("one CPU = %q", got)
	}
	if !Intersects(m, SetBit(nil, 40)) || Intersects(m, SetBit(nil, 5)) {
		t.Error("Intersects is wrong")
	}
}

func TestParseCPULine(t *testing.T) {
	busy, total := ParseCPULine("cpu  100 5 50 800 20 3 2 0 0 0")
	if busy != 160 || total != 980 {
		t.Errorf("ParseCPULine = %d, %d", busy, total)
	}
}
