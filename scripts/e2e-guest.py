"""Compare real Docker legacy and guest metadata, protection and create latency."""

import argparse
import json
import math
import socket
import statistics
import subprocess
import time
import uuid
from http.client import HTTPConnection
from pathlib import Path
from urllib.parse import urlencode


def run(args):
    root = Path(__file__).resolve().parents[1]
    args.out.mkdir(parents=True, exist_ok=True)
    report = {}
    for stage in ["fixture", "minimal"]:
        subprocess.run(
            [
                "docker",
                "build",
                "-f",
                str(root / "examples/guest/Dockerfile.agent-minimal"),
                "--target",
                stage,
                "-t",
                "tiny-sandbox-guest:" + stage,
                str(root / "examples/guest"),
            ],
            check=True,
        )
    expected = None
    for mode, stage in [("off", "fixture"), ("auto", "fixture"), ("auto", "minimal")]:
        owner = "guest-e2e-" + uuid.uuid4().hex
        with socket.socket() as allocation:
            allocation.bind(("127.0.0.1", 0))
            port = allocation.getsockname()[1]

        def request(method, path, body=None):
            conn = HTTPConnection("127.0.0.1", port, timeout=20)
            try:
                conn.request(
                    method,
                    path,
                    json.dumps(body) if body is not None else None,
                    {"Content-Type": "application/json"},
                )
                res = conn.getresponse()
                value = json.loads(res.read())
                assert 200 <= res.status < 300, (res.status, value)
                return value
            finally:
                conn.close()

        with (args.out / f"{mode}-{stage}.log").open("w") as log:
            server = subprocess.Popen(
                [
                    str(args.server.resolve()),
                    "-addr",
                    f"127.0.0.1:{port}",
                    "-owner",
                    owner,
                    "-guest-agent",
                    mode,
                ],
                stdout=log,
                stderr=log,
            )
            try:
                for _ in range(150):
                    try:
                        request("GET", "/readyz")
                        break
                    except OSError:
                        time.sleep(0.05)
                latencies = []
                for sample in range(args.samples if stage == "fixture" else 1):
                    started = time.monotonic_ns()
                    sb = request(
                        "POST",
                        "/v1/sandboxes",
                        {"image": "tiny-sandbox-guest:" + stage, "network": "none"},
                    )
                    latencies.append((time.monotonic_ns() - started) / 1e6)
                    base = "/v1/sandboxes/" + sb["id"]
                    try:
                        assert sb["agent_mode"] == (
                            "legacy" if mode == "off" else "guest"
                        ), sb
                        assert sb["protected"] == (mode == "auto"), sb
                        if sample == 0:
                            entries, cursor = [], ""
                            while True:
                                page = request(
                                    "GET",
                                    base
                                    + "/files/list?"
                                    + urlencode(
                                        dict(path="/fixture", limit=2, cursor=cursor)
                                    ),
                                )
                                entries.extend(page["entries"])
                                cursor = page.get("next_cursor")
                                if not cursor:
                                    break
                            assert entries[0]["size"] == 0, entries
                            assert [e["name"] for e in entries] == [
                                "dir",
                                "link",
                                "regular",
                            ], entries
                            stats = [
                                request(
                                    "GET",
                                    base
                                    + "/files/stat?"
                                    + urlencode({"path": "/fixture/" + entry["name"]}),
                                )
                                for entry in entries
                            ]
                            assert stats == entries, (stats, entries)
                            if expected is None:
                                expected = entries
                            assert expected == entries, (expected, entries)
                    finally:
                        request("DELETE", base)
                report[f"{mode}-{stage}"] = dict(
                    entries=entries,
                    stat=stats,
                    agent_mode=sb["agent_mode"],
                    protected=sb["protected"],
                    fresh_container_local_image=dict(
                        samples_ms=latencies,
                        p50_ms=statistics.median(latencies),
                        p95_ms=sorted(latencies)[math.ceil(0.95 * len(latencies)) - 1],
                    ),
                )
                (args.out / "report.json").write_text(
                    json.dumps(report, indent=2) + "\n"
                )
            finally:
                server.terminate()
                server.wait(timeout=30)
                assert not subprocess.check_output(
                    [
                        "docker",
                        "ps",
                        "-aq",
                        "--filter",
                        "label=tiny-sandbox.owner=" + owner,
                    ]
                ).strip()


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server", type=Path, default=Path("bin/sandboxd"))
    parser.add_argument("--out", type=Path, default=Path("artifacts/guest"))
    parser.add_argument("--samples", type=int, default=30)
    run(parser.parse_args())
