"""Exercise protected guest control under real Docker process and memory limits."""

import argparse
import json
import socket
import subprocess
import time
import uuid
from http.client import HTTPConnection
from pathlib import Path


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server", type=Path, default=Path("bin/execletd"))
    parser.add_argument("--out", type=Path, default=Path("artifacts/guest-faults"))
    args = parser.parse_args()
    args.out.mkdir(parents=True, exist_ok=True)
    owner = "guest-fault-" + uuid.uuid4().hex
    with socket.socket() as free:
        free.bind(("127.0.0.1", 0))
        port = free.getsockname()[1]
    report = {}

    def record(key, value):
        report[key] = value
        (args.out / "report.json").write_text(json.dumps(report, indent=2) + "\n")

    def api(method, path, body=None):
        conn = HTTPConnection("127.0.0.1", port, timeout=20)
        try:
            conn.request(
                method,
                path,
                None if body is None else json.dumps(body),
                {"Content-Type": "application/json"},
            )
            res = conn.getresponse()
            value = json.loads(res.read())
            assert 200 <= res.status < 300, (method, path, res.status, value)
            return value
        finally:
            conn.close()

    def wait(path, predicate):
        until = time.monotonic() + 20
        while time.monotonic() < until:
            value = api("GET", path)
            if predicate(value):
                return value
            time.sleep(0.03)
        raise AssertionError(value)

    def docker(*args):
        return subprocess.check_output(["docker", *args], text=True).strip()

    def create(image, **kw):
        sb = api(
            "POST",
            "/v1/sandboxes",
            dict(image=image, network="none", memory_mb=64, pids_limit=64, **kw),
        )
        assert sb["agent_mode"] == "guest", sb
        return sb, "/v1/sandboxes/" + sb["id"]

    def start(base, cmd, **kw):
        ex = api("POST", base + "/executions", dict(command=cmd, timeout_ms=0, **kw))
        return base + "/executions/" + ex["id"]

    log = (args.out / "server.log").open("w")
    server = subprocess.Popen(
        [str(args.server.resolve()), "-addr", f"127.0.0.1:{port}", "-owner", owner],
        stdout=log,
        stderr=log,
    )
    try:
        for _ in range(150):
            try:
                api("GET", "/readyz")
                break
            except OSError:
                time.sleep(0.05)
        sb, base = create("execlet-guest:minimal")
        assert sb["protected"]
        cid = "execlet-" + sb["id"]
        pid = docker(
            "exec",
            "-u",
            "0",
            cid,
            "sh",
            "-c",
            'pgrep -f "^/.execlet/guest daemon "',
        )
        check = 'id; test ! -r /run/execlet/agent.sock && ! touch /run/execlet/edit && ! kill -0 "$1" && test "$(id -u)" = 1000'
        ex = wait(
            start(base, ["sh", "-c", check, "guest", pid]),
            lambda v: v["state"] == "completed",
        )
        assert ex["exit_code"] == 0, ex
        record("nonroot_protection", ex)

        counter = [
            "sh",
            "-c",
            "echo side-effect >>/tmp/run-count; echo ready; while [ ! -e /tmp/release ]; do sleep .02; done; cat /tmp/run-count",
        ]
        ex_path = start(base, counter, idempotency_key="relay-reconnect")
        wait(ex_path, lambda v: "ready" in v["stdout"])
        relay = docker(
            "exec",
            "-u",
            "0",
            cid,
            "sh",
            "-c",
            'pgrep -f "^/.execlet/guest connect$"',
        )
        relays = relay.split()
        assert relays and all(pid.isdigit() for pid in relays), relay
        docker("exec", "-u", "0", cid, "kill", "-KILL", *relays)
        replay = start(base, counter, idempotency_key="relay-reconnect")
        assert replay == ex_path, (ex_path, replay)
        docker("exec", cid, "touch", "/tmp/release")
        resumed = wait(replay, lambda v: v["state"] == "completed")
        assert (
            resumed["exit_code"] == 0 and resumed["stdout"] == "ready\nside-effect\n"
        ), resumed
        record("relay_reconnect_once", resumed)

        command = ["sh", "-c", "for n in $(seq 1 64); do sleep 120 & done; wait"]
        ex_path = start(base, command)
        until = time.monotonic() + 8
        while time.monotonic() < until:
            pids = int(docker("stats", "--no-stream", "--format", "{{.PIDs}}", cid))
            if pids >= 64:
                break
        assert pids == 64, pids
        result = api("POST", ex_path + "/kill")
        assert (
            result["signaled"]
            and result["execution"]["termination_reason"] == "canceled"
        ), result
        assert api("GET", base)["state"] == "running"
        record("pid_limit_cancellation", dict(pids=pids, limit=64, result=result))
        api("DELETE", base)

        sb, base = create("python:3.12-alpine")
        assert not sb["protected"]
        cid = "execlet-" + sb["id"]
        ex = wait(start(base, ["id", "-u"]), lambda v: v["state"] == "completed")
        assert ex["stdout"].strip() == "0", ex
        record("root_identity", ex)

        ex_path = start(base, ["sleep", "120"])
        pid = docker(
            "exec", cid, "sh", "-c", 'pgrep -f "^/.execlet/guest daemon "'
        )
        assert pid.isdigit(), pid
        docker(
            "exec",
            cid,
            "sh",
            "-c",
            'echo 1000 > /proc/"$1"/oom_score_adj',
            "setup",
            pid,
        )
        event_start = str(int(time.time()))
        allocator = subprocess.run(
            [
                "docker",
                "exec",
                cid,
                "python3",
                "-c",
                "import time; blocks=[]\nfor _ in range(80): blocks.append(bytearray(1<<20)); time.sleep(.015)",
            ],
            capture_output=True,
            timeout=15,
        )
        result = wait(ex_path, lambda v: v["state"] == "completed")
        assert (
            result["termination_reason"] == "unknown"
            and result["output_truncated"]
            and result["exit_code"] is None
        ), result
        sandbox = wait(base, lambda v: v["state"] == "killed")
        events = docker(
            "events",
            "--since",
            event_start,
            "--until",
            str(int(time.time()) + 1),
            "--filter",
            "event=oom",
            "--filter",
            "label=execlet.owner=" + owner,
            "--format",
            "{{.Action}}",
        )
        assert "oom" in events.splitlines(), events
        record(
            "guest_oom",
            dict(
                docker_events=events,
                allocator_exit=allocator.returncode,
                result=result,
                sandbox=sandbox,
            ),
        )
    finally:
        server.terminate()
        server.wait(timeout=30)
        log.close()
        assert not docker("ps", "-aq", "--filter", "label=execlet.owner=" + owner)


if __name__ == "__main__":
    main()
