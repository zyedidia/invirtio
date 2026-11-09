package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestTeeOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.log")
	if err := TeeOutput(path); err != nil {
		t.Fatal(err)
	}
	fmt.Println("to stdout")
	fmt.Fprintln(os.Stderr, "to stderr")
	CloseOutput()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "to stdout\nto stderr\n" && string(data) != "to stderr\nto stdout\n" {
		t.Errorf("log = %q, %v", data, err)
	}
}

func TestParseSection(t *testing.T) {
	text := "dma\n  premapped          5\n  maps               12\niomem\n  faulted_accesses   3\n" +
		"0000:06:00.0\n  maps               99\n"
	c := ParseSection(text, "dma")
	if c["premapped"] != 5 || c["maps"] != 12 || len(c) != 2 {
		t.Errorf("ParseSection = %v", c)
	}
	if len(ParseSection(text, "missing")) != 0 {
		t.Error("ParseSection found a missing section")
	}
}
