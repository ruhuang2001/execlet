"""Verify template isolation, durable registration, snapshot exclusions and deletion on Docker."""

import argparse
import json
import socket
import subprocess
import sys
import time
import uuid
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "sandbox-python/src"))
from tiny_sandbox import Client, CreateSandboxRequest, Error


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server", default="bin/sandboxd")
    parser.add_argument("--out", type=Path, default=Path("artifacts/templates"))
    args = parser.parse_args()
    args.out.mkdir(parents=True, exist_ok=True)
    policy = args.out / "policy.json"
    owner = "template-e2e-" + uuid.uuid4().hex
    staged = subprocess.check_output(
        ["docker", "create", "--network", "none", "python:3.12-alpine"], text=True
    ).strip()
    try:
        digest = subprocess.check_output(
            ["docker", "commit", "--change", 'VOLUME ["/mnt/scratch"]', staged],
            text=True,
        ).strip()
    finally:
        subprocess.run(["docker", "rm", "-v", staged], check=True, capture_output=True)
    policy.write_text(
        json.dumps(
            dict(
                templates={"python": digest},
                allowed_images=[digest],
                allowed_networks=["none"],
                max_sandboxes=5,
                total_memory_mb=1280,
                total_pids=5120,
                total_nano_cpus=5000000000,
            )
        )
    )
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        port = sock.getsockname()[1]
    base = "http://127.0.0.1:" + str(port)
    client = Client(domain=base)
    rows = []

    def start():
        log = (args.out / "sandboxd.log").open("a")
        proc = subprocess.Popen(
            [
                args.server,
                "-addr",
                base.split("//")[1],
                "-policy",
                str(policy),
                "-owner",
                owner,
            ],
            stdout=log,
            stderr=log,
        )
        log.close()
        for _ in range(200):
            if proc.poll() is not None:
                raise RuntimeError("sandboxd exited")
            try:
                client.list_sandboxes()
                return proc
            except Error:
                time.sleep(0.05)
        proc.terminate()
        proc.wait()
        raise RuntimeError("sandboxd readiness timed out")

    def template(op, name, *extra):
        return subprocess.run(
            [
                sys.executable,
                str(ROOT / "scripts/template-admin.py"),
                op,
                "--policy",
                str(policy),
                "--name",
                name,
                *extra,
            ],
            text=True,
            capture_output=True,
        )

    def create(name):
        sb = client.create_sandbox(
            CreateSandboxRequest(image="template:" + name, memory_mb=128)
        )
        assert sb.image.startswith("sha256:"), sb
        return sb.id

    def command(sid, script):
        result = client.exec(sid, ["sh", "-ec", script])
        assert result.exit_code == 0, result
        return result.stdout.strip()

    name = "project-" + uuid.uuid4().hex[:12]
    proc = start()
    try:
        source = create("python")
        command(
            source,
            "mkdir -p /project /tmp/cache /var/tmp/cache /.tiny-sandbox; echo initial >/project/main; python3 -m venv /opt/project-env; echo private >/tmp/cache/token; echo secret >/var/tmp/cache/token; echo temporary >/mnt/scratch/token; echo ephemeral >/.tiny-sandbox/snapshot-marker",
        )
        saved = template("save", name, "--sandbox", source, "--owner", owner)
        assert saved.returncode == 0, saved.stderr
        saved = json.loads(saved.stdout)
        assert (
            template("save", name, "--sandbox", source, "--owner", owner).returncode
            != 0
        )
        rows.append(dict(operation="save", digest=saved["image"], saved=saved["name"]))
        saved_image = json.loads(
            subprocess.check_output(["docker", "image", "inspect", saved["image"]])
        )[0]
        assert saved_image["Config"]["Volumes"] == {"/mnt/scratch": {}}
        rows.append(
            dict(
                operation="cloned_volume_declaration",
                paths=list(saved_image["Config"]["Volumes"]),
            )
        )
        client.kill_sandbox(source)
        proc.terminate()
        proc.wait(timeout=30)
        proc = start()
        a, b = create(name), create(name)
        command(
            a,
            "echo changed >/project/main; /opt/project-env/bin/pip uninstall -y pip; test ! -e /opt/project-env/bin/pip",
        )
        for sid in [b, create(name)]:
            assert (
                command(
                    sid,
                    "cat /project/main; /opt/project-env/bin/pip --version >/dev/null; test ! -e /tmp/cache/token; test ! -e /var/tmp/cache/token; test ! -e /mnt/scratch/token; test ! -e /.tiny-sandbox/snapshot-marker",
                )
                == "initial"
            )
            rows.append(
                dict(
                    operation="isolated_clone",
                    id=sid,
                    digest=client.get_sandbox(sid).image,
                    project="initial",
                    pip="available",
                    excluded=[
                        "/tmp/cache",
                        "/var/tmp/cache",
                        "/mnt/scratch",
                        "/.tiny-sandbox/snapshot-marker",
                    ],
                )
            )
        removed = template("delete", name)
        assert removed.returncode == 0, removed.stderr
        try:
            create(name)
        except Error as err:
            assert err.code == "invalid_request", err
        else:
            raise AssertionError("removed template remained available")
        rows.append(
            dict(
                operation="delete",
                snapshot=json.loads(removed.stdout),
                retained_sandbox=client.get_sandbox(b).state,
            )
        )
    finally:
        proc.terminate()
        proc.wait(timeout=40)
        if name in json.loads(policy.read_text()).get("templates", {}):
            template("delete", name)
        subprocess.run(
            ["docker", "image", "rm", digest], check=True, capture_output=True
        )
    assert not subprocess.check_output(
        ["docker", "ps", "-aq", "--filter", "label=tiny-sandbox.owner=" + owner]
    ).strip()
    (args.out / "report.json").write_text(
        json.dumps(
            {
                "status": "passed",
                "owner": owner,
                "rows": rows,
                "remaining_containers": 0,
            },
            indent=2,
        )
        + "\n"
    )


if __name__ == "__main__":
    main()
