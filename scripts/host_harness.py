"""A private daemon and its exact Docker owner, shared by host acceptance runs."""
import contextlib
import hashlib
import http.client
import json
import os
import re
import secrets
import signal
import socket
import subprocess
import sys
import tempfile
import time
from pathlib import Path


@contextlib.contextmanager
def report(path):
    def save():
        f = tempfile.NamedTemporaryFile(mode="w", dir=path.parent, delete=False)
        try:
            with f:
                json.dump(record, f, indent=2, allow_nan=False)
                f.write("\n")
            os.replace(f.name, path)
        finally:
            Path(f.name).unlink(missing_ok=True)

    def terminate(signum, _frame):
        raise SystemExit(128 + signum)

    path.parent.mkdir(parents=True, exist_ok=True)
    record = {"schema_version": 1, "passed": False}
    save()
    previous = signal.signal(signal.SIGTERM, terminate)
    try:
        yield record
    except BaseException as error:
        record["error"] = f"{type(error).__name__}: {error}"
        raise
    else:
        record["passed"] = True
    finally:
        signal.signal(signal.SIGTERM, previous)
        save()


def checksum(path):
    digest = hashlib.sha256()
    with path.open("rb") as f:
        for block in iter(lambda: f.read(1 << 20), b""):
            digest.update(block)
    return digest.hexdigest()


def docker_host():
    host = os.environ.get("DOCKER_HOST")
    if os.environ.get("DOCKER_CONTEXT") or not host:
        host = subprocess.check_output(
            ["docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}"],
            text=True, timeout=15).strip()
    if not host.startswith("unix:///"):
        raise ValueError(f"a local Docker Unix socket is required, got {host!r}")
    return host


class Daemon:
    def __init__(self, server, work, *, state_dir=None):
        self.server = Path(server).resolve()
        self.work = Path(work)
        self.owner = "host-harness-" + secrets.token_hex(12)
        self.api_key = secrets.token_hex(24)
        self.host = docker_host()
        self.env = {**os.environ, "DOCKER_HOST": self.host, "SANDBOX_API_KEY": self.api_key}
        self.env.pop("DOCKER_CONTEXT", None)
        with socket.socket() as sock:
            sock.bind(("127.0.0.1", 0))
            self.port = sock.getsockname()[1]
        address = f"127.0.0.1:{self.port}"
        self.domain = "http://" + address
        self.env["SANDBOX_DOMAIN"] = self.domain
        self.args = [str(self.server), "-addr", address, "-owner", self.owner]
        if state_dir:
            self.args += ["-state-dir", str(state_dir)]
        self.generation = 0
        self.process = None

    def set_policy(self, policy):
        path = self.work / "policy.json"
        path.write_text(json.dumps(policy))
        self.args += ["-policy", str(path)]

    def docker(self, *args, timeout=30):
        return subprocess.check_output(["docker", *args], text=True, env=self.env,
                                       timeout=timeout).strip()

    def image_id(self, name):
        image = self.docker("image", "inspect", "--format", "{{.Id}}", "--", name)
        if not re.fullmatch(r"sha256:[0-9a-f]{64}", image):
            raise ValueError(f"could not resolve local image {name!r} to an immutable ID")
        return image

    def start(self):
        if self.process is not None:
            raise RuntimeError("daemon already started")
        path = self.work / f"daemon-{self.generation}.log"
        self.generation += 1
        with path.open("w") as log:
            self.process = subprocess.Popen(self.args, cwd=self.server.parent, env=self.env, stdin=subprocess.DEVNULL,
                                            stdout=log, stderr=log)
        try:
            deadline = time.monotonic() + 30
            while time.monotonic() < deadline and self.process.poll() is None:
                try:
                    if (self.request("/readyz", timeout=1)[0] == 200
                            and self.request("/metrics", timeout=1, authenticated=False)[0] == 401
                            and self.request("/metrics", timeout=1)[0] == 200):
                        return self
                except (OSError, http.client.HTTPException):
                    pass
                time.sleep(0.05)
            with path.open("rb") as log:
                log.seek(max(0, path.stat().st_size - 1000))
                raise RuntimeError(f"daemon not ready: {log.read().decode(errors='replace')}")
        except BaseException:
            self.stop(crash=True)
            raise

    def stop(self, *, crash=False):
        proc = self.process
        if proc is None:
            return
        try:
            if proc.poll() is None:
                proc.kill() if crash else proc.terminate()
            code = proc.wait(timeout=35)
            if code and not crash:
                raise RuntimeError(f"daemon exited with status {code}")
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait(timeout=10)
            raise
        finally:
            self.process = None

    @contextlib.contextmanager
    def running(self):
        try:
            self.start()
            yield self
        finally:
            primary = sys.exc_info()[1]
            try:
                try:
                    self.stop()
                finally:
                    remaining = self.owned()
                    if remaining:
                        self.docker("rm", "-f", *remaining)
                        self.require_empty()
                        raise RuntimeError(f"removed {len(remaining)} leftover containers for {self.owner}")
            except Exception as cleanup:
                if primary is None:
                    raise
                print(f"{self.owner}: {cleanup}", file=sys.stderr)

    def request(self, path, timeout=10, *, authenticated=True):
        conn = http.client.HTTPConnection("127.0.0.1", self.port, timeout=timeout)
        try:
            headers = {"Authorization": "Bearer " + self.api_key} if authenticated else {}
            conn.request("GET", path, headers=headers)
            response = conn.getresponse()
            return response.status, response.read().decode()
        finally:
            conn.close()

    def gauges(self):
        status, body = self.request("/metrics")
        if status != 200:
            raise RuntimeError(f"metrics returned {status}")
        values = dict(line.split() for line in body.splitlines()
                      if line and not line.startswith("#") and "{" not in line)
        return {key: int(values["execlet_" + key])
                for key in ("live", "cleanup_pending", "cleanup_failures_total")}

    def owned(self, sandbox_id=None):
        filters = ["--filter", "label=execlet.owner=" + self.owner]
        if sandbox_id is not None:
            filters += ["--filter", "label=execlet.id=" + sandbox_id]
        return self.docker("ps", "-aq", "--no-trunc", *filters).split()

    def require_empty(self, timeout=10):
        deadline = time.monotonic() + timeout
        while True:
            remaining = self.owned()
            if not remaining:
                return
            if time.monotonic() >= deadline:
                raise RuntimeError(f"{self.owner}: containers still present: {remaining}")
            time.sleep(0.2)
