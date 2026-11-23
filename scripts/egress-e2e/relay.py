"""Exercise both half-close directions of the installed proxy-accept helper."""

import socket
import subprocess
import sys
from pathlib import Path

path = Path("/run/tiny-sandbox/egress.sock")
path.parent.mkdir(exist_ok=True)
with socket.socket(socket.AF_UNIX) as listener:
    listener.bind(str(path))
    listener.listen()
    listener.settimeout(5)
    for stdin_first in (False, True):
        with subprocess.Popen([sys.argv[1], "proxy-accept"], stdin=subprocess.PIPE,
                              stdout=subprocess.PIPE) as proc:
            try:
                conn, _ = listener.accept()
                with conn:
                    conn.settimeout(5)
                    if not stdin_first:
                        conn.sendall(b"request")
                        conn.shutdown(socket.SHUT_WR)
                        assert proc.stdout.read() == b"request"
                    proc.stdin.write(b"response")
                    proc.stdin.close()
                    with conn.makefile("rb") as reader:
                        assert reader.read() == b"response"
                    if stdin_first:
                        conn.sendall(b"request")
                        conn.shutdown(socket.SHUT_WR)
                        assert proc.stdout.read() == b"request"
                assert proc.wait(timeout=5) == 0
            finally:
                if proc.poll() is None:
                    proc.kill()
