// Command uml-mconsole sends one command to a running UML guest's management
// console and prints the reply.
//
// Usage:
//
//	uml-mconsole <umid | socket path> <command...>
//
// The umid is the one given to the guest with umid=, and the socket is
// ~/.uml/<umid>/mconsole of the user running the guest. Examples:
//
//	uml-mconsole vfio proc vfio_uml
//	uml-mconsole vfio proc interrupts
//	uml-mconsole vfio help
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	magic      = 0xcafebabe
	version    = 2
	maxData    = 512
	headerSize = 12
	timeout    = 10 * time.Second
)

func usage() {
	fmt.Fprintln(os.Stderr, "usage: uml-mconsole <umid | socket path> <command...>")
	os.Exit(2)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "uml-mconsole:", err)
	os.Exit(1)
}

func socketPath(target string) (string, error) {
	if strings.Contains(target, "/") {
		return target, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".uml", target, "mconsole"), nil
}

// request sends cmd to the console at path and returns the reply text and the
// error flag of the first reply.
func request(path, cmd string) (string, uint32, error) {
	// The guest replies to the sender's address. An abstract address has no
	// file permissions, so a guest running as another user can reply to it.
	name := fmt.Sprintf("@uml-mconsole-%d-%d", os.Getpid(), time.Now().UnixNano())
	laddr := &net.UnixAddr{Name: name, Net: "unixgram"}
	raddr := &net.UnixAddr{Name: path, Net: "unixgram"}
	conn, err := net.DialUnix("unixgram", laddr, raddr)
	if err != nil {
		return "", 0, err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return "", 0, err
	}

	req := make([]byte, headerSize+len(cmd))
	binary.NativeEndian.PutUint32(req[0:], magic)
	binary.NativeEndian.PutUint32(req[4:], version)
	binary.NativeEndian.PutUint32(req[8:], uint32(len(cmd)))
	copy(req[headerSize:], cmd)
	if _, err := conn.Write(req); err != nil {
		return "", 0, err
	}

	// Each reply carries a chunk of the output, with a NUL counted in its
	// length. Only the first reply can report an error.
	var out []byte
	var replyErr uint32
	buf := make([]byte, headerSize+maxData)
	for first := true; ; first = false {
		n, err := conn.Read(buf)
		if err != nil {
			return "", 0, err
		}
		if n < headerSize {
			return "", 0, fmt.Errorf("short reply of %d bytes", n)
		}
		if first {
			replyErr = binary.NativeEndian.Uint32(buf[0:])
		}
		more := binary.NativeEndian.Uint32(buf[4:])
		end := headerSize + int(binary.NativeEndian.Uint32(buf[8:]))
		end = min(end, n)
		out = append(out, bytes.TrimRight(buf[headerSize:end], "\x00")...)
		if more == 0 {
			break
		}
	}

	return string(out), replyErr, nil
}

func main() {
	if len(os.Args) < 3 {
		usage()
	}

	path, err := socketPath(os.Args[1])
	if err != nil {
		fatal(err)
	}
	cmd := strings.Join(os.Args[2:], " ")
	if len(cmd) >= maxData {
		fatal(fmt.Errorf("command is longer than %d bytes", maxData-1))
	}

	text, replyErr, err := request(path, cmd)
	if err != nil {
		fatal(err)
	}

	text = strings.TrimLeft(text, "\n")
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	os.Stdout.WriteString(text)
	if replyErr != 0 {
		os.Exit(1)
	}
}
