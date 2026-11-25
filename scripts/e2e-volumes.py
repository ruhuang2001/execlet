#!/usr/bin/env python3
"""Exercise persistent volumes against the local Linux Docker host."""

import argparse
import hashlib
import io
import json
import os
import platform
import socket
import subprocess
import sys
import tarfile
import tempfile
import time
import traceback
import uuid
from concurrent.futures import ThreadPoolExecutor
from http.client import HTTPConnection, HTTPException
from pathlib import Path


SIZE = 16 << 20
PATH = "/mnt/work"
sys.path.insert(0, str(Path(__file__).resolve().parent / "e2b-compatibility"))


def shell(*args):
    return subprocess.check_output(args, text=True, timeout=45).strip()


def archive_bytes(*members):
    raw = io.BytesIO()
    with tarfile.open(fileobj=raw, mode="w") as archive:
        for name, data in members:
            entry = tarfile.TarInfo(name)
            entry.size = len(data)
            archive.addfile(entry, io.BytesIO(data))
    return raw.getvalue()


class Suite:
    def __init__(self, args, work, out, runtime):
        self.args, self.work, self.runtime = args, work, runtime
        self.state = work / "state"
        self.policy = work / "policy.json"
        self.owner = "volume-e2e-" + uuid.uuid4().hex
        self.key = uuid.uuid4().hex
        self.process = None
        self.evidence = {"owner": self.owner, "checks": {}}
        self.log = out / f"{runtime}.log"
        self.spec = dict(image=args.image, runtime=runtime, memory_mb=128, pids_limit=128,
                         nano_cpus=500000000, network="none", timeout_seconds=0)
        self.policy.write_text(json.dumps(dict(
            max_sandboxes=4, max_volumes=2, max_volume_bytes=SIZE,
            total_memory_mb=512, total_pids=512, total_nano_cpus=2000000000,
            allowed_images=[args.image], allowed_networks=["none"], allowed_runtimes=[runtime],
        )))
        with socket.socket() as sock:
            sock.bind(("127.0.0.1", 0))
            self.port = sock.getsockname()[1]

    def start(self, volumes=True, proxy=None):
        policy = self.policy
        if not volumes:
            entries = json.loads(policy.read_text())
            del entries["max_volumes"], entries["max_volume_bytes"]
            policy = self.work / "disabled.json"
            policy.write_text(json.dumps(entries))
        docker = ["-docker", proxy.path] if proxy else []
        with self.log.open("ab") as log:
            self.process = subprocess.Popen([
                self.args.server, "-addr", f"127.0.0.1:{self.port}", "-owner", self.owner,
                "-state-dir", self.state, "-policy", policy, *docker,
            ], env=dict(os.environ, SANDBOX_API_KEY=self.key), stdout=log, stderr=log)
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            assert self.process.poll() is None, f"daemon exited; see {self.log}"
            try:
                return self.request("GET", "/readyz", 200)
            except OSError:
                time.sleep(.1)
        raise RuntimeError(f"daemon not ready; see {self.log}")

    def stop(self, crash=False):
        if self.process is None:
            return
        if self.process.poll() is None:
            if crash:
                self.process.kill()
            else:
                self.process.terminate()
            try:
                self.process.wait(timeout=15)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait(timeout=5)
                raise
        self.process = None

    def request(self, method, path, want, body=None, code=None, auth=True):
        conn = HTTPConnection("127.0.0.1", self.port, timeout=45)
        headers = {"Authorization": "Bearer " + self.key} if auth else {}
        if isinstance(body, bytes):
            headers["Content-Type"] = "application/octet-stream"
        elif body is not None:
            headers["Content-Type"] = "application/json"
            body = json.dumps(body).encode()
        try:
            conn.request(method, path, body, headers)
            response = conn.getresponse()
            raw = response.read()
            value = json.loads(raw) if response.getheader("Content-Type", "").startswith("application/json") else raw
            assert response.status == want, (method, path, response.status, want, preview(value))
            if code:
                assert value["code"] == code, (method, path, value, code)
            if method == "POST" and path == "/v1/volumes" and want == 201:
                assert response.getheader("Location") == "/v1/volumes/" + value["id"], value
            return value
        finally:
            conn.close()

    def new_volume(self):
        v = self.request("POST", "/v1/volumes", 201, {"size_bytes": SIZE})
        assert v["size_bytes"] == SIZE and v["created_at"], v
        return v

    def attach(self, v, read_only=False):
        mount = dict(volume_id=v["id"], path=PATH, read_only=read_only)
        sb = self.request("POST", "/v1/sandboxes", 201, dict(self.spec, mounts=[mount]))
        assert sb["agent_mode"] == "guest" and sb["protected"] and sb["runtime"] == self.runtime, sb
        assert sb["mounts"][0] | {"read_only": sb["mounts"][0].get("read_only", False)} == mount, sb
        return "/v1/sandboxes/" + sb["id"]

    def file(self, sb, name, data=None, status=200, code=None):
        return self.request("GET" if data is None else "PUT", sb + "/files?path=" + PATH + "/" + name,
                            status, data, code=code)

    def run_command(self, sb, *argv):
        ex = self.request("POST", sb + "/executions", 201, {"command": argv})
        endpoint = sb + "/executions/" + ex["id"]
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            ex = self.request("GET", endpoint, 200)
            if ex["state"] == "completed":
                return ex
            time.sleep(.05)
        raise RuntimeError(f"execution did not complete: {ex}")

    def run(self):
        self.start(volumes=False)
        assert "volumes.v1" not in self.request("GET", "/v1/capabilities", 200)["capabilities"]
        self.request("GET", "/v1/volumes", 503, code="unavailable")
        self.stop()
        self.evidence["health"] = self.start()
        check = self.evidence["checks"]
        assert "volumes.v1" in self.request("GET", "/v1/capabilities", 200)["capabilities"]
        self.request("GET", "/v1/volumes", 401, code="unauthorized", auth=False)
        assert self.request("GET", "/v1/volumes", 200) == {"volumes": []}
        self.request("POST", "/v1/volumes", 400, {"size_bytes": SIZE + 1}, code="invalid_request")
        self.request("POST", "/v1/volumes", 400, {"size_bytes": SIZE << 1}, code="invalid_request")
        a, b = self.new_volume(), self.new_volume()
        for v in (a, b):
            assert self.request("GET", "/v1/volumes/" + v["id"], 200) == v
        assert self.request("GET", "/v1/volumes", 200)["volumes"] == sorted([a, b], key=lambda v: v["id"])
        check["count_limit"] = self.request("POST", "/v1/volumes", 429, {"size_bytes": SIZE}, code="rate_limited")
        va, vb = ("/v1/volumes/" + v["id"] for v in (a, b))
        image = self.state / "volumes" / a["id"] / "disk.img"
        allocated = int(shell("du", "-B1", image).split()[0])
        assert allocated >= SIZE and image.stat().st_size == SIZE, allocated
        mount = image.with_name("data")
        space = os.statvfs(mount)
        assert space.f_blocks * space.f_frsize <= SIZE, space
        loop = shell("losetup", "-j", image)
        assert loop.startswith("/dev/loop"), loop

        one, two = self.attach(a), self.attach(b)
        unrelated = self.request("POST", "/v1/sandboxes", 201, self.spec)
        untouched = "/v1/sandboxes/" + unrelated["id"]
        self.file(untouched, "same", status=404, code="not_found")
        self.request("DELETE", untouched, 200)
        conflict = dict(volume_id=a["id"], path=PATH, read_only=True)
        check["exclusive"] = self.request("POST", "/v1/sandboxes", 409, dict(self.spec, mounts=[conflict]), code="volume_in_use")
        check["running_delete"] = self.request("DELETE", va, 409, code="volume_in_use")
        a_data, b_data = b"first volume: persisted\n", b"second volume: separate\n"
        assert self.file(one, "same", a_data)["uid"] == 1000
        guest = self.run_command(two, "sh", "-c", 'printf "%s\n" "second volume: separate" > /mnt/work/same')
        assert guest["exit_code"] == 0, guest
        assert self.file(one, "same") == a_data and self.file(two, "same") == b_data
        check["isolated_sha256"] = [hashlib.sha256(self.file(s, "same")).hexdigest() for s in (one, two)]

        assert self.request("POST", one + "/pause", 200)["state"] == "paused"
        name = "execlet-" + one.rsplit("/", 1)[1]
        assert shell("docker", "inspect", "--format", "{{.State.Paused}}", name) == "true"
        check["paused_delete"] = self.request("DELETE", va, 409, code="volume_in_use")
        assert self.request("POST", one + "/resume", 200)["state"] == "running"
        assert self.file(one, "same") == a_data
        self.stop(crash=True)
        self.start()
        assert shell("docker", "inspect", "--format", "{{.HostConfig.Runtime}}", name) == self.runtime
        assert self.request("GET", one, 200)["state"] == "running"
        check["restart_delete"] = self.request("DELETE", va, 409, code="volume_in_use")
        assert self.file(one, "same") == a_data
        self.request("DELETE", one, 200)
        self.request("DELETE", two, 200)
        self.request("DELETE", vb, 204)

        read_only = self.attach(a, read_only=True)
        assert self.file(read_only, "same") == a_data
        check["read_only_file"] = self.file(read_only, "same", b"overwrite", 409, "volume_read_only")
        check["read_only_tar"] = self.request("PUT", read_only + "/files/archive?path=" + PATH, 409,
                                              archive_bytes(("same", b"overwrite")), code="volume_read_only")
        mixed = archive_bytes(("marker", b"first"), ("work/same", b"overwrite"))
        check["read_only_parent_tar"] = self.request("PUT", read_only + "/files/archive?path=/mnt", 409, mixed, code="volume_read_only")
        assert self.request("GET", read_only + "/files?path=/mnt/marker", 200) == b"first"
        guest = self.run_command(read_only, "sh", "-c", "echo overwrite > /mnt/work/same")
        assert guest["exit_code"] != 0 and self.file(read_only, "same") == a_data, guest
        self.request("DELETE", read_only, 200)

        writable = self.attach(a)
        fill = self.run_command(writable, "dd", "if=/dev/zero", "of=/mnt/work/fill", "bs=1M", "count=16", "conv=fsync")
        check["guest_fill"] = {"exit_code": fill["exit_code"], "stderr": fill["stderr"],
                               "free_bytes": os.statvfs(mount).f_bavail * space.f_frsize}
        assert fill["exit_code"] != 0 and check["guest_fill"]["free_bytes"] < (2 << 20), fill
        check["quota_file"] = self.file(writable, "same", b"x" * (2 << 20), 507, "volume_quota_exceeded")
        assert self.file(writable, "same") == a_data
        mixed = archive_bytes(("marker", b"written"), ("work/fill-more", b"x" * (2 << 20)))
        check["quota_parent_tar"] = self.request("PUT", writable + "/files/archive?path=/mnt", 507, mixed, code="volume_quota_exceeded")
        assert self.request("GET", writable + "/files?path=/mnt/marker", 200) == b"written"
        files = self.request("GET", writable + "/files/list?path=" + PATH, 200)["entries"]
        assert not any(f["name"].startswith(".execlet-upload-") for f in files), files
        rm = self.run_command(writable, "rm", PATH + "/fill")
        assert rm["exit_code"] == 0, rm
        assert self.file(writable, "new", b"space released")["size"] == 14
        self.request("DELETE", writable, 200)

        for crash in (False, True):
            self.check_delayed_create(a, crash)
            reused = self.attach(a)
            assert self.file(reused, "same") == a_data
            self.request("DELETE", reused, 200)

        self.request("DELETE", va, 204)
        assert not image.parent.exists() and not shell("losetup", "-j", image), loop
        self.request("GET", va, 404, code="not_found")
        assert self.request("GET", "/v1/volumes", 200) == {"volumes": []}
        check["disk_reclaimed"] = {"allocated_bytes_before_delete": allocated, "directory_exists_after": image.parent.exists(),
                                   "loop_before": loop.split(":", 1)[0], "loop_attached_after": False}
        replacement = self.new_volume()
        self.request("DELETE", "/v1/volumes/" + replacement["id"], 204)

    def check_delayed_create(self, volume, crash):
        from local import DockerFault

        label = "restart_in_flight_create" if crash else "lost_create_response"
        evidence = self.evidence["checks"][label] = {}
        endpoint = "/v1/volumes/" + volume["id"]
        request = {**self.spec, "mounts": [{"volume_id": volume["id"], "path": PATH}]}
        self.stop()
        proxy = DockerFault(self.work / "docker.sock")
        try:
            self.start(proxy=proxy)
            proxy.arm(True)
            with proxy.lock:
                proxy.drop_create = proxy.delay_create = True
                proxy.disconnect_create_first = not crash
            with ThreadPoolExecutor(max_workers=1) as workers:
                pending = workers.submit(self.request, "POST", "/v1/sandboxes", 502, request, "backend_error")
                try:
                    assert proxy.create_waiting.wait(10), "fault proxy did not intercept Docker create"
                    if crash:
                        sandboxes = self.request("GET", "/v1/sandboxes", 200)["sandboxes"]
                        candidates = [sb for sb in sandboxes if sb["state"] == "pending"]
                        assert len(candidates) == 1, candidates
                        pending_path = "/v1/sandboxes/" + candidates[0]["id"]
                        evidence["delete_pending_sandbox"] = self.request("DELETE", pending_path, 409, code="invalid_state")
                        self.stop(crash=True)
                        try:
                            pending.result(timeout=8)
                        except (OSError, HTTPException):
                            evidence["http_lost_on_crash"] = True
                        else:
                            raise AssertionError("sandbox create unexpectedly completed before crash")
                    else:
                        evidence["create_error"] = pending.result(timeout=8)
                        evidence["delete_before_restart"] = self.request("DELETE", endpoint, 409, code="volume_in_use")
                        self.stop(crash=True)
                except BaseException:
                    proxy.release_create.set()
                    raise

            assert not shell("docker", "ps", "-aq", "--filter", "label=execlet.owner=" + self.owner)
            self.start(proxy=proxy)
            evidence["delete_after_restart"] = self.request("DELETE", endpoint, 409, code="volume_in_use")
            evidence["reattach_after_restart"] = self.request("POST", "/v1/sandboxes", 409, request, "volume_in_use")

            proxy.release_create.set()
            assert proxy.create_finished.wait(10) and proxy.lost_create_id, "late create did not reach Docker"
            container = evidence["late_container"] = proxy.lost_create_id
            deadline = time.monotonic() + 8
            while not any(f"/containers/{container}?" in row for row in proxy.removals()):
                assert time.monotonic() < deadline, "daemon did not attempt the late container removal"
                time.sleep(.05)
            assert shell("docker", "inspect", "--format", "{{.State.Status}}", container) == "created"
            evidence["delete_during_remove_failure"] = self.request("DELETE", endpoint, 409, code="volume_in_use")
            proxy.arm(False)
            deadline = time.monotonic() + 8
            while True:
                metrics = self.request("GET", "/metrics", 200).decode().splitlines()
                if "execlet_cleanup_pending 0" in metrics:
                    break
                assert time.monotonic() < deadline, "daemon did not confirm the late container removal"
                time.sleep(.05)
            assert not shell("docker", "ps", "-aq", "--filter", "id=" + container)
            evidence["cleanup_confirmed"] = True
        finally:
            self.stop()
            proxy.release_create.set()
            if proxy.create_waiting.is_set():
                proxy.create_finished.wait(10)
            proxy.close()
        self.start()

    def cleanup(self):
        try:
            self.stop()
        finally:
            filters = ("docker", "ps", "-aq", "--filter", "label=execlet.owner=" + self.owner)
            remaining = self.evidence["containers_before_cleanup"] = shell(*filters).splitlines()
            if remaining:
                shell("docker", "rm", "-f", *remaining)
            volumes = self.state / "volumes"
            for point in volumes.glob("vol_*/data"):
                if os.path.ismount(point):
                    shell("umount", point)
            assert not shell(*filters), "suite left owner-labelled containers"
            assert not remaining or self.evidence.get("error"), remaining


def preview(body):
    if isinstance(body, bytes):
        return repr(body[:200])
    return body


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server", type=Path, default=Path("bin/execletd"))
    parser.add_argument("--image", default="execlet-guest:minimal")
    parser.add_argument("--runtime", choices=("runc", "runsc"))
    parser.add_argument("--out", type=Path, default=Path("artifacts/volumes"))
    args = parser.parse_args()
    if not __debug__:
        parser.error("this suite requires Python assertions to be enabled")
    args.server = args.server.resolve()
    args.out.mkdir(parents=True, exist_ok=True)
    assert platform.system() == "Linux" and os.geteuid() == 0, "run as root on the local Linux Docker host"
    info = json.loads(shell("docker", "info", "--format", "{{json .}}"))
    report = dict(kernel=platform.release(), arch=platform.machine(), docker=info["ServerVersion"],
                  image_id=shell("docker", "image", "inspect", "--format", "{{.Id}}", args.image),
                  daemon_sha256=hashlib.sha256(args.server.read_bytes()).hexdigest(), runtimes={})
    for runtime in [args.runtime] if args.runtime else ["runc", "runsc"]:
        with tempfile.TemporaryDirectory(prefix="execlet-volume-") as tmp:
            suite = Suite(args, Path(tmp), args.out, runtime)
            report["runtimes"][runtime] = result = suite.evidence
            try:
                assert runtime in info["Runtimes"], f"Docker runtime {runtime} is not registered"
                suite.run()
                result["status"] = "passed"
            except Exception:
                result.update(status="failed", error=traceback.format_exc())
                print(result["error"], flush=True)
            finally:
                try:
                    suite.cleanup()
                except Exception:
                    result.update(status="failed", cleanup_error=traceback.format_exc())
                    print(result["cleanup_error"], flush=True)
    failed = any(r["status"] != "passed" for r in report["runtimes"].values())
    report["status"] = "failed" if failed else "passed"
    path = args.out / "report.json"
    path.write_text(json.dumps(report, indent=2) + "\n")
    print(f"{report['status']}: {path}", flush=True)
    return int(failed)


if __name__ == "__main__":
    raise SystemExit(main())
