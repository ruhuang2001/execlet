"""Measure Docker inspect traffic and PTY echo latency while a real managed command sleeps."""

import argparse, base64, json, secrets, socket, socketserver, statistics, subprocess, tempfile, threading, time
from pathlib import Path
from http.client import HTTPConnection


def run(args):
    owner = "guest-idle-" + secrets.token_hex(5)
    upstream = (
        subprocess.check_output(
            ["docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}"],
            text=True,
        )
        .strip()
        .removeprefix("unix://")
    )
    inspections = []

    class Relay(socketserver.BaseRequestHandler):
        def handle(self):
            with socket.socket(socket.AF_UNIX) as target:
                target.connect(upstream)

                def pipe(source, dest, count):
                    try:
                        while data := source.recv(65536):
                            if (
                                count
                                and b"GET /v" in data
                                and b"/containers/" in data
                                and b"/json HTTP" in data
                            ):
                                inspections.append(time.monotonic())
                            dest.sendall(data)
                    except OSError:
                        pass
                    finally:
                        try:
                            dest.shutdown(socket.SHUT_WR)
                        except OSError:
                            pass

                t = threading.Thread(
                    target=pipe, args=(target, self.request, False), daemon=True
                )
                t.start()
                pipe(self.request, target, True)
                t.join()

    class Proxy(socketserver.ThreadingUnixStreamServer):
        daemon_threads = True

    with tempfile.TemporaryDirectory(dir="/tmp") as tmp:
        with Proxy(tmp + "/docker.sock", Relay) as proxy:
            threading.Thread(target=proxy.serve_forever, daemon=True).start()
            with socket.socket() as s:
                s.bind(("127.0.0.1", 0))
                port = s.getsockname()[1]

            def call(method, path, body=None):
                c = HTTPConnection("127.0.0.1", port, timeout=10)
                try:
                    c.request(
                        method,
                        path,
                        json.dumps(body) if isinstance(body, dict) else body,
                    )
                    r = c.getresponse()
                    b = r.read()
                    assert 200 <= r.status < 300, (r.status, b)
                    return json.loads(b) if b else None
                finally:
                    c.close()

            with open(tmp + "/daemon.log", "w") as log:
                d = subprocess.Popen(
                    [
                        str(args.server.resolve()),
                        "-docker",
                        tmp + "/docker.sock",
                        "-owner",
                        owner,
                        "-addr",
                        f"127.0.0.1:{port}",
                    ],
                    stdout=log,
                    stderr=log,
                )
                try:
                    for _ in range(100):
                        try:
                            call("GET", "/readyz")
                            break
                        except OSError:
                            time.sleep(0.03)
                    sb = call(
                        "POST",
                        "/v1/sandboxes",
                        {
                            "image": "alpine:3.21",
                            "network": "none",
                            "max_concurrent_executions": 2,
                        },
                    )
                    b = "/v1/sandboxes/" + sb["id"]
                    ex = call(
                        "POST",
                        b + "/executions",
                        {"command": ["sleep", "90"], "timeout_ms": 0},
                    )
                    tid = call("POST", b + "/terminals", {"command": ["cat"]})[
                        "terminal_id"
                    ]
                    p = b + "/terminals/" + tid
                    stream = HTTPConnection("127.0.0.1", port, timeout=10)
                    stream.request("GET", p + "/events")
                    r = stream.getresponse()
                    start = time.monotonic()
                    time.sleep(2.5)
                    elapsed = time.monotonic() - start
                    rate = sum(x >= start for x in inspections) / elapsed
                    times = []
                    raw = b""
                    for i in range(10):
                        marker = ("echo-%02d" % i).encode()
                        t = time.monotonic()
                        call("POST", p + "/input", marker + b"\n")
                        while marker not in raw:
                            line = r.readline()
                            assert line
                            if line.startswith(b"data:"):
                                raw += base64.b64decode(json.loads(line[5:])["data"])
                        times.append((time.monotonic() - t) * 1000)
                        time.sleep(0.016 + i * 0.004)
                    r.close()
                    stream.close()
                    call("POST", b + "/executions/" + ex["id"] + "/kill")
                    call("DELETE", p)
                    call("DELETE", b + "?purge=true")
                    data = {
                        "idle_inspects_per_second": rate,
                        "echo_ms": times,
                        "echo_p50_ms": statistics.median(times),
                        "echo_p95_ms": max(times),
                    }
                    args.out.parent.mkdir(exist_ok=True, parents=True)
                    args.out.write_text(json.dumps(data, indent=2) + "\n")
                    if args.verify:
                        assert rate < 6 and data["echo_p50_ms"] < 40, data
                finally:
                    d.terminate()
                    d.wait(timeout=30)
                    proxy.shutdown()
                    assert not subprocess.check_output(
                        [
                            "docker",
                            "ps",
                            "-aq",
                            "--filter",
                            "label=tiny-sandbox.owner=" + owner,
                        ]
                    ).strip()


if __name__ == "__main__":
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--server", type=Path, required=True)
    p.add_argument("--out", type=Path, required=True)
    p.add_argument("--verify", action="store_true")
    run(p.parse_args())
