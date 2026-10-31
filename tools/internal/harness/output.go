package harness

import (
	"io"
	"os"
)

var output struct {
	file   *os.File
	stdout *os.File
	stderr *os.File
	pipe   *os.File
	done   chan struct{}
}

// logWriter writes to the log file and then to the terminal. The terminal
// can go away on a hangup, and the log still gets the rest.
type logWriter struct {
	file, term *os.File
}

func (w logWriter) Write(p []byte) (int, error) {
	w.file.Write(p)
	w.term.Write(p)
	return len(p), nil
}

// TeeOutput copies everything that the program writes to its stdout and
// stderr to a log file at path, which it replaces. Both go through one pipe,
// so that the log keeps their order, and reach the terminal on stdout.
func TeeOutput(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	r, w, err := os.Pipe()
	if err != nil {
		f.Close()
		return err
	}
	output.file, output.stdout, output.stderr, output.pipe = f, os.Stdout, os.Stderr, w
	output.done = make(chan struct{})
	go func() {
		io.Copy(logWriter{f, output.stdout}, r)
		r.Close()
		close(output.done)
	}()
	os.Stdout, os.Stderr = w, w
	return nil
}

// CloseOutput writes out what TeeOutput has not copied yet and restores
// stdout and stderr. The program calls it before it exits.
func CloseOutput() {
	if output.file == nil {
		return
	}
	os.Stdout, os.Stderr = output.stdout, output.stderr
	output.pipe.Close()
	<-output.done
	output.file.Close()
	output.file = nil
}
