"""Verify policy admission against real runc and runsc on a Linux Docker host."""

import argparse
import hashlib
import io
import json
import tarfile
import os
import platform
import socket
import subprocess
import time
import uuid
from http.client import HTTPConnection
from pathlib import Path


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server", type=Path, default=Path("bin/sandboxd"))
    parser.add_argument("--image", default="tiny-sandbox-guest:minimal")
    parser.add_argument("--out", type=Path, default=Path("artifacts/runtime"))
    parser.add_argument(
        "--engine-default",
        action="store_true",
        help="verify the Docker daemon default runtime without a policy override",
    )
    args = parser.parse_args()
    args.out.mkdir(parents=True, exist_ok=True)
    owner = "runtime-e2e-" + uuid.uuid4().hex
    spec = dict(
        image=args.image,
        memory_mb=256,
        pids_limit=128,
        nano_cpus=500000000,
        network="none",
    )
    policy = dict(
        max_sandboxes=2,
        total_memory_mb=512,
        total_nano_cpus=1000000000,
        total_pids=256,
        allowed_networks=["none"],
        allowed_images=[args.image],
        allowed_runtimes=["runc", "runsc"],
    )
    if args.engine_default:
        del policy["allowed_runtimes"]
    policy_path = args.out / "policy.json"
    policy_path.write_text(json.dumps(policy))
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        port = sock.getsockname()[1]

    def http(method, path, body=None):
        c = HTTPConnection("127.0.0.1", port, timeout=30)
        try:
            data = (
                body
                if isinstance(body, bytes)
                else json.dumps(body).encode()
                if body is not None
                else None
            )
            c.request(
                method,
                path,
                data,
                {
                    "Content-Type": "application/octet-stream"
                    if isinstance(body, bytes)
                    else "application/json"
                },
            )
            r = c.getresponse()
            raw = r.read()
            return r.status, (
                json.loads(raw)
                if r.getheader("Content-Type", "").startswith("application/json")
                else raw
            )
        finally:
            c.close()

    def request(method, path, body=None):
        code, obj = http(method, path, body)
        assert 200 <= code < 300, (code, obj)
        return obj

    def cli(*cmd):
        return subprocess.check_output(cmd, text=True).strip()

    report = dict(
        kernel=platform.release(),
        arch=platform.machine(),
        kvm=os.path.exists("/dev/kvm"),
        runsc=cli("runsc", "--version"),
        docker=cli("docker", "version", "--format", "{{.Server.Version}}"),
        image_id=cli("docker", "image", "inspect", "--format", "{{.Id}}", args.image),
        runtimes={},
    )
    log = (args.out / "runtime.log").open("w")

    def start():
        proc = subprocess.Popen(
            [
                str(args.server.resolve()),
                "-addr",
                f"127.0.0.1:{port}",
                "-owner",
                owner,
                "-policy",
                str(policy_path),
                "-state-dir",
                str(args.out / ("state-" + owner)),
            ],
            stdout=log,
            stderr=log,
        )
        for _ in range(100):
            if proc.poll() is not None:
                raise RuntimeError("sandboxd exited during startup")
            try:
                if request("GET", "/readyz"):
                    return proc
            except OSError:
                time.sleep(0.05)
        proc.terminate()
        proc.wait(timeout=20)
        raise RuntimeError("sandboxd readiness timed out")

    srv = start()
    try:
        code, refusal = http("POST", "/v1/sandboxes", {**spec, "runtime": "unapproved"})
        assert code == 400, (code, refusal)
        report["unapproved"] = refusal
        default_runtime = cli("docker", "info", "--format", "{{.DefaultRuntime}}")
        report["engine_default"] = default_runtime
        for runtime in [default_runtime] if args.engine_default else ["runc", "runsc"]:
            inputs = spec if args.engine_default else {**spec, "runtime": runtime}
            sb = request("POST", "/v1/sandboxes", inputs)
            base = "/v1/sandboxes/" + sb["id"]
            try:
                assert (
                    (args.engine_default or sb["runtime"] == runtime)
                    and sb["agent_mode"] == "guest"
                    and sb["protected"]
                ), sb
                cid = cli(
                    "docker",
                    "inspect",
                    "--format",
                    "{{.Id}}",
                    "tiny-sandbox-" + sb["id"],
                )
                assert (
                    cli("docker", "inspect", "--format", "{{.HostConfig.Runtime}}", cid)
                    == runtime
                )
                pid = cli("docker", "inspect", "--format", "{{.State.Pid}}", cid)
                cg = next(
                    line[3:]
                    for line in Path("/proc", pid, "cgroup").read_text().splitlines()
                    if line.startswith("0::")
                )
                scope = Path("/sys/fs/cgroup") / cg.lstrip("/")
                assert cid in scope.name, scope
                limits = {
                    f: (scope / f).read_text().strip()
                    for f in ["memory.max", "memory.swap.max", "pids.max", "cpu.max"]
                }
                assert limits == {
                    "memory.max": "268435456",
                    "memory.swap.max": "0",
                    "pids.max": "128",
                    "cpu.max": "50000 100000",
                }, limits
                net = cli("docker", "exec", cid, "cat", "/proc/net/dev")
                assert [
                    line.split(":")[0].strip()
                    for line in net.splitlines()
                    if ":" in line
                ] == ["lo"], net
                entry = request("PUT", base + "/files?path=/tmp/probe", b"guest-ok")
                stat = request("GET", base + "/files/stat?path=/tmp/probe")
                assert entry == stat and entry["size"] == 8, entry
                ex = request(
                    "POST",
                    base + "/executions",
                    {"command": ["cat", "/tmp/probe"], "timeout_ms": 2000},
                )
                deadline = time.monotonic() + 15
                while ex["state"] == "running" and time.monotonic() < deadline:
                    time.sleep(0.03)
                    ex = request("GET", base + "/executions/" + ex["id"])
                assert ex["exit_code"] == 0 and ex["stdout"] == "guest-ok", ex
                assert request("GET", base + "/files?path=/tmp/probe") == b"guest-ok"
                content = b"repeated transfer data" * 32768
                raw = io.BytesIO()
                with tarfile.open(fileobj=raw, mode="w") as tf:
                    entry_header = tarfile.TarInfo("workspace/sample")
                    entry_header.size = len(content)
                    tf.addfile(entry_header, io.BytesIO(content))
                request("PUT", base + "/files/archive?path=/tmp", raw.getvalue())
                downloaded = request("GET", base + "/files/archive?path=/tmp/workspace")
                with tarfile.open(fileobj=io.BytesIO(downloaded)) as tf:
                    assert tf.extractfile("workspace/sample").read() == content
                assert (
                    cli(
                        "docker", "exec", cid, "sha256sum", "/tmp/workspace/sample"
                    ).split()[0]
                    == hashlib.sha256(content).hexdigest()
                )
                cli("docker", "exec", cid, "sh", "-c", "echo changed > /tmp/probe")
                assert request("GET", base + "/files?path=/tmp/probe") == b"changed\n"
                srv.terminate()
                srv.wait(timeout=25)
                srv = start()
                assert request("GET", base)["state"] == "running"
                assert request("GET", base + "/files?path=/tmp/probe") == b"changed\n"
                report["runtimes"][runtime] = dict(
                    requested_runtime=inputs.get("runtime"),
                    restored_filesystem=True,
                    sandbox=sb,
                    cgroup=cg,
                    host_limits=limits,
                    guest_net=net,
                    file=entry,
                    execution=ex,
                    archive_sha256=hashlib.sha256(content).hexdigest(),
                    observed_guest_write=True,
                )
            finally:
                request("DELETE", base)
            (args.out / "report.json").write_text(json.dumps(report, indent=2) + "\n")
    finally:
        srv.terminate()
        srv.wait(timeout=25)
        log.close()
        assert not cli(
            "docker", "ps", "-aq", "--filter", "label=tiny-sandbox.owner=" + owner
        )


if __name__ == "__main__":
    main()
