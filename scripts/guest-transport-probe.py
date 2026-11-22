"""Record actual Docker control frames and inject one selected transport failure."""

import argparse
import http.client
import http.server
import json
import os
import socket
import socketserver
import struct
import subprocess
import tempfile
import threading
import time
import uuid
from pathlib import Path


class DockerConnection(http.client.HTTPConnection):
    def __init__(self, path):
        super().__init__("docker", timeout=20)
        self.path = path

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX)
        self.sock.settimeout(self.timeout)
        self.sock.connect(self.path)


class Engine(socketserver.ThreadingUnixStreamServer):
    daemon_threads = True

    def __init__(self, path, upstream):
        super().__init__(path, Handler)
        self.upstream = upstream
        self.fault = ""
        self.calls = []
        self.frames = []


class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def handle_one(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
        path = self.path.partition("?")[0]
        self.server.calls.append((self.command, path))
        if self.headers.get("Upgrade", "").lower() == "tcp":
            self.upgrade(body)
            return
        c = DockerConnection(self.server.upstream)
        try:
            c.request(
                self.command,
                self.path,
                body,
                {
                    "Content-Type": self.headers.get(
                        "Content-Type", "application/json"
                    ),
                    "Connection": "close",
                },
            )
            reply = c.getresponse()
            data = reply.read()
            if (
                "/images/" in path
                and path.endswith("/json")
                and self.server.fault == "missing_arch"
                and reply.status == 200
            ):
                image = json.loads(data)
                image.pop("Architecture", None)
                data = json.dumps(image).encode()
            self.send_response(reply.status)
            self.send_header("Content-Length", str(len(data)))
            self.send_header(
                "Content-Type", reply.getheader("Content-Type", "application/json")
            )
            self.end_headers()
            self.wfile.write(data)
        finally:
            c.close()

    def upgrade(self, body):
        with socket.socket(socket.AF_UNIX) as upstream:
            upstream.connect(self.server.upstream)
            request = (
                f"{self.command} {self.path} HTTP/1.1\r\n"
                + "".join(f"{k}: {v}\r\n" for k, v in self.headers.items())
                + "\r\n"
            )
            upstream.sendall(request.encode() + body)
            with upstream.makefile("rb") as inbound:
                while True:
                    line = inbound.readline()
                    if not line:
                        return
                    self.wfile.write(line)
                    if line == b"\r\n":
                        break
                while True:
                    head = self.rfile.read(5)
                    if len(head) != 5:
                        return
                    n = int.from_bytes(head[1:], "big")
                    raw = self.rfile.read(n)
                    if len(raw) != n:
                        return
                    kind = chr(head[0])
                    req = json.loads(raw)
                    op = req.get("Op", "")
                    self.server.frames.append(
                        {
                            "kind": kind,
                            "op": op,
                            "id": req.get("ID", ""),
                            "request_bytes": n,
                        }
                    )
                    upstream.sendall(head + raw)
                    data = bytearray()
                    while len(data) < 5 or len(data) < 5 + int.from_bytes(
                        data[1:5], "big"
                    ):
                        frame = inbound.read(8)
                        if len(frame) != 8:
                            return
                        chunk = inbound.read(int.from_bytes(frame[4:], "big"))
                        if frame[0] == 1:
                            data.extend(chunk)
                    if (
                        kind == "C"
                        and op == "start"
                        and self.server.fault == "lost_start"
                    ):
                        self.server.fault = ""
                        return
                    if kind == "H" and self.server.fault == "version":
                        data = data.replace(b'"Version":1', b'"Version":9')
                    if kind == "S" and self.server.fault == "oversize":
                        self.server.fault = ""
                        data = b"s" + struct.pack(">I", (1 << 20) + 1)
                    self.wfile.write(
                        b"\x01\x00\x00\x00" + struct.pack(">I", len(data)) + data
                    )

    do_GET = do_POST = do_PUT = do_DELETE = handle_one


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server", type=Path, default=Path("bin/sandboxd"))
    parser.add_argument("--out", type=Path, default=Path("artifacts/guest-transport"))
    args = parser.parse_args()
    args.out.mkdir(parents=True, exist_ok=True)
    owner = "guest-wire-" + uuid.uuid4().hex

    def docker(*args):
        return subprocess.check_output(["docker", *args], text=True).strip()

    upstream = os.environ.get("DOCKER_HOST") or docker(
        "context", "inspect", "--format", "{{.Endpoints.docker.Host}}"
    )
    with tempfile.TemporaryDirectory(dir="/tmp") as d, socket.socket() as port_alloc:
        port_alloc.bind(("127.0.0.1", 0))
        port = port_alloc.getsockname()[1]
        port_alloc.close()
        socket_path = str(Path(d) / "docker.sock")
        with (
            Engine(socket_path, upstream.removeprefix("unix://")) as proxy,
            (args.out / "daemon.log").open("w") as log,
        ):
            threading.Thread(target=proxy.serve_forever, daemon=True).start()
            server = subprocess.Popen(
                [
                    str(args.server.resolve()),
                    "-docker",
                    socket_path,
                    "-owner",
                    owner,
                    "-addr",
                    f"127.0.0.1:{port}",
                ],
                stdout=log,
                stderr=log,
            )

            def call(method, path, payload=None, expect=200):
                c = http.client.HTTPConnection("127.0.0.1", port, timeout=15)
                try:
                    c.request(
                        method,
                        path,
                        json.dumps(payload) if payload is not None else None,
                        {"Content-Type": "application/json"},
                    )
                    r = c.getresponse()
                    value = json.loads(r.read())
                    assert r.status == expect, (method, path, r.status, value)
                    return value
                finally:
                    c.close()

            report = {}

            def record(name, value):
                report[name] = value
                (args.out / "report.json").write_text(
                    json.dumps(report, indent=2) + "\n"
                )

            try:
                for _ in range(150):
                    try:
                        call("GET", "/readyz")
                        break
                    except OSError:
                        time.sleep(0.04)

                def create():
                    sb = call(
                        "POST",
                        "/v1/sandboxes",
                        {
                            "image": "python:3.12-alpine",
                            "network": "none",
                            "max_concurrent_executions": 2,
                        },
                        201,
                    )
                    return sb, "/v1/sandboxes/" + sb["id"]

                for fault in ["missing_arch", "version"]:
                    proxy.fault = fault
                    sb, base = create()
                    assert sb["agent_mode"] == "unavailable" and not sb["protected"], sb
                    assert (
                        call("GET", base + "/files/stat?path=/bin/sh")["type"]
                        == "symlink"
                    )
                    if fault == "missing_arch":
                        cid = "tiny-sandbox-" + sb["id"]
                        actual = docker(
                            "exec", cid, "sha256sum", "/.tiny-sandbox/guest"
                        ).split()[0]
                        from hashlib import sha256
                        import platform

                        arch = {
                            "x86_64": "amd64",
                            "arm64": "arm64",
                            "aarch64": "arm64",
                        }[platform.machine()]
                        local = (
                            Path(__file__).resolve().parents[1]
                            / "internal/dockerbackend/guestbin"
                            / ("guest-linux-" + arch)
                        )
                        assert sha256(local.read_bytes()).hexdigest() == actual
                    record(fault, {"agent_mode": sb["agent_mode"], "legacy_stat": True})
                    call("DELETE", base)
                proxy.fault = ""
                sb, base = create()
                assert sb["agent_mode"] == "guest"
                proxy.fault = "lost_start"
                request = {
                    "command": [
                        "sh",
                        "-c",
                        "echo once >>/tmp/run-once; cat /tmp/run-once",
                    ],
                    "timeout_ms": 3000,
                    "idempotency_key": "lost-start",
                }
                ex = call("POST", base + "/executions", request, 201)
                eid = ex["id"]
                for _ in range(100):
                    ex = call("GET", base + "/executions/" + eid)
                    if ex["state"] == "completed":
                        break
                    time.sleep(0.03)
                assert ex["exit_code"] == 0 and ex["stdout"] == "once\n", ex
                assert call("POST", base + "/executions", request)["id"] == eid
                events = [e for e in proxy.frames if e["id"] == eid]
                assert sum(e["op"] == "start" for e in events) == 1 and any(
                    e["op"] == "read" for e in events
                ), events
                record("lost_start_reply", {"id": eid, "result": ex, "frames": events})
                proxy.fault = "oversize"
                error = call("GET", base + "/files/stat?path=/tmp", expect=502)
                assert error["code"] == "backend_error"
                count = len([e for e in proxy.frames if e["kind"] == "H"])
                assert call("GET", base + "/files/stat?path=/tmp")["type"] == "dir"
                assert sum(e["kind"] == "H" for e in proxy.frames) == count + 1
                record(
                    "oversize_frame",
                    {
                        "declared_bytes": (1 << 20) + 1,
                        "error": error,
                        "reconnect_verified_version": True,
                    },
                )
                creation = next(
                    i
                    for i, (_, p) in enumerate(proxy.calls)
                    if p.endswith("/containers/create")
                )
                assert any(
                    "/images/" in p and p.endswith("/json")
                    for _, p in proxy.calls[:creation]
                )
                call("DELETE", base)
                record("image_inspected_before_create", True)
            finally:
                server.terminate()
                server.wait(timeout=30)
                proxy.shutdown()
                assert not docker(
                    "ps", "-aq", "--filter", "label=tiny-sandbox.owner=" + owner
                )
            record("cleanup", True)


if __name__ == "__main__":
    main()
