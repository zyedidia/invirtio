package main

import (
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// The client detaches when it reads Ctrl-].
const detachKey = 0x1d

// terminalReset undoes the modes that a guest can leave on. It leaves VT52
// mode, in which the rest would not parse, and selects UTF-8 and the first
// character set. A soft reset then covers the cursor, the keys, the
// margins, insert mode, the character sets and the colors. The rest leaves
// the alternate screen and reverse video, and ends mouse tracking, focus
// events, bracketed paste and the extended keyboard modes.
const terminalReset = "\x1b<\x1b%G\x0f\x1b[!p" +
	"\x1b[?1049l\x1b[?5l\x1b[?9l\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l" +
	"\x1b[?1004l\x1b[?2004l\x1b[>4m\x1b[<99u\x1b[?25h\x1b(B\x1b[0m"

// utf8Locale reports whether the locale, as LC_ALL, LC_CTYPE and LANG give
// it, uses UTF-8.
func utf8Locale() bool {
	for _, v := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if l := os.Getenv(v); l != "" {
			l = strings.ToLower(l)
			return strings.Contains(l, "utf-8") || strings.Contains(l, "utf8")
		}
	}
	return false
}

const scrollback = 64 << 10

// A client starts the connection with its terminal's size, as two
// big-endian 16-bit numbers, rows first. Everything after that is the
// console's input and output.
const consoleHeader = 4

// consoleServer relays the guest's console, the master of the pty that the
// launcher runs on, to the clients attached to a Unix socket.
type consoleServer struct {
	pty      *os.File
	launcher *os.Process

	mu      sync.Mutex
	recent  []byte
	clients map[chan []byte]net.Conn
}

func newConsoleServer(pty *os.File, launcher *os.Process) *consoleServer {
	return &consoleServer{
		pty:      pty,
		launcher: launcher,
		clients:  map[chan []byte]net.Conn{},
	}
}

// output keeps the guest's output for later clients and passes it to the
// attached ones. A client that falls too far behind is dropped, so that it
// cannot hold up the guest's console.
func (s *consoleServer) output(p []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recent = append(s.recent, p...)
	if len(s.recent) > scrollback {
		s.recent = append([]byte(nil), s.recent[len(s.recent)-scrollback:]...)
	}
	for ch, conn := range s.clients {
		select {
		case ch <- append([]byte(nil), p...):
		default:
			delete(s.clients, ch)
			close(ch)
			conn.Close()
		}
	}
}

func (s *consoleServer) serve(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go s.attach(conn)
	}
}

func (s *consoleServer) attach(conn net.Conn) {
	var hdr [consoleHeader]byte
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		conn.Close()
		return
	}
	conn.SetReadDeadline(time.Time{})
	s.resize(binary.BigEndian.Uint16(hdr[0:]), binary.BigEndian.Uint16(hdr[2:]))

	ch := make(chan []byte, 256)
	s.mu.Lock()
	ch <- append([]byte(nil), s.recent...)
	s.clients[ch] = conn
	s.mu.Unlock()

	go func() {
		for p := range ch {
			if _, err := conn.Write(p); err != nil {
				conn.Close()
			}
		}
	}()

	io.Copy(s.pty, conn)
	s.mu.Lock()
	if _, ok := s.clients[ch]; ok {
		delete(s.clients, ch)
		close(ch)
	}
	s.mu.Unlock()
	conn.Close()
}

// resize sets the size of the launcher's terminal. The launcher is not in
// the terminal's foreground process group, so it is told with a signal.
func (s *consoleServer) resize(rows, cols uint16) {
	if rows == 0 || cols == 0 {
		return
	}
	ws := &unix.Winsize{Row: rows, Col: cols}
	if err := withFd(s.pty, func(fd int) error {
		return unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, ws)
	}); err != nil {
		return
	}
	s.launcher.Signal(syscall.SIGWINCH)
}

func (s *consoleServer) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch, conn := range s.clients {
		delete(s.clients, ch)
		close(ch)
		conn.Close()
	}
}

// withFd runs fn on a file's descriptor without Fd, which would make the
// file blocking and so end its read deadlines and its Close waking a read.
func withFd(f *os.File, fn func(fd int) error) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var ferr error
	if err := rc.Control(func(fd uintptr) { ferr = fn(int(fd)) }); err != nil {
		return err
	}
	return ferr
}

// openPty returns the master and the slave of a new pty.
func openPty() (*os.File, *os.File, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	var n int
	err = withFd(master, func(fd int) error {
		if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
			return err
		}
		var err error
		n, err = unix.IoctlGetInt(fd, unix.TIOCGPTN)
		return err
	})
	if err != nil {
		master.Close()
		return nil, nil, err
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return nil, nil, err
	}
	withFd(slave, func(fd int) error {
		return unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Row: 24, Col: 80})
	})
	return master, slave, nil
}

// escapeFilter passes the guest's output to a terminal without the escape
// sequences that carry strings, OSC, DCS, SOS, PM and APC, which can set
// the window title, write the clipboard or send the terminal commands. The
// control sequences that move the cursor and set colors pass. Text must be
// valid UTF-8, and the C1 controls are dropped, as single bytes and in
// UTF-8, since some terminals act on them. Those that open a string start
// one.
type escapeFilter struct {
	w     io.Writer
	state int
	// pending holds a UTF-8 sequence until it is complete.
	pending []byte
}

const (
	filterText = iota
	filterEscape
	filterString
	filterStringEscape
	// filterDrop drops an escape sequence with an intermediate byte, up to
	// its final byte.
	filterDrop
)

// opensString reports whether a C1 control, or the character that follows
// ESC for it, starts a string. ESC k starts the title string of screen.
func opensString(c rune) bool {
	switch c {
	case ']', 'P', 'X', '^', '_', 'k', 0x9d, 0x90, 0x98, 0x9e, 0x9f:
		return true
	}
	return false
}

func (f *escapeFilter) Write(p []byte) (int, error) {
	var out []byte
	for _, b := range p {
		out = f.feed(b, out)
	}
	if _, err := f.w.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (f *escapeFilter) feed(b byte, out []byte) []byte {
	switch f.state {
	case filterString, filterStringEscape:
		// A string ends at BEL or ST, as ESC \ or the C1 ST, and nothing in
		// it reaches the terminal.
		switch {
		case b == 0x07, b == 0x9c, f.state == filterStringEscape && b == '\\':
			f.state = filterText
		case b == 0x1b:
			f.state = filterStringEscape
		default:
			f.state = filterString
		}
		return out
	case filterEscape:
		// Terminals run a C0 control that follows ESC and stay in the
		// escape, ignore DEL there, and end the escape at CAN and SUB, so
		// the filter does the same.
		switch {
		case b == 0x1b, b == 0x7f:
			return out
		case b == 0x18, b == 0x1a:
			f.state = filterText
			return append(out, b)
		case b < 0x20:
			return append(out, b)
		case b >= 0x80:
			// ESC before a byte that is not ASCII is dropped, and the byte
			// is text.
			f.state = filterText
			return f.text(b, out)
		case opensString(rune(b)):
			f.state = filterString
			return out
		case b >= 0x20 && b <= 0x2f && !strings.ContainsRune("()*+-./", rune(b)):
			// Of the sequences with an intermediate byte, only the
			// character set designations pass. Others, such as ESC %
			// which leaves UTF-8 in xterm, could make the terminal read
			// text as C1 controls.
			f.state = filterDrop
			return out
		}
		f.state = filterText
		return append(out, 0x1b, b)
	case filterDrop:
		switch {
		case b == 0x1b:
			f.state = filterEscape
		case b == 0x18, b == 0x1a:
			f.state = filterText
			return append(out, b)
		case b < 0x20:
			return append(out, b)
		case b >= 0x30 && b <= 0x7e:
			f.state = filterText
		case b >= 0x80:
			f.state = filterText
			return f.text(b, out)
		}
		return out
	}
	return f.text(b, out)
}

func (f *escapeFilter) text(b byte, out []byte) []byte {
	if len(f.pending) == 0 && b < 0x80 {
		if b == 0x1b {
			f.state = filterEscape
			return out
		}
		return append(out, b)
	}
	f.pending = append(f.pending, b)
	if !utf8.FullRune(f.pending) {
		return out
	}
	r, size := utf8.DecodeRune(f.pending)
	seq := f.pending[:size]
	rest := append([]byte(nil), f.pending[size:]...)
	f.pending = f.pending[:0]
	switch {
	case r == utf8.RuneError && size == 1:
		// An invalid byte, such as a raw C1 control or the start of an
		// overlong encoding, is dropped, and a raw C1 that opens a string
		// starts one.
		if opensString(rune(seq[0])) {
			f.state = filterString
		}
	case r >= 0x80 && r <= 0x9f:
		if opensString(r) {
			f.state = filterString
		}
	default:
		out = append(out, seq...)
	}
	for _, c := range rest {
		out = f.feed(c, out)
	}
	return out
}

func cmdConsole(args []string) error {
	fs := flag.NewFlagSet("console", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: invirtio console <name>\n\nAttach to the console of a device's guest. Ctrl-] detaches.")
	}
	fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		os.Exit(2)
	}
	requireRoot()
	name := fs.Arg(0)
	if err := checkName(name); err != nil {
		return err
	}

	conn, err := net.Dial("unix", consolePath(name))
	if err != nil {
		return fmt.Errorf("cannot reach the console of %s, is invirtio@%s running? %v", name, name, err)
	}
	defer conn.Close()

	in := int(os.Stdin.Fd())
	var hdr [consoleHeader]byte
	if cols, rows, err := term.GetSize(in); err == nil {
		binary.BigEndian.PutUint16(hdr[0:], uint16(rows))
		binary.BigEndian.PutUint16(hdr[2:], uint16(cols))
	}
	if _, err := conn.Write(hdr[:]); err != nil {
		return err
	}

	if !utf8Locale() {
		warn("the locale is not UTF-8, and a terminal that is not in UTF-8 can take parts of UTF-8 text from the guest as controls")
	}
	var restore func()
	if term.IsTerminal(in) {
		old, err := term.MakeRaw(in)
		if err != nil {
			return err
		}
		restore = func() {
			os.Stdout.WriteString(terminalReset)
			term.Restore(in, old)
		}
	}
	// The terminal must read the guest's output as UTF-8, which the filter
	// checks it against.
	os.Stdout.WriteString("\x1b%G")
	fmt.Fprintf(os.Stderr, "Attached to %s. Press Ctrl-] to detach.\r\n", name)

	done := make(chan error, 3)
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT)
	go func() {
		<-sigs
		done <- nil
	}()
	output := make(chan struct{})
	go func() {
		defer close(output)
		_, err := io.Copy(&escapeFilter{w: os.Stdout}, conn)
		done <- err
	}()
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			for i := 0; i < n; i++ {
				if buf[i] == detachKey {
					conn.Write(buf[:i])
					done <- nil
					return
				}
			}
			if n > 0 {
				if _, err := conn.Write(buf[:n]); err != nil {
					done <- err
					return
				}
			}
			if err != nil {
				done <- nil
				return
			}
		}
	}()
	err = <-done
	// The guest's output stops before the terminal is reset, so that none
	// of it lands after the reset.
	conn.Close()
	<-output
	if restore != nil {
		restore()
	}
	fmt.Fprintf(os.Stderr, "\r\nDetached from %s.\r\n", name)
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}
