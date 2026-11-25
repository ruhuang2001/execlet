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
from execlet import Client, CreateSandboxRequest, Error


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server", default="bin/execletd")
    parser.add_argument("--out", type=Path, default=Path("artifacts/templates"))
    parser.add_argument("--runtime", choices=("runc", "runsc"), default="runc")
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
                allowed_runtimes=[args.runtime],
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
        log = (args.out / "execletd.log").open("a")
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
                raise RuntimeError("execletd exited")
            try:
                client.list_sandboxes()
                return proc
            except Error:
                time.sleep(0.05)
        proc.terminate()
        proc.wait()
        raise RuntimeError("execletd readiness timed out")

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
            CreateSandboxRequest(image="template:" + name, memory_mb=256 if args.runtime == "runsc" else 128)
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
            "mkdir -p /project /tmp/cache /var/tmp/cache /.execlet; echo initial >/project/main; python3 -m venv /opt/project-env; echo private >/tmp/cache/token; echo secret >/var/tmp/cache/token; echo temporary >/mnt/scratch/token; echo ephemeral >/.execlet/snapshot-marker",
        )
        command(
            source,
            "rm /etc/alpine-release; rm -rf /etc/apk/keys; mkdir /etc/apk/keys; echo ours >/etc/apk/keys/only; ln /project/main /project/linked",
        )
        saved = template("save", name, "--sandbox", source, "--owner", owner)
        assert saved.returncode == 0, saved.stderr
        saved = json.loads(saved.stdout)
        assert command(source, "cat /project/main") == "initial"
        if args.runtime == "runsc":
            failed_name = name + "-failed"
            failed = template(
                "save",
                failed_name,
                "--sandbox",
                source,
                "--owner",
                owner,
                "--runsc-root",
                "/tmp/missing-runsc-root-" + owner,
            )
            assert (
                failed.returncode != 0
                and failed_name not in json.loads(policy.read_text())["templates"]
            )
            assert command(source, "cat /project/main") == "initial"
            rows.append(
                dict(operation="capture_failure", unpublished=True, source_running=True)
            )
            client.pause_sandbox(source)
            paused_name = name + "-paused"
            paused = template(
                "save", paused_name, "--sandbox", source, "--owner", owner
            )
            assert paused.returncode == 0, paused.stderr
            assert (
                subprocess.check_output(
                    [
                        "docker",
                        "inspect",
                        "--format",
                        "{{.State.Paused}}",
                        "execlet-" + source,
                    ],
                    text=True,
                ).strip()
                == "true"
            )
            assert template("delete", paused_name).returncode == 0
            client.resume_sandbox(source)
            assert command(source, "cat /project/main") == "initial"
            rows.append(
                dict(operation="preserve_source_state", running=True, paused=True)
            )
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
                    'cat /project/main; test /project/main -ef /project/linked; test ! -e /etc/alpine-release; test "$(ls /etc/apk/keys)" = only; /opt/project-env/bin/pip --version >/dev/null; test ! -e /tmp/cache/token; test ! -e /var/tmp/cache/token; test ! -e /mnt/scratch/token; test ! -e /.execlet/snapshot-marker',
                )
                == "initial"
            )
            rows.append(
                dict(
                    operation="isolated_clone",
                    id=sid,
                    digest=client.get_sandbox(sid).image,
                    project="initial",
                    deleted_base_file_absent=True,
                    replaced_base_directory_opaque=True,
                    hardlink_preserved=True,
                    pip="available",
                    excluded=[
                        "/tmp/cache",
                        "/var/tmp/cache",
                        "/mnt/scratch",
                        "/.execlet/snapshot-marker",
                    ],
                )
            )
        catalog = json.loads(policy.read_text())
        alias = name + "-copy"
        catalog["templates"][alias] = saved["image"]
        next_policy = policy.with_suffix(".tmp")
        next_policy.write_text(json.dumps(catalog))
        next_policy.replace(policy)
        tags = saved_image["RepoTags"]
        assert len(tags) == 1, tags
        tag = tags[0]
        removed = template("delete", name)
        assert removed.returncode == 0, removed.stderr
        try:
            create(name)
        except Error as err:
            assert err.code == "invalid_request", err
        else:
            raise AssertionError("removed template remained available")
        assert (
            subprocess.run(
                ["docker", "image", "inspect", tag], capture_output=True
            ).returncode
            == 0
        )
        live = create(alias)
        assert command(live, "cat /project/main") == "initial"
        client.kill_sandbox(live)
        removed_alias = template("delete", alias)
        assert removed_alias.returncode == 0, removed_alias.stderr
        try:
            assert (
                subprocess.run(
                    ["docker", "image", "inspect", tag], capture_output=True
                ).returncode
                != 0
            ), tag
        finally:
            subprocess.run(["docker", "image", "rm", tag], capture_output=True)
        rows.append(
            dict(
                operation="delete",
                released_snapshot_tag=tag,
                final_alias=alias,
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
        ["docker", "ps", "-aq", "--filter", "label=execlet.owner=" + owner]
    ).strip()
    (args.out / "report.json").write_text(
        json.dumps(
            {
                "status": "passed",
                "owner": owner,
                "runtime": args.runtime,
                "rows": rows,
                "remaining_containers": 0,
            },
            indent=2,
        )
        + "\n"
    )


if __name__ == "__main__":
    main()
