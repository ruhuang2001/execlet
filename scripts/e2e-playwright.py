"""Run a fixed offline Chromium screenshot through Execlet's public API."""

import argparse
import hashlib
import json
import os
import signal
import socket
import struct
import subprocess
import sys
import time
import uuid
import zlib
from contextlib import closing
from http.client import HTTPConnection, HTTPException
from pathlib import Path
from urllib.parse import urlencode

ROOT = Path(__file__).resolve().parents[1]
EXAMPLE = ROOT / "examples/playwright"
FIXTURE = "/tmp/execlet-playwright.html"
SCREENSHOT = "/tmp/execlet-playwright.png"
VIEWPORT = (800, 480)


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def output(cmd, timeout=30):
    proc = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout)
    require(proc.returncode == 0, f"{cmd}: {proc.stderr.strip() or proc.stdout.strip()}")
    return proc.stdout.strip()


def validate_png(data):
    require(len(data) >= 1024 and data.startswith(b"\x89PNG\r\n\x1a\n"), "missing or tiny PNG")
    require(data[8:16] == b"\x00\x00\x00\rIHDR", "missing PNG IHDR")
    width, height = struct.unpack_from(">II", data, 16)
    require((width, height) == VIEWPORT, f"unexpected screenshot viewport: {width}x{height}")

    offset, has_pixels = 8, False
    while offset + 12 <= len(data):
        size, kind = struct.unpack_from(">I4s", data, offset)
        end = offset + 8 + size
        require(end + 4 <= len(data), f"truncated PNG chunk {kind!r}")
        crc = struct.unpack_from(">I", data, end)[0]
        require(zlib.crc32(data[offset + 4 : end]) == crc, f"bad PNG CRC in {kind!r}")
        offset = end + 4
        if kind == b"IDAT" and size:
            has_pixels = True
        if kind == b"IEND":
            require(size == 0 and offset == len(data) and has_pixels, "invalid PNG end or no pixels")
            return width, height
    raise RuntimeError("missing PNG IEND")


def run(args, owner, out):
    context = os.getenv("DOCKER_CONTEXT")
    endpoint = None if context else os.getenv("DOCKER_HOST")
    if not endpoint:
        endpoint = output([
            "docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}",
            *([context] if context else []),
        ])
    require(endpoint.startswith("unix://"), f"a Docker Unix socket is required, got {endpoint!r}")
    docker = ["docker", "--host", endpoint]
    temp_image = None if args.image else f"execlet-playwright:{owner}"
    image_ref = args.image or temp_image
    owner_filter = ["--filter", f"label=execlet.owner={owner}"]

    api_key = uuid.uuid4().hex

    def request(method, path, body=None, *, status=200, auth=True, timeout=15):
        headers = {"Authorization": f"Bearer {api_key}"} if auth else {}
        if isinstance(body, bytes):
            headers["Content-Type"] = "application/octet-stream"
        elif body is not None:
            headers["Content-Type"] = "application/json"
            body = json.dumps(body).encode()
        with closing(HTTPConnection("127.0.0.1", port, timeout=timeout)) as conn:
            conn.request(method, path, body, headers)
            response = conn.getresponse()
            data = response.read()
            require(
                response.status == status,
                f"{method} {path}: HTTP {response.status}, wanted {status}: {data[:2048]!r}",
            )
            return data, response.getheader("Content-Type")

    report = {"owner": owner, "image_ref": image_ref}
    server = None
    sandbox_path = None
    completed = False
    build_attempted = False
    cleanup_errors = []
    try:
        for binary in (args.server, args.cli):
            require(binary.is_file() and os.access(binary, os.X_OK), f"missing executable {binary}; run make build first")
        report["execletd"] = output([args.server, "-version"])
        report["execletctl"] = output([args.cli, "version"])
        report["docker"] = output([*docker, "version", "--format", "{{.Server.Os}}/{{.Server.Arch}} {{.Server.Version}}"])
        if temp_image:
            print(f"Building {temp_image}; see {out / 'build.log'}", flush=True)
            with (out / "build.log").open("w") as log:
                build_attempted = True
                build = subprocess.run(
                    [*docker, "build", "--load", "--progress=plain", "-t", temp_image, EXAMPLE],
                    stdout=log, stderr=subprocess.STDOUT, timeout=1200,
                )
            require(build.returncode == 0, f"Docker build failed; see {out / 'build.log'}")
        image = json.loads(output([*docker, "image", "inspect", "--", image_ref]))[0]
        report["image"] = {key: image[key] for key in ("Id", "Os", "Architecture", "Size")}

        with socket.socket() as allocation:
            allocation.bind(("127.0.0.1", 0))
            port = allocation.getsockname()[1]
        address = f"127.0.0.1:{port}"
        env = dict(os.environ, SANDBOX_DOMAIN=f"http://{address}", SANDBOX_API_KEY=api_key)
        with (out / "execletd.log").open("w") as log:
            server = subprocess.Popen(
                [args.server, "-addr", address, "-docker", endpoint.removeprefix("unix://"), "-owner", owner],
                stdout=log, stderr=subprocess.STDOUT, env=env,
            )
        deadline = time.monotonic() + 15
        while True:
            require(server.poll() is None, f"execletd exited; see {out / 'execletd.log'}")
            try:
                request("GET", "/readyz", timeout=2)
                break
            except (OSError, HTTPException, RuntimeError) as exc:
                if time.monotonic() >= deadline:
                    raise RuntimeError(f"execletd readiness timed out: {exc}") from exc
                time.sleep(0.1)

        sandboxes = "/v1/sandboxes"
        request("GET", sandboxes, status=401, auth=False)
        require(json.loads(request("GET", sandboxes)[0])["sandboxes"] == [], "expected a fresh daemon")
        spec = {
            "image": image["Id"], "network": "none", "memory_mb": 1024, "timeout_seconds": 180,
        }
        created = json.loads(request("POST", sandboxes, spec, status=201)[0])
        sandbox_id = created["id"]
        sandbox_path = f"{sandboxes}/{sandbox_id}"
        report["sandbox"] = created
        require(created["network"] == "none", f"Execlet reported network {created['network']!r}")

        container_name = f"execlet-{sandbox_id}"
        container = json.loads(output([*docker, "container", "inspect", container_name]))[0]
        require(container["Config"]["Labels"]["execlet.owner"] == owner, "unexpected container owner")
        require(container["Image"] == image["Id"], "unexpected container image")
        report["docker_network"] = container["HostConfig"]["NetworkMode"]
        report["docker_runtime"] = container["HostConfig"]["Runtime"]
        require(report["docker_network"] == "none", "Docker container is not running with network none")

        files = f"{sandbox_path}/files"
        fixture_query = urlencode({"path": FIXTURE})
        screenshot_query = urlencode({"path": SCREENSHOT})
        source = (EXAMPLE / "fixture.html").read_bytes()
        entry = json.loads(request("PUT", f"{files}?{fixture_query}", source)[0])
        require(entry["type"] == "file" and entry["size"] == len(source), f"bad fixture upload: {entry}")
        request("GET", f"{files}?{screenshot_query}", status=404)
        report["screenshot_absent_before_execution"] = True

        command = [
            args.cli, "run", "--sandbox", sandbox_id, "--exec-timeout", "45", "--",
            "node", "/opt/execlet-playwright/screenshot.cjs",
        ]
        with (out / "browser.json").open("w") as stdout, (out / "execletctl.log").open("w") as stderr:
            execution = subprocess.run(command, env=env, stdout=stdout, stderr=stderr, timeout=60)
        require(execution.returncode == 0, f"Chromium command exited {execution.returncode}; see {out / 'execletctl.log'}")
        browser = json.loads((out / "browser.json").read_bytes())
        require(browser["browser"] == "chromium" and browser["version"], f"unexpected browser: {browser}")
        require(
            browser["title"] == "Execlet browser QA" and browser["heading"] == "Execlet browser check",
            f"unexpected rendered page: {browser}",
        )

        png, content_type = request("GET", f"{files}?{screenshot_query}")
        require(content_type == "application/octet-stream", f"unexpected file media type: {content_type!r}")
        width, height = validate_png(png)
        digest = hashlib.sha256(png).hexdigest()
        require(browser["bytes"] == len(png) and browser["sha256"] == digest, "download differs from Chromium screenshot")
        (out / "screenshot.png").write_bytes(png)
        report["screenshot"] = dict(width=width, height=height, bytes=len(png), sha256=digest)
        completed = True
    finally:
        if sandbox_path:
            try:
                deleted = json.loads(request("DELETE", sandbox_path)[0])
                require(deleted["state"] == "killed", f"sandbox cleanup returned {deleted['state']!r}")
            except (OSError, HTTPException, RuntimeError, ValueError, KeyError) as exc:
                cleanup_errors.append(f"DELETE sandbox: {exc}")
        if server:
            try:
                if server.poll() is None:
                    server.terminate()
                status = server.wait(timeout=25)
                require(status == 0, f"execletd exited with status {status}")
            except subprocess.TimeoutExpired:
                server.kill()
                server.wait()
                cleanup_errors.append("execletd did not shut down within 25 seconds")
            except (OSError, RuntimeError) as exc:
                cleanup_errors.append(str(exc))
            try:
                remaining = output([*docker, "ps", "-aq", *owner_filter]).splitlines()
                report["containers_before_fallback"] = remaining
                if remaining:
                    cleanup_errors.append(f"daemon left {len(remaining)} owned container(s); fallback removal required")
                    output([*docker, "rm", "-fv", *remaining])
                    remaining = output([*docker, "ps", "-aq", *owner_filter]).splitlines()
                report["containers_remaining"] = remaining
                require(not remaining, f"containers still labeled {owner}: {remaining}")
            except (OSError, RuntimeError, subprocess.TimeoutExpired) as exc:
                cleanup_errors.append(str(exc))
        if build_attempted:
            try:
                if output([*docker, "image", "ls", "-q", "--filter", f"reference={temp_image}"]):
                    output([*docker, "image", "rm", "--no-prune", temp_image])
            except (OSError, RuntimeError, subprocess.TimeoutExpired) as exc:
                cleanup_errors.append(str(exc))
        report["cleanup_errors"] = cleanup_errors
        report["status"] = "passed" if completed and not cleanup_errors else "failed"
        (out / "result.json").write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
        if cleanup_errors:
            print("Cleanup failed: " + "; ".join(cleanup_errors), file=sys.stderr)
    return not cleanup_errors


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server", type=Path, default=ROOT / "bin/execletd", help="path to execletd")
    parser.add_argument("--cli", type=Path, default=ROOT / "bin/execletctl", help="path to execletctl")
    parser.add_argument("--image", help="use an already-built local Playwright preset instead of building it")
    parser.add_argument("--out", type=Path, default=ROOT / ".amp/in/artifacts", help="parent directory for per-run artifacts")
    args = parser.parse_args()
    args.server = args.server.resolve()
    args.cli = args.cli.resolve()
    owner = "execlet-playwright-" + uuid.uuid4().hex
    out = args.out.resolve() / owner
    out.mkdir(mode=0o700, parents=True)
    print(f"Artifacts: {out}", flush=True)
    try:
        if run(args, owner, out):
            print(f"PASS: verified Chromium screenshot: {out / 'screenshot.png'}")
            return 0
    except (OSError, HTTPException, RuntimeError, subprocess.TimeoutExpired, ValueError, KeyError) as exc:
        print(f"FAIL: {exc}", file=sys.stderr)
    return 1


if __name__ == "__main__":
    def interrupt(_signal, _frame):
        raise SystemExit(143)

    signal.signal(signal.SIGTERM, interrupt)
    sys.exit(main())
