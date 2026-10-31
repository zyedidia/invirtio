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
