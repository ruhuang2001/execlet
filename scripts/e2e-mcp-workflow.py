#!/usr/bin/env python3
import argparse
import json
import os
import pathlib
import platform
import select
import shutil
import signal
import socket
import statistics
import subprocess
import sys
import tempfile
import time
import urllib.request
import uuid

ROOT = pathlib.Path(__file__).resolve().parents[1]
REVISION = "8c4ab4f02ef64dcea5502e47a6113e8292944087"
HANDSHAKE_TIMEOUT = 15
CALL_TIMEOUT = 90
CLEANUP_TIMEOUT = 10


def command(*args, **kwargs):
    return subprocess.run(args, check=True, timeout=300, **kwargs)


def run(args, tmp):
    release = tmp / "release"
    release.mkdir()
    command("tar", "-xzf", args.archive, "-C", release)
    cli = release / "sandboxctl"
    env = dict(os.environ, HOME=str(tmp), SANDBOX_API_KEY="")
    if not env.get("DOCKER_HOST"):
        env["DOCKER_HOST"] = command(
            "docker",
            "context",
            "inspect",
            "--format",
            "{{.Endpoints.docker.Host}}",
            capture_output=True,
            text=True,
        ).stdout.strip()
    go = args.go
    if go:
        work = tmp / "jsonschema-go"
        command("git", "init", "-q", work)
        command(
            "git",
            "-C",
            work,
            "fetch",
            "-q",
            "--depth=1",
            "https://github.com/google/jsonschema-go.git",
            REVISION,
        )
        command("git", "-C", work, "checkout", "-q", "--detach", REVISION)
        context = tmp / "deps"
        context.mkdir()
        for name in ("go.mod", "go.sum"):
            shutil.copy(work / name, context)
        dockerfile = ROOT / "examples/mcp/go/Dockerfile"
        source = work / "jsonschema/resolve.go"
        from_text, to_text = "do not form a tree", "unexpected schema tree"
        test = [
            "go",
            "test",
            "-p=1",
            "-count=1",
            "-run",
            "^TestSchemaStructure$",
            "./jsonschema",
        ]
        failure_text = "unexpected schema tree"
    else:
        work = release / "examples/mcp/demo"
        dockerfile = work.parent / "Dockerfile"
        context = work.parent
        source = work / "calculator.py"
        from_text, to_text = "value * 2", "value * 3"
        test = ["python3", "-B", "-m", "pytest", "-q", "-p", "no:cacheprovider"]
        failure_text = "assert 21 == 14"

    image = command(
        "docker",
        "build",
        "-q",
        "-f",
        dockerfile,
        context,
        capture_output=True,
        text=True,
    ).stdout.strip()
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        address = f"127.0.0.1:{sock.getsockname()[1]}"
    domain = "http://" + address
    env["SANDBOX_DOMAIN"] = domain
    if not go:
        assert_doctor_fails(env, "daemon:", cli, "doctor", "mcp", "--workspace", work)
    owner = "mcp-workflow-" + uuid.uuid4().hex
    with args.out.with_suffix(".log").open("w") as log, subprocess.Popen(
        [release / "sandboxd", "-addr", address, "-owner", owner],
        env=env,
        stdout=log,
        stderr=log,
    ) as daemon:
        try:
            for _ in range(100):
                try:
                    urllib.request.urlopen(domain + "/readyz", timeout=0.5).close()
                    break
                except OSError:
                    if daemon.poll() is not None:
                        raise RuntimeError("sandboxd exited; see daemon log")
                    time.sleep(0.1)
            else:
                raise RuntimeError("sandboxd did not become ready")

            doctor = [cli, "doctor", "mcp", "--workspace", work, "--image", image]
            doctor += ["--profile", "go", "--check-deps"] if go else ["--collect"]
            config = json.loads(command(*doctor, env=env, capture_output=True).stdout)
            server = config["mcpServers"]["tiny-sandbox"]
            assert server["args"] == ["mcp", "--workspace", str(work), "--image", image]
            assert server["env"] == {"SANDBOX_DOMAIN": domain}
            assert pathlib.Path(server["command"]).samefile(cli)
            if go:
                dependency = work / "jsonschema/doctor_dependency_test.go"
                dependency.write_text('package jsonschema_test\nimport _ "example.invalid/missing"\n')
                assert_doctor_fails(env, "example.invalid/missing", *doctor)
                command(*doctor[:-1], env=env, capture_output=True)
                dependency.unlink()
            else:
                assert_doctor_fails(env, "install the Go toolchain", *doctor[:-1], "--profile", "go")
                assert_doctor_fails(
                    env,
                    "workspace:",
                    cli,
                    "doctor",
                    "mcp",
                    "--workspace",
                    tmp / "missing",
                )
                assert_doctor_fails(
                    env,
                    "missing from the local Docker context",
                    cli,
                    "doctor",
                    "mcp",
                    "--workspace",
                    work,
                    "--image",
                    "127.0.0.1:1/tiny-sandbox-missing:local",
                )
                dependency = work / "test_missing_dependency.py"
                dependency.write_text("import demo_missing_dependency\n")
                assert_doctor_fails(env, "demo_missing_dependency", *doctor)
                dependency.unlink()

            original = source.read_text()
            broken = original.replace(from_text, to_text) if go else original
            fixed = original if go else original.replace(to_text, from_text)
            assert broken != fixed
            runs = []
            for _ in range(args.runs):
                source.write_text(broken)
                runs.append(
                    workflow(
                        args, server, env, owner, source, fixed, test, failure_text
                    )
                )
            with urllib.request.urlopen(
                domain + "/v1/sandboxes", timeout=3
            ) as response:
                assert not json.load(response)["sandboxes"]
            assert not command(
                "docker",
                "ps",
                "-aq",
                "--filter",
                f"label=tiny-sandbox.owner={owner}",
                capture_output=True,
            ).stdout.strip()
        finally:
            failing = sys.exc_info()[0] is not None
            daemon.terminate()
            try:
                daemon.wait(timeout=30)
            except subprocess.TimeoutExpired:
                daemon.kill()
                daemon.wait(timeout=CLEANUP_TIMEOUT)
                if not failing:
                    raise
                print("sandboxd did not stop within 30 seconds", file=sys.stderr)
            finally:
                if failing:
                    try:
                        remaining = subprocess.check_output(
                            [
                                "docker",
                                "ps",
                                "-aq",
                                "--filter",
                                f"label=tiny-sandbox.owner={owner}",
                            ],
                            timeout=CLEANUP_TIMEOUT,
                            text=True,
                        ).split()
                        if remaining:
                            subprocess.run(
                                ["docker", "rm", "-f", *remaining],
                                check=True,
                                timeout=CLEANUP_TIMEOUT,
                            )
                    except (OSError, subprocess.SubprocessError) as err:
                        print(
                            f"cleanup for Docker owner {owner} failed: {err}",
                            file=sys.stderr,
                        )
    return {
        "workload": REVISION if go else "packaged pytest demo",
        "network": "none",
        "cleanup_verified": True,
        "platform": platform.platform(),
        "archive": args.archive.name,
        "image": image,
        "runs": runs,
        "median_create_ms": statistics.median(r["create_ms"] for r in runs),
        "median_ms": {
            phase: {
                key: statistics.median(r[phase][key] for r in runs)
                for key in runs[0][phase]
            }
            for phase in ("initial", "unchanged", "changed")
        },
    }


def assert_doctor_fails(env, diagnostic, *argv):
    result = subprocess.run(argv, env=env, capture_output=True, text=True, timeout=120)
    assert (
        result.returncode and not result.stdout and diagnostic in result.stderr
    ), result


def read_response(pipe, operation, timeout):
    deadline = time.monotonic() + timeout
    data = bytearray()
    while not data.endswith(b"\n"):
        remaining = deadline - time.monotonic()
        if remaining <= 0 or not select.select([pipe], [], [], remaining)[0]:
            raise TimeoutError(
                f"MCP {operation}: no complete response within {timeout}s"
            )
        chunk = os.read(pipe.fileno(), 65536)
        if not chunk:
            raise EOFError(
                f"MCP {operation}: client closed stdout before a complete response"
            )
        data.extend(chunk)
    return json.loads(data)


def workflow(args, server, env, owner, source, fixed, test, failure_text):
    with subprocess.Popen(
        [args.client, server["command"], *server["args"]],
        env={**env, **server["env"]},
        start_new_session=True,
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
    ) as proc:
        try:
            assert any(
                tool["name"] == "sync_workspace"
                for tool in read_response(proc.stdout, "handshake", HANDSHAKE_TIMEOUT)[
                    "tools"
                ]
            )

            def call(name, timeout=CALL_TIMEOUT, **kwargs):
                started = time.perf_counter()
                proc.stdin.write(
                    (json.dumps({"name": name, "arguments": kwargs}) + "\n").encode()
                )
                proc.stdin.flush()
                result = read_response(proc.stdout, name, timeout)
                elapsed = (time.perf_counter() - started) * 1000
                assert not result.get("isError"), result
                return result["structuredContent"], elapsed

            def sync_and_test(sid, success):
                synced, sync_ms = call("sync_workspace", sandbox_id=sid)
                execution, test_ms = call("run_command", sandbox_id=sid, command=test)
                assert execution["exit_code"] == (0 if success else 1), execution
                if not success:
                    assert failure_text in execution["stdout"], execution
                return {
                    "sync_ms": sync_ms,
                    "test_ms": test_ms,
                    "uploaded": synced["uploaded"],
                    "exit_code": execution["exit_code"],
                    **{
                        f"{key}_ms": value / 1000
                        for key, value in synced["timing_us"].items()
                    },
                }

            created, create_ms = call("create_sandbox")
            sid = created["sandbox_id"]
            try:
                containers = command(
                    "docker",
                    "ps",
                    "-q",
                    "--filter",
                    f"label=tiny-sandbox.owner={owner}",
                    capture_output=True,
                    text=True,
                ).stdout.split()
                assert len(containers) == 1
                mode = command(
                    "docker",
                    "inspect",
                    "--format",
                    "{{.HostConfig.NetworkMode}}",
                    containers[0],
                    capture_output=True,
                    text=True,
                ).stdout.strip()
                assert created["network"] == mode == "none"
                initial = sync_and_test(sid, False)
                unchanged = sync_and_test(sid, False)
                source.write_text(fixed)
                changed = sync_and_test(sid, True)
                assert (
                    initial["uploaded"] >= 2
                    and unchanged["uploaded"] == 0
                    and changed["uploaded"] == 1
                )
                assert unchanged["apply_ms"] == unchanged["archive_upload_ms"] == 0
                return dict(
                    create_ms=create_ms,
                    initial=initial,
                    unchanged=unchanged,
                    changed=changed,
                )
            finally:
                if sys.exc_info()[0] is None:
                    call("destroy_sandbox", timeout=CLEANUP_TIMEOUT, sandbox_id=sid)
        finally:
            failing = sys.exc_info()[0] is not None
            proc.stdin.close()
            if not failing:
                try:
                    assert proc.wait(timeout=CLEANUP_TIMEOUT) == 0
                except subprocess.TimeoutExpired:
                    os.killpg(proc.pid, signal.SIGKILL)
                    proc.wait(timeout=CLEANUP_TIMEOUT)
                    raise
            else:
                try:
                    os.killpg(proc.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                proc.wait(timeout=CLEANUP_TIMEOUT)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(
        description="Exercise offline repair using a release archive and the MCP test client"
    )
    parser.add_argument("archive", type=pathlib.Path)
    parser.add_argument("client", type=pathlib.Path)
    parser.add_argument(
        "--go",
        action="store_true",
        help="use pinned google/jsonschema-go instead of the packaged demo",
    )
    parser.add_argument("--runs", type=int, default=3)
    parser.add_argument(
        "--out", type=pathlib.Path, default=pathlib.Path("artifacts/mcp-workflow.json")
    )
    args = parser.parse_args()
    if args.runs < 1:
        parser.error("--runs must be positive")
    args.client = args.client.resolve()
    args.out.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="ts-mcp-workflow-") as tmp:
        record = run(args, pathlib.Path(tmp).resolve())
    args.out.write_text(json.dumps(record, indent=2) + "\n")
    print(args.out)
