"""Exercise the guest control boundary through sandboxd HTTP and a real Docker Engine."""

import argparse
import hashlib
import json
import socket
import subprocess
import time
import tempfile
import uuid
from http.client import HTTPConnection
from pathlib import Path
from urllib.parse import urlencode


def run(binary, out, image):
    out.mkdir(parents=True, exist_ok=True)
    owner = "guest-boundaries-" + uuid.uuid4().hex
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        port = s.getsockname()[1]

    def call(method, url, data=None):
        c = HTTPConnection("127.0.0.1", port, timeout=15)
        try:
            c.request(
                method,
                url,
                json.dumps(data) if data is not None else None,
                {"Content-Type": "application/json"},
            )
            r = c.getresponse()
            body = r.read()
            return r.status, json.loads(body) if body else None
        finally:
            c.close()

    def ok(method, url, data=None):
        status, body = call(method, url, data)
        assert 200 <= status < 300, (status, body)
        return body

    def docker(*args):
        return subprocess.check_output(["docker", *args], text=True).strip()

    report = {}

    def record(key, value):
        report[key] = value
        (out / "report.json").write_text(json.dumps(report, indent=2) + "\n")

    pages = {}
    for mode in ["auto", "off"]:
        with (
            tempfile.TemporaryDirectory(prefix="guest-journal-") as journal,
            (out / (mode + ".log")).open("w") as log,
        ):
            daemon = subprocess.Popen(
                [
                    str(binary.resolve()),
                    "-addr",
                    f"127.0.0.1:{port}",
                    "-owner",
                    owner,
                    "-guest-agent",
                    mode,
                    "-state-dir",
                    journal,
                ],
                stdout=log,
                stderr=log,
            )
            try:
                for _ in range(120):
                    try:
                        ok("GET", "/readyz")
                        break
                    except OSError:
                        time.sleep(0.05)
                sb = ok(
                    "POST",
                    "/v1/sandboxes",
                    {"image": image, "network": "none", "max_concurrent_executions": 2},
                )
                base = "/v1/sandboxes/" + sb["id"]
                cid = "tiny-sandbox-" + sb["id"]
                assert sb["agent_mode"] == ("guest" if mode == "auto" else "legacy")
                if mode == "auto":
                    p = "/tmp"
                    ok("GET", base + "/files/stat?path=" + p)
                    command = ok(
                        "POST",
                        base + "/executions",
                        {
                            "command": [
                                "sh",
                                "-c",
                                "while [ ! -f /tmp/finish ]; do sleep .05; done; exit 9",
                            ],
                            "timeout_ms": 0,
                        },
                    )
                    job = "/executions/" + command["id"]
                    rejected, error = call("POST", base + "/executions", {
                        "command": ["/bin/true"] + ["x" * 1022] * 1024, "timeout_ms": 0,
                    })
                    assert rejected == 413 and error["code"] == "payload_too_large", (rejected, error)
                    assert ok("GET", base + job)["state"] == "running"
                    term = (
                        "/terminals/"
                        + ok("POST", base + "/terminals", {})["terminal_id"]
                    )
                    docker("pause", cid)
                    refused = []
                    try:
                        for method, route, payload in [
                            ("GET", "/files/stat?path=/tmp", None),
                            ("GET", "/files/list?path=/tmp", None),
                            ("POST", "/terminals", {}),
                            ("POST", job + "/cancel", {}),
                            ("POST", job + "/kill", {}),
                            ("POST", term + "/input", "hello"),
                            ("POST", term + "/resize", {"rows": 25, "cols": 80}),
                            ("GET", term + "/events", None),
                            ("DELETE", term, None),
                            (
                                "POST",
                                "/executions",
                                {"command": ["true"], "timeout_ms": 1000},
                            ),
                        ]:
                            t = time.monotonic()
                            status, body = call(method, base + route, payload)
                            elapsed = time.monotonic() - t
                            refused.append(
                                {
                                    "route": route,
                                    "status": status,
                                    "code": body.get("code"),
                                    "elapsed_seconds": elapsed,
                                }
                            )
                            record("docker_paused", refused)
                            assert (
                                status == 409
                                and body["code"] == "invalid_state"
                                and elapsed < 1.5
                            ), refused
                        assert (
                            docker("inspect", "--format", "{{.State.Paused}}", cid)
                            == "true"
                        )
                    finally:
                        docker("unpause", cid)
                    assert [
                        ex["id"] for ex in ok("GET", base + "/executions")["executions"]
                    ] == [command["id"]]
                    docker("exec", cid, "touch", "/tmp/finish")
                    for _ in range(100):
                        final = ok("GET", base + job)
                        if final["state"] == "completed":
                            break
                        time.sleep(0.03)
                    assert (
                        final["exit_code"] == 9
                        and final["termination_reason"] == "exited"
                    ), final
                    ok("DELETE", base + term)
                    assert ok("GET", base + "/files/stat?path=/tmp")["type"] == "dir"
                    ok("POST", base + "/pause")
                    assert ok("GET", base + "/files/stat?path=/tmp")["type"] == "dir"
                    assert ok("GET", base)["state"] == "running"
                    record("api_paused_resumed", True)
                setup = """import os
os.mkdir('/tmp/links')
target='target/'*240
for i in range(1001):
 p='/tmp/links/link%04d'%i
 os.symlink(target,p)
 os.utime(p,(1700000000,1700000000),follow_symlinks=False)
"""
                docker("exec", cid, "python3", "-c", setup)
                page = ok("GET", base + "/files/list?path=/tmp/links&limit=1000")
                entries = page["entries"]
                assert len(entries) == 1000 and "next_cursor" in page
                tail = ok(
                    "GET",
                    base
                    + "/files/list?"
                    + urlencode(
                        {
                            "path": "/tmp/links",
                            "cursor": page["next_cursor"],
                            "limit": 1000,
                        }
                    ),
                )
                assert len(tail["entries"]) == 1 and not tail.get("next_cursor")
                entries += tail["entries"]
                assert [e["name"] for e in entries] == [
                    "link%04d" % i for i in range(1001)
                ]
                raw = json.dumps(
                    entries, sort_keys=True, separators=(",", ":")
                ).encode()
                assert len(raw) > 1 << 20
                pages[mode] = {
                    "bytes": len(raw),
                    "sha256": hashlib.sha256(raw).hexdigest(),
                    "entries": len(entries),
                }
                record("large_page", pages)
                if mode == "auto":
                    docker(
                        "exec",
                        cid,
                        "python3",
                        "-c",
                        "import os; os.mkdir('/tmp/many'); [open('/tmp/many/f%d'%i,'w').close() for i in range(10001)]",
                    )
                    status, body = call("GET", base + "/files/list?path=/tmp/many")
                    assert status == 413 and body["code"] == "payload_too_large"
                    record("over_10000", body)
                if mode == "auto":
                    architectures = {}
                    for arch, digest in (
                        (
                            "amd64",
                            "sha256:2d8a11041a92082ba21e364dec73559ce44f1de69b60fa68315f6d3774e59117",
                        ),
                        (
                            "arm64",
                            "sha256:e77b401c86e6d0651b12ca45dea812bdf6ffca3b6e16662b7c116180e56ec9f7",
                        ),
                    ):
                        ref = "alpine@" + digest
                        docker("pull", "--platform=linux/" + arch, ref)
                        other = ok(
                            "POST", "/v1/sandboxes", {"image": ref, "network": "none"}
                        )
                        try:
                            oid = "tiny-sandbox-" + other["id"]
                            assert other["agent_mode"] == "guest", other
                            inspected = docker(
                                "image", "inspect", "--format", "{{.Architecture}}", ref
                            )
                            assert inspected == arch
                            actual = docker(
                                "exec", oid, "sha256sum", "/.tiny-sandbox/guest"
                            ).split()[0]
                            wanted = hashlib.sha256(
                                (
                                    Path(__file__).resolve().parents[1]
                                    / "internal/dockerbackend/guestbin"
                                    / ("guest-linux-" + arch)
                                ).read_bytes()
                            ).hexdigest()
                            assert actual == wanted
                            st = ok(
                                "GET",
                                "/v1/sandboxes/"
                                + other["id"]
                                + "/files/stat?path=/.tiny-sandbox/guest",
                            )
                            assert st["mode"] == 0o755 and st["uid"] == st["gid"] == 0
                            socket_stat = ok(
                                "GET",
                                "/v1/sandboxes/"
                                + other["id"]
                                + "/files/stat?path=/run/tiny-sandbox/agent.sock",
                            )
                            assert (
                                socket_stat["mode"] == 0o600
                                and socket_stat["type"] == "other"
                            )
                            architectures[arch] = {
                                "digest": digest,
                                "binary_sha256": actual,
                                "agent_mode": other["agent_mode"],
                                "file_mode": st["mode"],
                                "socket_mode": socket_stat["mode"],
                            }
                        finally:
                            ok("DELETE", "/v1/sandboxes/" + other["id"])
                    record("image_architectures", architectures)
                ok("DELETE", base)
            finally:
                for cleanup_id in docker(
                    "ps", "-aq", "--filter", "label=tiny-sandbox.owner=" + owner
                ).split():
                    docker("rm", "-f", cleanup_id)
                daemon.terminate()
                daemon.wait(timeout=30)
                assert not docker(
                    "ps", "-aq", "--filter", "label=tiny-sandbox.owner=" + owner
                )
    assert pages["auto"] == pages["off"], pages
    record("cleanup", True)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server", type=Path, default=Path("bin/sandboxd"))
    parser.add_argument("--out", type=Path, default=Path("artifacts/guest-boundaries"))
    parser.add_argument("--image", default="python:3.12-alpine")
    args = parser.parse_args()
    run(args.server, args.out, args.image)
