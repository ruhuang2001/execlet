#!/usr/bin/env python3
import argparse
import contextlib
import datetime
import hashlib
import json
import os
import pathlib
import shutil
import socket
import subprocess
import tempfile
import time
import urllib.request
import uuid

ROOT = pathlib.Path(__file__).resolve().parents[1]
IMAGE = "tiny-sandbox-mcp:local"


def shell(*args, **kwargs):
    return subprocess.run(args, check=True, cwd=ROOT, **kwargs)


def get(url):
    with urllib.request.urlopen(url, timeout=3) as response:
        return json.load(response)


def compact(value):
    if isinstance(value, str) and len(value) > 8192:
        data = value.encode()
        return {
            "bytes": len(data),
            "sha256": hashlib.sha256(data).hexdigest(),
            "tail": value[-80:],
        }
    if isinstance(value, dict):
        return {k: compact(v) for k, v in value.items()}
    if isinstance(value, list):
        return [compact(v) for v in value]
    return value


def run(tmp, evidence, record):
    for binary, pkg in (
        ("sandboxd", "./cmd/sandboxd"),
        ("sandboxctl", "./cmd/sandboxctl"),
        ("client", "./cmd/sandboxctl/testdata/mcp-client"),
    ):
        shell("go", "build", "-o", tmp / binary, pkg)
    shell("docker", "build", "-t", IMAGE, "examples/mcp")
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        addr = f"127.0.0.1:{sock.getsockname()[1]}"
    domain = "http://" + addr
    owner = "mcp-e2e-" + str(uuid.uuid4())
    work = tmp / "workspace"
    shell("git", "clone", "--quiet", "--local", "--no-checkout", str(ROOT), str(work))
    shell("git", "-C", str(work), "checkout", "HEAD", "--", "sandbox-python")
    revision = shell(
        "git", "-C", str(work), "rev-parse", "HEAD", capture_output=True, text=True
    ).stdout.strip()
    record["repository"] = {
        "commit": revision,
        "project": "tiny-sandbox/sandbox-python",
    }
    (work / ".gitignore").write_text("*.ignored\n**/private.bin\n")
    for name in ("private.bin", "ignored/nested/private.bin", "ignored/private.bin"):
        f = work / name
        f.parent.mkdir(parents=True, exist_ok=True)
        f.write_text("ignored")

    (work / ".env").write_text("excluded")
    (work / "sample.ignored").write_text("excluded")
    (work / "outside-link").symlink_to(tmp / "sandboxctl")
    pkg = work / "sandbox-python"
    (pkg / "remove me").write_text("old")
    source = pkg / "src/tiny_sandbox/e2b.py"
    correct = source.read_text()
    regression = correct.replace("round(value * 1000)", "round(value * 100)", 1)
    assert regression != correct
    source.write_text(regression)
    cache_home = tmp / "client-home"
    cache_home.mkdir()
    env = dict(os.environ, SANDBOX_DOMAIN=domain, SANDBOX_API_KEY="")
    with contextlib.ExitStack() as stack:
        log = stack.enter_context((evidence / "daemon.log").open("w"))
        daemon = subprocess.Popen(
            [tmp / "sandboxd", "-addr", addr, "-owner", owner],
            env=env,
            stdout=log,
            stderr=log,
        )
        stack.callback(daemon.wait)
        stack.callback(daemon.terminate)
        for _ in range(150):
            try:
                with urllib.request.urlopen(domain + "/healthz", timeout=0.5):
                    break
            except OSError:
                if daemon.poll() is not None:
                    raise RuntimeError("sandboxd exited; see daemon.log")
                time.sleep(0.1)
        else:
            raise RuntimeError("sandboxd did not start")

        def connect(workspace=work, extra=(), endpoint=domain, ttl=90):
            proc = subprocess.Popen(
                [
                    tmp / "client",
                    tmp / "sandboxctl",
                    "mcp",
                    "--workspace",
                    workspace,
                    "--image",
                    IMAGE,
                    *(["--sandbox-ttl", str(ttl)] if ttl else []),
                    *extra,
                ],
                text=True,
                stdin=subprocess.PIPE,
                stdout=subprocess.PIPE,
                env={**env, "SANDBOX_DOMAIN": endpoint, "HOME": str(cache_home)},
            )
            stack.callback(proc.wait)
            stack.callback(proc.stdin.close)
            tools = json.loads(proc.stdout.readline())["tools"]
            return proc, tools

        def disconnect(proc):
            proc.stdin.close()
            assert proc.wait(timeout=10) == 0

        for missing_args in (["--image", IMAGE], ["--workspace", str(work)]):
            invalid = subprocess.run(
                [tmp / "sandboxctl", "mcp", *missing_args],
                capture_output=True,
                timeout=10,
            )
            assert invalid.returncode == 2 and not invalid.stdout
        proc, tools = connect(ttl=None)
        record["tools"] = [tool["name"] for tool in tools]
        assert len(tools) == 8
        for tool in tools:
            if tool["name"] != "create_sandbox":
                assert "sandbox_id" in tool["inputSchema"]["required"], tool

        def call(name, error=False, **args):
            request = {"name": name, "arguments": args}
            proc.stdin.write(json.dumps(request) + "\n")
            proc.stdin.flush()
            response = json.loads(proc.stdout.readline())
            record["calls"].append(
                {"request": compact(request), "response": compact(response)}
            )
            assert bool(response.get("isError")) == error, (name, response)
            if error:
                fault = response["structuredContent"]
                assert (
                    fault["code"] and fault["message"] in response["content"][0]["text"]
                )
            return (
                response["content"][0]["text"]
                if error
                else response["structuredContent"]
            )

        assert "policy_denied" in call("create_sandbox", error=True, network="bridge")
        created = call("create_sandbox")
        sb = created["sandbox_id"]
        assert created["network"] == "none" and created["protected"]
        info = get(domain + "/v1/sandboxes/" + sb)
        assert info["image"] == IMAGE and info["timeout_seconds"] == 1800
        record["creation_defaults"] = {
            "ttl_seconds": info["timeout_seconds"],
            "image": info["image"],
            "network": info["network"],
        }

        def tool(name, **args):
            return call(name, sandbox_id=sb, **args)

        initial = tool("sync_workspace")
        assert initial["uploaded"] >= 10, initial
        cmd = [
            "env",
            "PYTHONPATH=sandbox-python/src",
            "python3",
            "-B",
            "-m",
            "pytest",
            "-p",
            "no:cacheprovider",
            "-q",
            "sandbox-python/tests/test_background_commands.py::BackgroundCommandsTests::test_pinned_duration_conversion_via_public_api",
        ]
        failed = tool("run_command", command=cmd, idempotency_key="mcp-failing-test")
        assert failed["exit_code"] == 1 and "1 failed" in failed["stdout"], failed
        replay = tool("run_command", command=cmd, idempotency_key="mcp-failing-test")
        assert replay["execution_id"] == failed["execution_id"]
        disconnect(proc)
        before = get(domain + "/v1/sandboxes/" + sb)["expires_at"]
        other = tmp / "other-workspace"
        other.mkdir()
        for workspace, extra in (
            (other, ()),
            (work, ("--allow-network", "bridge")),
            (work, ("--image", "another-image")),
        ):
            proc, _ = connect(workspace, extra)
            msg = call("sync_workspace", error=True, sandbox_id=sb)
            assert "sandbox_not_running" in msg, msg
            if "--allow-network" in extra:
                bridge = call("create_sandbox", network="bridge")
                assert bridge["network"] == "bridge"
                call("destroy_sandbox", sandbox_id=bridge["sandbox_id"])
            disconnect(proc)
        assert get(domain + "/v1/sandboxes/" + sb)["expires_at"] == before
        proc, _ = connect(work, endpoint=domain + "/")
        resumed = tool("sync_workspace")
        assert resumed["uploaded"] == 0
        record["reconnection"] = {
            "sandbox_id": sb,
            "unchanged_uploaded": 0,
            "workspace_image_network_mismatches_rejected": True,
        }
        source.write_text(correct)
        assert tool("sync_workspace")["uploaded"] == 1
        passed = tool("run_command", command=cmd)
        assert passed["exit_code"] == 0 and "1 passed" in passed["stdout"], passed
        archive_limit_cases(work, sb, call, tool, record)
        (pkg / "remove me").unlink()
        assert tool("sync_workspace")["deleted"] == 1
        gone = tool(
            "run_command",
            command=[
                "python3",
                "-c",
                "from pathlib import Path; assert not any(Path(p).exists() for p in "
                "['.git', '.env', 'sample.ignored', 'private.bin', 'ignored/private.bin', 'ignored/nested/private.bin', 'outside-link', 'sandbox-python/remove me'])",
            ],
        )
        assert gone["exit_code"] == 0, gone
        assert tool("write_file", path="/tmp/note", content="hello")["type"] == "file"
        assert tool("read_file", path="/tmp/note")["content"] == "hello"
        manifest = cache_home / ".cache/tiny-sandbox/mcp" / (sb + ".json")
        assert tool("run_command", command=["touch", "guest-only"])["exit_code"] == 0
        manifest.write_text("{interrupted")
        assert tool("sync_workspace", reset=True)["uploaded"] == initial["uploaded"] - 1
        assert (
            tool("run_command", command=["test", "!", "-e", "guest-only"])["exit_code"]
            == 0
        )
        mixed = tool(
            "run_command",
            command=["python3", "-c", "print('a'*65535+'界'+'b'*2000,end='')"],
        )
        unicode_page = tool(
            "get_execution", execution_id=mixed["execution_id"], offset=0
        )
        remaining = tool(
            "get_execution",
            execution_id=mixed["execution_id"],
            offset=unicode_page["next_offset"],
        )
        assert len(unicode_page["stdout"].encode()) <= 65536
        assert (
            unicode_page["stdout"] + remaining["stdout"]
            == "a" * 65535 + "界" + "b" * 2000
        )
        binary = tool(
            "run_command",
            command=[
                "python3",
                "-c",
                "open('/tmp/mcp-binary','wb').write(bytes([255]))",
            ],
        )
        assert binary["exit_code"] == 0
        assert "payload_too_large" in call(
            "read_file", error=True, sandbox_id=sb, path="/tmp/mcp-binary"
        )
        large = tool(
            "run_command", command=["python3", "-c", "print('a' * 70000 + 'THE END')"]
        )
        assert large["output_truncated"] and large["stdout"].endswith("THE END\n")
        first = tool(
            "get_execution",
            execution_id=large["execution_id"],
            offset=large["next_offset"],
        )
        last = tool(
            "get_execution",
            execution_id=large["execution_id"],
            offset=first["next_offset"],
        )
        assert first["stdout"] + last["stdout"] == "a" * 70000 + "THE END\n"
        for stream in ("stdout", "stderr"):
            end_marker = "終わり-" + stream
            oversized = tool(
                "run_command",
                command=[
                    "python3",
                    "-c",
                    "import sys; sys."
                    + stream
                    + ".write('x'*1200000 + "
                    + repr(end_marker)
                    + ")",
                ],
            )
            assert oversized["exit_code"] == 0 and oversized["output_truncated"]
            assert oversized[stream].endswith(end_marker), (
                stream,
                oversized[stream][-30:],
            )
            assert len(oversized[stream].encode()) <= 65536
            observed = tool("get_execution", execution_id=oversized["execution_id"])
            assert observed[stream] == oversized[stream]
        record["true_output_tail"] = {
            "bytes_per_stream": 1200000,
            "terminal_markers": ["stdout", "stderr"],
        }
        started = time.monotonic()
        live = tool(
            "run_command",
            command=[
                "python3",
                "-uc",
                "import pathlib,sys,time;print('started');"
                "exec('while not pathlib.Path(\"/tmp/output-go\").exists(): time.sleep(.05)');"
                "print('a'*1200000+'first');"
                "exec('while not pathlib.Path(\"/tmp/output-more\").exists(): time.sleep(.05)');"
                "print('b'*1250000+'second');time.sleep(90)",
            ],
        )
        assert live["state"] == "running" and time.monotonic() - started < 70, live
        polled = tool("get_execution", execution_id=live["execution_id"], offset=0)
        assert polled["state"] == "running" and "started" in polled["stdout"]
        tool("write_file", path="/tmp/output-go", content="go")

        def snapshot(marker):
            for _ in range(100):
                value = get(
                    domain
                    + "/v1/sandboxes/"
                    + sb
                    + "/executions/"
                    + live["execution_id"]
                )
                if value["stdout"].endswith(marker + "\n"):
                    return value
                time.sleep(0.02)
            raise AssertionError(marker)

        snap = snapshot("first")
        first = tool("get_execution", execution_id=live["execution_id"], offset=0)
        assert (
            first["output_lost"]
            and first["stdout_start_offset"] == snap["stdout_offset"] > 0
        )
        cursor = first["next_offset"]
        tool("write_file", path="/tmp/output-more", content="go")
        snap = snapshot("second")
        resumed = tool(
            "get_execution", execution_id=live["execution_id"], offset=cursor
        )
        assert (
            resumed["output_lost"]
            and resumed["stdout_start_offset"] == snap["stdout_offset"] > cursor
        )
        assert (
            resumed["stdout"] == snap["stdout"][:65536]
            and resumed["next_offset"] == snap["stdout_offset"] + 65536
        )
        terminal = tool("cancel_execution", execution_id=live["execution_id"])
        assert (
            terminal["state"] == "completed"
            and terminal["termination_reason"] == "canceled"
        )
        before = get(domain + "/v1/sandboxes/" + sb)["expires_at"]
        time.sleep(1.1)
        tool("read_file", path="/tmp/note")
        after = get(domain + "/v1/sandboxes/" + sb)["expires_at"]
        assert after[:19] > before[:19]
        record["renewal"] = {"before": before, "after": after}
        with urllib.request.urlopen(
            urllib.request.Request(
                domain + "/v1/sandboxes/" + sb + "/pause", method="POST"
            )
        ) as response:
            assert json.load(response)["state"] == "paused"
        assert tool("destroy_sandbox")["state"] == "killed"
        records = get(domain + "/v1/sandboxes")["sandboxes"]
        containers = shell(
            "docker",
            "ps",
            "-aq",
            "--filter",
            "label=tiny-sandbox.owner=" + owner,
            capture_output=True,
            text=True,
        ).stdout.splitlines()
        manifest = cache_home / ".cache/tiny-sandbox/mcp" / (sb + ".json")
        record["cleanup"] = {
            "active_ids": [row["id"] for row in records if row["state"] != "killed"],
            "records": [(row["id"], row["state"]) for row in records],
            "container_ids": containers,
            "manifest_exists": manifest.exists(),
        }
        assert (
            all(row["id"] != sb for row in records)
            and not containers
            and not manifest.exists()
        )
        assert not manifest.with_suffix(".owner").exists()
        disconnect(proc)
        fixtures = (
            (
                "github.com/google/jsonschema-go",
                "v0.4.3",
                "8c4ab4f02ef64dcea5502e47a6113e8292944087",
            ),
            (
                "github.com/modelcontextprotocol/go-sdk",
                "v1.8.0",
                "eeddd84db4054984ba4d2d2504851a5001182d53",
            ),
        )
        for module, version, revision in fixtures:
            repo = tmp / module.rsplit("/", 1)[1]
            shell("git", "init", "-q", str(repo))
            shell(
                "git",
                "-C",
                str(repo),
                "fetch",
                "-q",
                "--depth=1",
                "https://" + module + ".git",
                revision,
            )
            shell("git", "-C", str(repo), "checkout", "-q", "--detach", "FETCH_HEAD")
            commit = shell(
                "git",
                "-C",
                str(repo),
                "rev-parse",
                "HEAD",
                capture_output=True,
                text=True,
            ).stdout.strip()
            proc, _ = connect(repo)
            sid = call("create_sandbox")["sandbox_id"]
            upload = call("sync_workspace", sandbox_id=sid)
            unchanged = call("sync_workspace", sandbox_id=sid)
            checksum = hashlib.sha256((repo / "go.mod").read_bytes()).hexdigest()
            check = call("run_command", sandbox_id=sid, command=["sha256sum", "go.mod"])
            assert check["exit_code"] == 0 and check["stdout"].split()[0] == checksum
            assert upload["uploaded"] > 20 and unchanged["uploaded"] == 0
            record.setdefault("upstream_sync", []).append(
                {
                    "repository": module,
                    "version": version,
                    "commit": commit,
                    "upload": upload,
                    "go_mod_sha256": checksum,
                }
            )
            call("destroy_sandbox", sandbox_id=sid)
            disconnect(proc)
        proc, _ = connect()
        stale = call("create_sandbox")["sandbox_id"]
        call("sync_workspace", sandbox_id=stale)
        with urllib.request.urlopen(
            urllib.request.Request(domain + "/v1/sandboxes/" + stale, method="DELETE")
        ) as response:
            assert response.status == 200
        assert "sandbox_not_running" in call(
            "sync_workspace", sandbox_id=stale, error=True
        )
        assert not (manifest.parent / (stale + ".owner")).exists()
        assert not (manifest.parent / (stale + ".json")).exists()
        disconnect(proc)
        plain = tmp / "no-git"
        (plain / "sub" / "deep").mkdir(parents=True)
        (plain / ".gitignore").write_text("**/private.bin\n")
        for name in ["private.bin", "sub/private.bin", "sub/deep/private.bin", "keep"]:
            (plain / name).touch()
        proc, _ = connect(plain, ttl=1)
        expired = call("create_sandbox")["sandbox_id"]
        upload = call("sync_workspace", sandbox_id=expired)
        assert upload["uploaded"] == 2, upload
        disconnect(proc)
        until = time.monotonic() + 10
        while time.monotonic() < until:
            if get(domain + "/v1/sandboxes/" + expired)["state"] == "killed":
                break
            time.sleep(0.1)
        proc, _ = connect(work)
        until = time.monotonic() + 5
        owner_path = manifest.parent / (expired + ".owner")
        while owner_path.exists() and time.monotonic() < until:
            time.sleep(0.05)
        assert (
            not owner_path.exists()
            and not (manifest.parent / (expired + ".json")).exists()
        )
        record["expired_cache_collected_after_restart"] = True
        disconnect(proc)
        proc, _ = connect(ttl=5)
        ttl_sandbox = call("create_sandbox")
        ttl_id = ttl_sandbox["sandbox_id"]
        original_deadline = datetime.datetime.fromisoformat(
            ttl_sandbox["expires_at"].removesuffix("Z")[:26] + "+00:00"
        ).timestamp()
        time.sleep(2.1)
        call(
            "write_file",
            sandbox_id=ttl_id,
            path="/tmp/renewal-evidence",
            content="alive",
        )
        new_deadline = get(domain + "/v1/sandboxes/" + ttl_id)["expires_at"]
        time.sleep(max(0, original_deadline - time.time() + 0.25))
        assert get(domain + "/v1/sandboxes/" + ttl_id)["state"] == "running"
        assert (
            call("read_file", sandbox_id=ttl_id, path="/tmp/renewal-evidence")[
                "content"
            ]
            == "alive"
        )
        record["survived_original_deadline"] = {
            "initial": ttl_sandbox["expires_at"],
            "renewed": new_deadline,
        }
        call("destroy_sandbox", sandbox_id=ttl_id)
        disconnect(proc)
        proc, _ = connect()
        offline_id = call("create_sandbox")["sandbox_id"]
        with urllib.request.urlopen(
            urllib.request.Request(
                domain + "/v1/sandboxes/" + offline_id, method="DELETE"
            )
        ) as response:
            assert response.status == 200
        daemon.terminate()
        daemon.wait(timeout=15)
        inputs = {
            "sync_workspace": {},
            "read_file": {"path": "/tmp/a"},
            "write_file": {"path": "/tmp/a", "content": "x"},
            "run_command": {"command": ["true"]},
            "get_execution": {"execution_id": "exe_missing"},
            "cancel_execution": {"execution_id": "exe_missing"},
            "destroy_sandbox": {},
        }
        for name, payload in inputs.items():
            msg = call(name, error=True, sandbox_id=offline_id, **payload)
            assert "daemon_unreachable" in msg and "sandboxctl doctor" in msg, (
                name,
                msg,
            )
        msg = call("create_sandbox", error=True)
        assert "daemon_unreachable" in msg and "sandboxctl doctor" in msg
        (manifest.parent / (offline_id + ".owner")).unlink(missing_ok=True)
        policy = tmp / "bridge-only.json"
        policy.write_text(
            json.dumps(
                {
                    "max_sandboxes": 2,
                    "total_memory_mb": 2048,
                    "total_pids": 2048,
                    "total_nano_cpus": 2000000000,
                    "allowed_images": [IMAGE],
                    "allowed_networks": ["bridge"],
                }
            )
        )
        replacement = subprocess.Popen(
            [tmp / "sandboxd", "-addr", addr, "-owner", owner, "-policy", policy],
            env=env,
            stdout=log,
            stderr=log,
        )
        stack.callback(replacement.wait)
        stack.callback(replacement.terminate)
        for _ in range(150):
            try:
                with urllib.request.urlopen(domain + "/healthz", timeout=0.5):
                    break
            except OSError:
                time.sleep(0.1)
        msg = call("create_sandbox", error=True)
        assert "invalid_request" in msg and "network none" in msg, msg
        assert not get(domain + "/v1/sandboxes")["sandboxes"]
        record["offline_policy_denial"] = msg


def archive_limit_cases(work, sid, call, tool, record):
    limits = work / "limits"
    limits.mkdir()
    oversized = limits / "payload"
    batch = limits / "batch"
    batch.mkdir()
    try:
        (work / "sandbox-python/remove me").unlink()
        with oversized.open("wb") as f:
            f.truncate((16 << 20) + 1)
        msg = call("sync_workspace", error=True, sandbox_id=sid)
        assert "payload_too_large" in msg and "16 MiB" in msg, msg
        assert (
            tool("run_command", command=["test", "-f", "sandbox-python/remove me"])[
                "exit_code"
            ]
            == 0
        )
        oversized.unlink()
        for n in range(4097):
            (batch / str(n)).touch()
        result = tool("sync_workspace")
        assert result["uploaded"] == 4097 and result["deleted"] == 1, result
        assert (
            tool(
                "run_command",
                command=[
                    "sh",
                    "-c",
                    "test -f limits/batch/4096 && echo edited >> limits/batch/0",
                ],
            )["exit_code"]
            == 0
        )
        (work / "sandbox-python/remove me").write_text("old")
        shutil.rmtree(batch)
        batch.mkdir()
        for n in range(108):
            (batch / f"item {n}").touch()
        assert tool("sync_workspace")["uploaded"] == 109
        shutil.rmtree(batch)
        assert tool("sync_workspace")["deleted"] == 108
        record["archive_limits"] = {
            "extracted_bytes_rejected": (16 << 20) + 1,
            "members_uploaded": 4097,
            "deleted_in_batches": 108,
        }
    finally:
        shutil.rmtree(limits)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "artifacts", nargs="?", default="artifacts/mcp-e2e", type=pathlib.Path
    )
    args = parser.parse_args()
    args.artifacts.mkdir(parents=True, exist_ok=True)
    record = {
        "transport": "official MCP Go SDK CommandTransport",
        "calls": [],
        "passed": False,
    }
    try:
        with tempfile.TemporaryDirectory(prefix="mcp-e2e-") as tmp:
            run(pathlib.Path(tmp), args.artifacts, record)
        record["passed"] = True
    finally:
        output = args.artifacts / "acceptance.json"
        output.write_text(json.dumps(record, indent=2) + "\n")
        print(output)


if __name__ == "__main__":
    main()
