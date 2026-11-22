"""Exercise both output retention policies and permanent deletion across daemon restarts."""

import json
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
import uuid
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
OUT = ROOT / "artifacts/output-retention"
LIMIT = 1 << 20


def main():
    OUT.mkdir(parents=True, exist_ok=True)
    owner = "output-e2e-" + uuid.uuid4().hex
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        addr = f"127.0.0.1:{sock.getsockname()[1]}"
    with tempfile.TemporaryDirectory() as tmp, (OUT / "daemon.log").open("w") as log:

        def request(method, path, value=None, raw=False, headers=None):
            body = json.dumps(value).encode() if value is not None else None
            req = urllib.request.Request(
                "http://" + addr + path,
                data=body,
                method=method,
                headers=headers or {"Content-Type": "application/json"},
            )
            with urllib.request.urlopen(req, timeout=20) as resp:
                return resp.read().decode() if raw else json.load(resp)

        def start():
            daemon = subprocess.Popen(
                [
                    ROOT / "bin/sandboxd",
                    "-addr",
                    addr,
                    "-owner",
                    owner,
                    "-state-dir",
                    tmp,
                ],
                stdout=log,
                stderr=log,
            )
            for _ in range(100):
                if daemon.poll() is not None:
                    raise RuntimeError("daemon stopped, see daemon.log")
                try:
                    request("GET", "/readyz")
                    return daemon
                except OSError:
                    time.sleep(0.05)
            daemon.terminate()
            daemon.wait()
            raise TimeoutError("sandboxd startup")

        def stop(daemon):
            daemon.terminate()
            daemon.wait(timeout=30)

        daemon = start()
        try:
            sb = request(
                "POST",
                "/v1/sandboxes",
                {"image": "python:3.12-alpine", "network": "none"},
            )
            base = "/v1/sandboxes/" + sb["id"]

            def complete(req):
                first = request("POST", base + "/executions", req)
                path = base + "/executions/" + first["id"]
                for _ in range(200):
                    result = request("GET", path)
                    if result["state"] == "completed":
                        return path, result
                    time.sleep(0.05)
                raise TimeoutError(path)

            cmd = [
                "python3",
                "-c",
                "import sys;sys.stdout.write('BEGIN'+'x'*(2**20+200_000)+'END');sys.stdout.flush();sys.stderr.write('FINAL ERROR')",
            ]
            _, prefix = complete({"command": cmd})
            assert (
                prefix["output_truncated"]
                and prefix["stdout"].startswith("BEGIN")
                and not prefix["stdout"].endswith("END")
            )
            payload = {
                "command": cmd,
                "output_tail": True,
                "idempotency_key": "keep-tail",
            }
            managed_path, managed = complete(
                {"command": cmd, "output_tail": True, "timeout_ms": 60000}
            )
            assert (
                managed["stdout"].endswith("END")
                and managed["stderr"] == "FINAL ERROR"
                and managed["output_truncated"]
            )
            path, tail = complete(payload)
            assert (
                tail["output_truncated"]
                and tail["stdout"].endswith("END")
                and tail["stderr"] == "FINAL ERROR"
            ), "true tail lost"
            assert len((tail["stdout"] + tail["stderr"]).encode()) <= LIMIT

            def replay():
                events = request("GET", path + "/events", raw=True)
                parsed = [
                    block.splitlines()
                    for block in events.split("\n\n")
                    if block.startswith("id: ")
                ]
                ids = [int(block[0][4:]) for block in parsed]
                assert (
                    ids == sorted(set(ids))
                    and parsed[-1][1] == "event: complete"
                    and parsed[0][1] == "event: truncated"
                ), ids[:4]
                output = "".join(
                    json.loads(b[2][6:])["data"]
                    for b in parsed
                    if b[1] == "event: stdout"
                )
                assert output == tail["stdout"]
                return ids

            ids = replay()
            stop(daemon)
            daemon = start()
            assert (
                request("GET", path) == tail
                and request("GET", managed_path) == managed
                and replay() == ids
            )
            assert request("POST", base + "/executions", payload)["id"] == tail["id"]
            try:
                request("POST", base + "/executions", {**payload, "output_tail": False})
                raise AssertionError("retention mode ignored in idempotency")
            except urllib.error.HTTPError as err:
                assert (
                    err.code == 409 and json.load(err)["code"] == "idempotency_conflict"
                )
            request("POST", base + "/executions", {"command": ["sleep", "30"]})
            assert request("DELETE", base + "?purge=true")["state"] == "killed"
            stop(daemon)
            daemon = start()
            assert all(
                x["id"] != sb["id"]
                for x in request("GET", "/v1/sandboxes")["sandboxes"]
            )
            assert not list(Path(tmp).glob(sb["id"] + "*.json"))
            report = dict(
                prefix_bytes=len(prefix["stdout"]),
                tail_bytes=len(tail["stdout"] + tail["stderr"]),
                stderr=tail["stderr"],
                suffix=tail["stdout"][-3:],
                first_event_id=ids[0],
                last_event_id=ids[-1],
                managed_tail_after_restart=True,
                identical_replay_after_restart=True,
                purged_after_restart=True,
            )
            (OUT / "acceptance.json").write_text(json.dumps(report, indent=2) + "\n")
        finally:
            if daemon.poll() is None:
                stop(daemon)
            ids = subprocess.check_output(
                [
                    "docker",
                    "ps",
                    "-aq",
                    "--filter",
                    "label=tiny-sandbox.owner=" + owner,
                ],
                text=True,
            ).split()
            if ids:
                subprocess.run(
                    ["docker", "rm", "-f", *ids], check=True, stdout=subprocess.DEVNULL
                )


if __name__ == "__main__":
    main()
