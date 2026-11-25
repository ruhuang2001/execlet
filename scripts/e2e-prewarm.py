#!/usr/bin/env python3
import argparse
import concurrent.futures
import contextlib
import io
import json
import math
import os
import pathlib
import socket
import statistics
import subprocess
import tarfile
import tempfile
import time
import urllib.error
import urllib.request
import uuid

ROOT = pathlib.Path(__file__).resolve().parents[1]


def docker(*args, check=True):
    return subprocess.run(
        ["docker", *args], check=check, capture_output=True, text=True
    ).stdout.strip()


def until(check, seconds=20):
    end = time.monotonic() + seconds
    while time.monotonic() < end:
        result = check()
        if result:
            return result
        time.sleep(0.05)
    raise TimeoutError(check)


def run(args, tmp, report):
    subprocess.run(
        ["go", "build", "-o", tmp / "execletd", "./cmd/execletd"], cwd=ROOT, check=True
    )
    owner = "prewarm-e2e-" + uuid.uuid4().hex
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        addr = f"127.0.0.1:{sock.getsockname()[1]}"
    url = "http://" + addr
    policy = tmp / "policy.json"
    state = tmp / "state"
    spec = {
        "memory_mb": 64,
        "pids_limit": 128,
        "nano_cpus": 500_000_000,
        "network": "none",
        "runtime": "runc",
    }

    def http(path, method="GET", data=None):
        request = urllib.request.Request(
            url + path,
            method=method,
            headers={"Content-Type": "application/json"},
            data=None if data is None else json.dumps(data).encode(),
        )
        with urllib.request.urlopen(request, timeout=40) as response:
            raw = response.read()
            return (
                json.loads(raw)
                if response.headers.get_content_type() == "application/json"
                else raw.decode()
            )

    def create(image, **extra):
        return http("/v1/sandboxes", "POST", {**spec, "image": image, **extra})["id"]

    def command(sid):
        root = "/v1/sandboxes/" + sid + "/executions"
        ex = http(root, "POST", {"command": ["echo", "ready"]})

        def terminal():
            ex2 = http(root + "/" + ex["id"])
            return ex2 if ex2["state"] == "completed" else None

        ex = until(terminal)
        assert ex["exit_code"] == 0 and ex["stdout"] == "ready\n", ex

    def kill(sid):
        assert http("/v1/sandboxes/" + sid, "DELETE")["state"] == "killed"

    def owned():
        return docker(
            "ps", "-qa", "--no-trunc", "--filter", "label=execlet.owner=" + owner
        ).splitlines()

    def ready():
        delivered = {s["id"] for s in http("/v1/sandboxes")["sandboxes"]}
        rows = docker(
            "ps",
            "--no-trunc",
            "--filter",
            "label=execlet.owner=" + owner,
            "--format",
            '{{.ID}} {{.Label "execlet.id"}}',
        )
        return [
            row.split()[0]
            for row in rows.splitlines()
            if len(row.split()) == 2 and row.split()[1] not in delivered
        ]

    def ready_count():
        for line in http("/metrics").splitlines():
            if line.startswith("execlet_prewarm_ready "):
                return int(line.split()[1])
        raise ValueError("missing prewarm metric")

    def pool():
        return until(lambda: ready() if ready_count() == 1 else None)

    with contextlib.ExitStack() as stack:

        def clean():
            for cid in owned():
                docker("rm", "-f", cid, check=False)

        stack.callback(clean)
        registry = docker("run", "--rm", "-d", "-p", "127.0.0.1::5000", "registry:2")
        stack.callback(docker, "rm", "-f", registry, check=False)
        port = docker("port", registry, "5000/tcp").rsplit(":", 1)[1]
        repo = f"127.0.0.1:{port}/{owner}:bench"
        base = docker("create", args.image, "true")
        stack.callback(docker, "rm", "-f", base, check=False)
        rootfs = tmp / "fixture.tar"
        docker("export", "--output", str(rootfs), base)
        with tarfile.open(rootfs, "a") as archive:
            marker = owner.encode()
            header = tarfile.TarInfo("tmp/" + owner)
            header.size = len(marker)
            archive.addfile(header, io.BytesIO(marker))
        docker("image", "import", "--change", 'CMD ["/bin/sh"]', str(rootfs), repo)
        stack.callback(docker, "image", "rm", repo, check=False)
        docker("push", repo)
        digest = docker("image", "inspect", "--format", "{{.Id}}", repo)
        rules = {
            "allowed_images": [repo],
            "allowed_networks": ["none"],
            "allowed_runtimes": ["runc"],
            "max_sandboxes": 2,
            "total_memory_mb": 256,
            "total_pids": 512,
            "total_nano_cpus": 2_000_000_000,
            "templates": {"bench": digest},
        }
        report["fixture"] = {
            "source_image": args.image,
            "digest": digest,
            "registry": "local loopback",
            "cold_image": "unique single-layer image and repository tag absent locally before each create",
        }

        def start():
            policy.write_text(json.dumps(rules))
            log = stack.enter_context((args.out / "daemon.log").open("a"))
            proc = subprocess.Popen(
                [
                    tmp / "execletd",
                    "-addr",
                    addr,
                    "-owner",
                    owner,
                    "-policy",
                    policy,
                    "-state-dir",
                    state,
                ],
                env={**os.environ, "SANDBOX_API_KEY": ""},
                stdout=log,
                stderr=log,
            )

            def stop():
                if proc.poll() is None:
                    proc.terminate()
                    proc.wait(timeout=20)

            stack.callback(stop)

            def started():
                if proc.poll() is not None:
                    raise RuntimeError("execletd exited; see daemon.log")
                try:
                    return http("/readyz")
                except OSError:
                    return None

            until(started)
            return proc

        for group in ("cold_image", "local_image", "named_template", "prewarm_hit"):
            if group == "cold_image":
                server = start()
            if group == "prewarm_hit":
                server.terminate()
                server.wait(timeout=20)
                rules["prewarm"] = [{**spec, "image": "template:bench", "count": 1}]
                server = start()
            ms = []
            rows = []
            for _ in range(args.samples):
                if group == "cold_image":
                    docker("image", "rm", repo)
                    assert not docker("image", "ls", "-q", repo) and not docker(
                        "image", "inspect", "-f", "{{.Id}}", digest, check=False
                    )
                before = pool() if group == "prewarm_hit" else []
                image = (
                    "template:bench"
                    if group in ("prewarm_hit", "named_template")
                    else repo
                )
                start_ns = time.perf_counter_ns()
                sid = create(image)
                command(sid)
                ms.append((time.perf_counter_ns() - start_ns) / 1e6)
                cid = docker("inspect", "-f", "{{.Id}}", "execlet-" + sid)
                assert not before or cid in before, (cid, before)
                assert (
                    docker(
                        "inspect",
                        "-f",
                        '{{index .Config.Labels "execlet.id"}}',
                        cid,
                    )
                    == sid
                )
                rows.append(
                    {"sandbox": sid, "container": cid, "pooled_before_create": before}
                )
                kill(sid)
            report["latency"][group] = {
                "samples_ms": ms,
                "p50_ms": statistics.median(ms),
                "p95_ms": sorted(ms)[math.ceil(len(ms) * 0.95) - 1],
                "records": rows,
            }
        ready_before = pool()
        live = create("template:bench", memory_mb=80)
        assert (
            docker("inspect", "-f", "{{.Id}}", "execlet-" + live)
            not in ready_before
        )
        try:
            create("template:bench", memory_mb=96)
        except urllib.error.HTTPError as err:
            assert err.code == 429, err.read()
        else:
            raise AssertionError("standby reservation did not count against quota")
        server.kill()
        server.wait(timeout=10)
        server = start()
        recovered = http("/v1/sandboxes/" + live)
        assert recovered["state"] == "running"
        command(live)
        rebuilt = pool()
        assert not set(ready_before).intersection(owned()), (owned(), ready_before)
        kill(live)
        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as workers:
            pair = list(workers.map(create, ["template:bench"] * 2))
        claimed = [
            docker("inspect", "-f", "{{.Id}}", "execlet-" + sid) for sid in pair
        ]
        assert (
            len(set(claimed)) == 2 and len(set(rebuilt).intersection(claimed)) == 1
        ), claimed
        for sid in pair:
            kill(sid)
        original = pool()[0]
        alternate = docker("image", "inspect", "--format", "{{.Id}}", args.image)
        assert alternate != digest

        def publish_templates(templates):
            rules["templates"] = templates
            staged = policy.with_suffix(".tmp")
            staged.write_text(json.dumps(rules))
            staged.replace(policy)

        publish_templates({"bench": alternate, "previous": digest})

        def replacement():
            ids = ready()
            if len(ids) != 1 or ids[0] == original or ready_count() != 1:
                return None
            return (
                ids[0]
                if docker("inspect", "--format", "{{.Image}}", ids[0]) == alternate
                else None
            )

        switched = until(replacement)
        sid = create("template:bench")
        assert (
            docker("inspect", "--format", "{{.Id}}", "execlet-" + sid) == switched
        )
        command(sid)
        kill(sid)
        publish_templates({"previous": digest})
        until(lambda: ready_count() == 0 and not ready())
        server.kill()
        server.wait(timeout=20)
        server = start()
        assert ready_count() == 0
        publish_templates({"bench": digest})
        after_restore = pool()[0]
        report["alias_rollover"] = {
            "old_digest_still_allowed": digest,
            "old_ready": original,
            "new_digest": alternate,
            "claimed_after_switch": switched,
            "ready_while_alias_missing_after_restart": 0,
            "restored_ready": after_restore,
        }
        server.terminate()
        server.wait(timeout=20)
        report["recovery"] = {
            "before_crash": ready_before,
            "after_restart": rebuilt,
            "preserved_active_sandbox": live,
            "quota_rejection_http": 429,
            "concurrent_claims": claimed,
            "after_shutdown": owned(),
        }
        assert not owned()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--samples", type=int, default=10)
    parser.add_argument("--image", default="alpine:3.21")
    parser.add_argument("--out", type=pathlib.Path, default="artifacts/prewarm-e2e")
    args = parser.parse_args()
    if args.samples < 1 or not __debug__:
        parser.error("positive sample count and enabled assertions are required")
    args.out.mkdir(parents=True, exist_ok=True)
    report = {
        "passed": False,
        "latency": {},
        "interval": "create dispatch to first completed echo",
    }
    try:
        with tempfile.TemporaryDirectory(prefix="tiny-prewarm-") as tmp:
            run(args, pathlib.Path(tmp), report)
        report["passed"] = True
    finally:
        (args.out / "acceptance.json").write_text(json.dumps(report, indent=2) + "\n")


if __name__ == "__main__":
    main()
