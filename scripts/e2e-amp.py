"""Run the offline repository repair in the installed Amp CLI, using its active login."""

import argparse
import hashlib
import json
import os
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import urllib.request
import uuid
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
IMAGE = "execlet-mcp:local"
COMMAND = [
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


def copy_fixture(work):
    subprocess.run(["git", "clone", "--quiet", "--local", "--no-checkout", ROOT, work], check=True)
    paths = subprocess.check_output(["git", "ls-files", "-coz", "--exclude-standard", "--", "sandbox-python"], cwd=ROOT)
    for raw in set(filter(None, paths.split(b"\0"))):
        path = Path(os.fsdecode(raw))
        src = ROOT / path
        if src.is_symlink() or src.is_file():
            dest = work / path
            dest.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(src, dest, follow_symlinks=False)


def run(out, tmp):
    def command(*args, **kw):
        return subprocess.check_output(args, text=True, **kw).strip()

    out.mkdir(parents=True, exist_ok=True)
    work = tmp / "checkout"
    copy_fixture(work)
    source = work / "sandbox-python/src/execlet/e2b.py"
    correct = source.read_text()
    broken = correct.replace("round(value * 1000)", "round(value * 100)", 1)
    assert broken != correct
    source.write_text(broken)
    image = command("docker", "image", "inspect", IMAGE, "--format", "{{.Id}}")
    owner = "amp-e2e-" + uuid.uuid4().hex
    with socket.socket() as listen:
        listen.bind(("127.0.0.1", 0))
        address = "127.0.0.1:" + str(listen.getsockname()[1])
    domain = "http://" + address
    transcript = out / "mcp.jsonl"
    transcript.write_text("")
    settings = tmp / "settings.json"
    settings.write_text(
        json.dumps(
            {
                "amp.mcpServers": {
                    "execlet": {
                        "command": sys.executable,
                        "args": [
                            str(ROOT / "scripts/amp-e2e/record_mcp.py"),
                            str(transcript),
                            str(ROOT / "bin/execletctl"),
                            "mcp",
                            "--workspace",
                            str(work),
                            "--image",
                            image,
                        ],
                        "env": {
                            "SANDBOX_DOMAIN": domain,
                            "SANDBOX_API_KEY": "",
                            "XDG_CACHE_HOME": str(tmp / "cache"),
                        },
                    }
                },
                "amp.tools.enable": ["mcp__execlet__*", "apply_patch"],
            }
        )
    )
    prompt = (
        "Use the configured execlet MCP tools to repair an intentionally broken Python unit in this temporary "
        "checkout. Create a sandbox with default network and sync_workspace. Run exactly this argv with the run_command "
        "MCP tool and observe the pytest failure: "
        + json.dumps(COMMAND)
        + ". In the HOST checkout edit only "
        "sandbox-python/src/execlet/e2b.py: change round(value * 100) back to round(value * 1000). "
        "Use your host editing tool for that edit. Sync the checkout incrementally with reset=false; only one changed file should upload. Run the same pytest argv using the "
        "MCP server again, verify it passes and destroy_sandbox. Do not run tests on the host or use Docker directly. "
        "Make no commits. Stop after reporting the failed and passed exit codes and cleanup."
    )
    with (out / "daemon.log").open("w") as log:
        daemon = subprocess.Popen(
            [ROOT / "bin/execletd", "-owner", owner, "-addr", address],
            stdout=log,
            stderr=log,
            env={**os.environ, "SANDBOX_API_KEY": ""},
        )
        try:
            for _ in range(100):
                try:
                    with urllib.request.urlopen(domain + "/readyz") as r:
                        if r.status == 200:
                            break
                except OSError:
                    time.sleep(0.05)
            cli = [
                "amp",
                "--settings-file",
                str(settings),
                "--log-file",
                str(tmp / "amp.log"),
                "--executor",
                "local",
                "--visibility",
                "private",
                "--mode",
                "low",
            ]
            tools = command(*cli, "tools", "list", cwd=work)
            (out / "tools.txt").write_text(
                "\n".join(x for x in tools.splitlines() if "mcp__execlet__" in x)
                + "\n"
            )
            with (
                (tmp / "stream.jsonl").open("w") as stream,
                (out / "amp-stderr.log").open("w") as err,
            ):
                run = subprocess.run(
                    [*cli, "--stream-json", "-x", prompt],
                    cwd=work,
                    stdout=stream,
                    stderr=err,
                    timeout=240,
                )
            events = []
            test_ids = set()
            visible_tests = []
            for line in (tmp / "stream.jsonl").read_text().splitlines():
                try:
                    ev = json.loads(line)
                except ValueError:
                    continue
                for block in (
                    ev.get("message", {}).get("content", [])
                    if isinstance(ev.get("message"), dict)
                    else []
                ):
                    if isinstance(block, dict) and block.get("type") == "tool_use":
                        events.append({"tool": block["name"], "id": block.get("id")})
                        if (
                            block["name"].endswith("__run_command")
                            and block.get("input", {}).get("command") == COMMAND
                        ):
                            test_ids.add(block["id"])
                    if (
                        isinstance(block, dict)
                        and block.get("type") == "tool_result"
                        and block.get("tool_use_id") in test_ids
                    ):
                        visible_tests.append(str(block.get("content", ""))[-2200:])
                if ev.get("type") == "result":
                    events.append(
                        {
                            "type": "result",
                            "success": not ev.get("is_error"),
                            "summary": str(ev.get("result", ""))[-700:],
                        }
                    )
            (out / "amp-events.json").write_text(json.dumps(events, indent=2) + "\n")
            assert run.returncode == 0, (
                run.returncode,
                (out / "amp-stderr.log").read_text()[-1200:],
                events[-1:],
            )
            rows = [json.loads(line) for line in transcript.read_text().splitlines()]
            rows = [row for row in rows if not row["error"]]
            created = next(e["value"] for e in rows if e["tool"] == "create_sandbox")
            syncs = [e["value"] for e in rows if e["tool"] == "sync_workspace"]
            runs = [
                e["value"]
                for e in rows
                if e["tool"] == "run_command" and e["arguments"]["command"] == COMMAND
            ]
            assert (
                created["network"] == "none"
                and len(syncs) >= 2
                and syncs[0]["uploaded"] > 1
                and syncs[-1]["uploaded"] == 1
            ), rows
            assert runs[0]["exit_code"] == 1 and "1 failed" in runs[0]["stdout"], runs[
                0
            ]
            assert runs[-1]["exit_code"] == 0 and "1 passed" in runs[-1]["stdout"], (
                runs[-1]
            )
            assert source.read_text() == correct and any(
                e.get("tool") == "apply_patch" for e in events
            )
            assert any("1 failed" in t for t in visible_tests) and any(
                "1 passed" in t for t in visible_tests
            ), visible_tests
            network = next(
                r["docker_network_mode"] for r in rows if r["tool"] == "create_sandbox"
            )
            assert network == "none", network
            (out / "visible-tests.json").write_text(
                json.dumps(visible_tests, indent=2) + "\n"
            )
            assert rows[-1]["tool"] == "destroy_sandbox", rows[-1]
            remaining = command(
                "docker", "ps", "-aq", "--filter", "label=execlet.owner=" + owner
            ).split()
            assert not remaining, remaining
            report = dict(
                amp=command("amp", "--version"),
                image=image,
                sandbox=created,
                docker_network_mode=network,
                repository="execlet/sandbox-python",
                base_commit=command("git", "-C", str(work), "rev-parse", "HEAD"),
                test=COMMAND[-1],
                exit_codes=[runs[0]["exit_code"], runs[-1]["exit_code"]],
                host_before_sha256=hashlib.sha256(broken.encode()).hexdigest(),
                host_after_sha256=hashlib.sha256(correct.encode()).hexdigest(),
                uploaded=[syncs[0]["uploaded"], syncs[-1]["uploaded"]],
                owned_containers_after=remaining,
            )
            (out / "acceptance.json").write_text(json.dumps(report, indent=2) + "\n")
        finally:
            daemon.terminate()
            daemon.wait(timeout=30)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", type=Path, default=Path("artifacts/amp"))
    args = parser.parse_args()
    with tempfile.TemporaryDirectory(prefix="ts-amp-") as tmp:
        run(args.out.resolve(), Path(tmp))
