#!/usr/bin/env python3
"""Send one command to a running UML guest's management console.

usage: uml-mconsole.py <umid | socket path> <command...>

The umid is the one given to the guest with umid=, and the socket is
~/.uml/<umid>/mconsole of the user running the guest. Examples:

    uml-mconsole.py vfio proc vfio_uml
    uml-mconsole.py vfio proc interrupts
    uml-mconsole.py vfio help
"""

import os
import socket
import struct
import sys
import tempfile

MAGIC = 0xCAFEBABE
VERSION = 2
MAX_DATA = 512
HEADER = struct.Struct("=III")


def socket_path(target):
    if "/" in target:
        return target
    return os.path.expanduser(os.path.join("~/.uml", target, "mconsole"))


def main():
    if len(sys.argv) < 3:
        print(__doc__.strip(), file=sys.stderr)
        return 2

    path = socket_path(sys.argv[1])
    command = " ".join(sys.argv[2:]).encode()
    if len(command) >= MAX_DATA:
        print("command is too long", file=sys.stderr)
        return 2

    with tempfile.TemporaryDirectory() as tmp:
        sock = socket.socket(socket.AF_UNIX, socket.SOCK_DGRAM)
        sock.bind(os.path.join(tmp, "client"))
        sock.settimeout(10)
        sock.sendto(HEADER.pack(MAGIC, VERSION, len(command)) + command, path)

        # Each reply carries a chunk of the output, with a NUL counted in
        # its length. Only the first reply can report an error.
        out = b""
        err = None
        while True:
            reply = sock.recv(HEADER.size + MAX_DATA)
            chunk_err, more, length = HEADER.unpack_from(reply)
            if err is None:
                err = chunk_err
            out += reply[HEADER.size:HEADER.size + length].rstrip(b"\0")
            if not more:
                break
        sock.close()

    text = out.decode(errors="replace").lstrip("\n")
    if text and not text.endswith("\n"):
        text += "\n"
    sys.stdout.write(text)
    return 1 if err else 0


if __name__ == "__main__":
    sys.exit(main())
