// Package mconsole talks to a UML guest's management console.
package mconsole

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"time"
)

const (
	mconsoleMagic   = 0xcafebabe
	mconsoleVersion = 2
	mconsoleMaxData = 512
	mconsoleHeader  = 12
	// The guest chooses how long a reply is, so it is bounded.
	mconsoleMaxReply = 1 << 20
)

// Request sends one command to a guest's management console, which the
// launcher relays from the socket at path, and returns the reply's text.
func Request(path, cmd string) (string, error) {
	if len(cmd) >= mconsoleMaxData {
		return "", fmt.Errorf("the command is longer than %d bytes", mconsoleMaxData-1)
	}
	// The relay replies to the sender's address, which must be bound.
	name := fmt.Sprintf("@invirtio-mconsole-%d-%d", os.Getpid(), time.Now().UnixNano())
	laddr := &net.UnixAddr{Name: name, Net: "unixgram"}
	raddr := &net.UnixAddr{Name: path, Net: "unixgram"}
	conn, err := net.DialUnix("unixgram", laddr, raddr)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return "", err
	}

	req := make([]byte, mconsoleHeader+len(cmd))
	binary.NativeEndian.PutUint32(req[0:], mconsoleMagic)
	binary.NativeEndian.PutUint32(req[4:], mconsoleVersion)
	binary.NativeEndian.PutUint32(req[8:], uint32(len(cmd)))
	copy(req[mconsoleHeader:], cmd)
	if _, err := conn.Write(req); err != nil {
		return "", err
	}

	// Each reply carries a chunk of the output, with a NUL counted in its
	// length. Only the first reply can report an error.
	var out []byte
	var replyErr uint32
	buf := make([]byte, mconsoleHeader+mconsoleMaxData)
	for first := true; ; first = false {
		n, err := conn.Read(buf)
		if err != nil {
			return "", err
		}
		if n < mconsoleHeader {
			return "", fmt.Errorf("short reply of %d bytes", n)
		}
		if first {
			replyErr = binary.NativeEndian.Uint32(buf[0:])
		}
		more := binary.NativeEndian.Uint32(buf[4:])
		end := min(mconsoleHeader+int(binary.NativeEndian.Uint32(buf[8:])), n)
		out = append(out, bytes.TrimRight(buf[mconsoleHeader:end], "\x00")...)
		if len(out) > mconsoleMaxReply {
			return "", fmt.Errorf("the reply is longer than %d bytes", mconsoleMaxReply)
		}
		if more == 0 {
			break
		}
	}
	if replyErr != 0 {
		return "", fmt.Errorf("%s", bytes.TrimSpace(out))
	}
	return string(out), nil
}
