#!/usr/bin/env python3
import argparse
import contextlib
import hashlib
import json
import os
import pathlib
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
        return {"bytes": len(data), "sha256": hashlib.sha256(data).hexdigest(), "tail": value[-80:]}
    if isinstance(value, dict):
        return {k: compact(v) for k, v in value.items()}
    if isinstance(value, list):
        return [compact(v) for v in value]
    return value


def run(tmp, evidence, record):
    for binary, pkg in (("sandboxd", "./cmd/sandboxd"), ("sandboxctl", "./cmd/sandboxctl"),
                        ("client", "./cmd/sandboxctl/testdata/mcp-client")):
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
    revision = shell("git", "-C", str(work), "rev-parse", "HEAD", capture_output=True, text=True).stdout.strip()
    record["repository"] = {"commit": revision, "project": "tiny-sandbox/sandbox-python"}
    (work / ".gitignore").write_text("*.ignored\n")
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
    env = dict(os.environ, SANDBOX_DOMAIN=domain, SANDBOX_API_KEY="")
    with contextlib.ExitStack() as stack:
        log = stack.enter_context((evidence / "daemon.log").open("w"))
        daemon = subprocess.Popen([tmp / "sandboxd", "-addr", addr, "-owner", owner], env=env, stdout=log, stderr=log)
        stack.callback(daemon.wait)
        stack.callback(daemon.terminate)
        for _ in range(150):
            try:
                with urllib.request.urlopen(domain + "/healthz", timeout=.5):
                    break
            except OSError:
                if daemon.poll() is not None:
                    raise RuntimeError("sandboxd exited; see daemon.log")
                time.sleep(.1)
        else:
            raise RuntimeError("sandboxd did not start")
        proc = subprocess.Popen([tmp / "client", tmp / "sandboxctl", "mcp", "--workspace", work,
                                 "--image", IMAGE, "--sandbox-ttl", "90"],
                                text=True, stdin=subprocess.PIPE, stdout=subprocess.PIPE, env=env)
        stack.callback(proc.wait)
        stack.callback(proc.stdin.close)
        tools = json.loads(proc.stdout.readline())["tools"]
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
            record["calls"].append({"request": compact(request), "response": compact(response)})
            assert bool(response.get("isError")) == error, (name, response)
            return response["content"][0]["text"] if error else response["structuredContent"]

        assert "policy_denied" in call("create_sandbox", error=True, network="bridge")
        created = call("create_sandbox")
        sb = created["sandbox_id"]
        assert created["network"] == "none" and isinstance(created["protected"], bool)

        def tool(name, **args):
            return call(name, sandbox_id=sb, **args)

        initial = tool("sync_workspace")
        assert initial["uploaded"] >= 10, initial
        cmd = ["env", "PYTHONPATH=sandbox-python/src", "python3", "-B", "-m", "pytest", "-p", "no:cacheprovider",
               "-q", "sandbox-python/tests/test_background_commands.py::BackgroundCommandsTests::test_pinned_duration_conversion_via_public_api"]
        failed = tool("run_command", command=cmd, idempotency_key="mcp-failing-test")
        assert failed["exit_code"] == 1 and "1 failed" in failed["stdout"], failed
        replay = tool("run_command", command=cmd, idempotency_key="mcp-failing-test")
        assert replay["execution_id"] == failed["execution_id"]
        source.write_text(correct)
        assert tool("sync_workspace")["uploaded"] == 1
        passed = tool("run_command", command=cmd)
        assert passed["exit_code"] == 0 and "1 passed" in passed["stdout"], passed
        (pkg / "remove me").unlink()
        assert tool("sync_workspace")["deleted"] == 1
        gone = tool("run_command", command=["python3", "-c",
                    "from pathlib import Path; assert not any(Path(p).exists() for p in "
                    "['.git', '.env', 'sample.ignored', 'outside-link', 'sandbox-python/remove me'])"])
        assert gone["exit_code"] == 0, gone
        assert tool("write_file", path="/tmp/note", content="hello")["type"] == "file"
        assert tool("read_file", path="/tmp/note")["content"] == "hello"
        assert tool("sync_workspace", reset=True)["uploaded"] == initial["uploaded"] - 1
        large = tool("run_command", command=["python3", "-c", "print('a' * 70000 + 'THE END')"])
        assert large["output_truncated"] and large["stdout"].endswith("THE END\n")
        first = tool("get_execution", execution_id=large["execution_id"], offset=large["next_offset"])
        last = tool("get_execution", execution_id=large["execution_id"], offset=first["next_offset"])
        assert first["stdout"] + last["stdout"] == "a" * 70000 + "THE END\n"
        started = time.monotonic()
        live = tool("run_command", command=["sh", "-c", "echo started; exec sleep 85"])
        assert live["state"] == "running" and time.monotonic() - started < 70, live
        polled = tool("get_execution", execution_id=live["execution_id"], offset=0)
        assert polled["state"] == "running" and "started" in polled["stdout"]
        terminal = tool("cancel_execution", execution_id=live["execution_id"])
        assert terminal["state"] == "completed" and terminal["termination_reason"] == "canceled"
        before = get(domain + "/v1/sandboxes/" + sb)["expires_at"]
        time.sleep(1.1)
        tool("read_file", path="/tmp/note")
        after = get(domain + "/v1/sandboxes/" + sb)["expires_at"]
        assert after[:19] > before[:19]
        record["renewal"] = {"before": before, "after": after}
        assert tool("destroy_sandbox")["state"] == "killed"
        records = get(domain + "/v1/sandboxes")["sandboxes"]
        containers = shell("docker", "ps", "-aq", "--filter", "label=tiny-sandbox.owner=" + owner,
                           capture_output=True, text=True).stdout.splitlines()
        manifest = pathlib.Path.home() / ".cache/tiny-sandbox/mcp" / (sb + ".json")
        record["cleanup"] = {"active_ids": [row["id"] for row in records if row["state"] != "killed"],
                             "records": [(row["id"], row["state"]) for row in records],
                             "container_ids": containers, "manifest_exists": manifest.exists()}
        assert sb not in record["cleanup"]["active_ids"] and not containers and not manifest.exists()
        daemon.terminate()
        daemon.wait(timeout=15)
        assert "daemon_unreachable" in call("create_sandbox", error=True)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("artifacts", nargs="?", default="artifacts/mcp-e2e", type=pathlib.Path)
    args = parser.parse_args()
    args.artifacts.mkdir(parents=True, exist_ok=True)
    record = {"transport": "official MCP Go SDK CommandTransport", "calls": [], "passed": False}
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
