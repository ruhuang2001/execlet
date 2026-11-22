"""Publish and retire local sandbox filesystem snapshots in an administrator policy."""

import argparse
import fcntl
import json
import os
import re
import stat
import subprocess
import sys
import tarfile
import tempfile
import uuid
from contextlib import ExitStack
from pathlib import Path, PurePosixPath

NAME = re.compile(r"[a-z0-9][a-z0-9._-]{0,63}")
TRANSIENT_DIRS = {"tmp", "var/tmp", "run", "dev", "proc", "sys"}
EXCLUDED = {
    ".tiny-sandbox",
    ".dockerenv",
    "etc/hostname",
    "etc/hosts",
    "etc/resolv.conf",
}


def docker(*args):
    return subprocess.check_output(["docker", *args], text=True).strip()


def inspect(kind, ref):
    return json.loads(docker(kind, "inspect", ref))[0]


def archive_name(value):
    path = PurePosixPath(value)
    if path.is_absolute() or ".." in path.parts:
        raise ValueError(f"invalid export archive path {value!r}")
    return str(path)


def exclude(path, roots):
    return any(path == root or path.startswith(root + "/") for root in roots)


def filtered_export(container, destination, mounts):
    roots = EXCLUDED | TRANSIENT_DIRS | mounts
    with subprocess.Popen(
        ["docker", "export", container], stdout=subprocess.PIPE
    ) as proc:
        try:
            with (
                tarfile.open(fileobj=proc.stdout, mode="r|") as source,
                tarfile.open(destination, mode="w") as output,
            ):
                for item in source:
                    name = archive_name(item.name)
                    if exclude(name, roots):
                        if name in TRANSIENT_DIRS | mounts and item.isdir():
                            output.addfile(item)
                        continue
                    if item.islnk() and exclude(archive_name(item.linkname), roots):
                        raise ValueError(
                            f"hard link {name!r} points into an excluded directory"
                        )
                    output.addfile(
                        item, source.extractfile(item) if item.isreg() else None
                    )
        finally:
            proc.stdout.close()
        if proc.wait() != 0:
            raise RuntimeError("Docker filesystem export failed")


def image_settings(base, name):
    cfg = base["Config"]
    settings = ["LABEL tiny-sandbox.template=" + name]
    for key in ["User", "WorkingDir"]:
        if cfg.get(key):
            settings.append(("USER " if key == "User" else "WORKDIR ") + cfg[key])
    for var in cfg.get("Env") or []:
        key, _, value = var.partition("=")
        if not re.fullmatch("[A-Za-z_][A-Za-z0-9_]*", key) or "\n" in value:
            raise ValueError("unsupported multiline or non-POSIX image environment")
        value = json.dumps(value, ensure_ascii=False).replace("$", "\\$")
        settings.append("ENV " + key + "=" + value)
    for key in ["Cmd", "Entrypoint"]:
        if cfg.get(key):
            settings.append(key.upper() + " " + json.dumps(cfg[key]))
    if cfg.get("Volumes"):
        settings.append("VOLUME " + json.dumps(sorted(cfg["Volumes"])))
    args = [
        "--platform",
        "/".join(filter(None, [base["Os"], base["Architecture"], base.get("Variant")])),
    ]
    for change in settings:
        args.extend(["--change", change])
    return args


def publish(args, cleanup):
    if not re.fullmatch(r"sbx_[a-zA-Z0-9_-]+", args.sandbox):
        raise ValueError("expected a sandbox ID")
    source = inspect("container", "tiny-sandbox-" + args.sandbox)
    if (source["Config"].get("Labels") or {}).get("tiny-sandbox.owner") != args.owner:
        raise ValueError("source belongs to a different deployment owner")
    if not source["State"]["Running"]:
        raise ValueError("source sandbox must be running or paused")
    if source["HostConfig"].get("Runtime") == "runsc":
        raise ValueError(
            "runsc snapshots are unsupported: Docker export can omit guest filesystem changes"
        )
    base = inspect("image", source["Image"])
    settings = image_settings(base, args.name)
    mounts = {m["Destination"].strip("/") for m in source.get("Mounts", [])}
    tag = "tiny-sandbox-template/" + args.name + ":snapshot-" + uuid.uuid4().hex
    with (
        ExitStack() as staging,
        tempfile.TemporaryDirectory(prefix="tiny-sandbox-template-") as work,
    ):
        snapshot = docker("commit", source["Id"])
        staging.callback(docker, "image", "rm", snapshot)
        stopped = docker("create", "--network", "none", snapshot, "/bin/true")
        staging.callback(docker, "rm", "-v", stopped)
        archive = Path(work) / "filesystem.tar"
        filtered_export(stopped, archive, mounts)
        digest = docker("import", *settings, str(archive), tag)
        cleanup.callback(docker, "image", "rm", tag)
    return digest


def persist(path, policy, *, published=None):
    fd, tmp = tempfile.mkstemp(prefix=".template-", dir=path.parent)
    try:
        with os.fdopen(fd, "w") as out:
            os.fchmod(out.fileno(), stat.S_IMODE(path.stat().st_mode))
            json.dump(policy, out, indent=2)
            out.write("\n")
            out.flush()
            os.fsync(out.fileno())
        os.replace(tmp, path)
        if published is not None:
            published.pop_all()
        directory = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        Path(tmp).unlink(missing_ok=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    subs = parser.add_subparsers(dest="action", required=True)
    for action in ["save", "delete"]:
        sub = subs.add_parser(action)
        sub.add_argument("--policy", type=Path, required=True)
        sub.add_argument("--name", required=True)
        if action == "save":
            sub.add_argument("--owner", required=True)
            sub.add_argument("--sandbox", required=True)
    args = parser.parse_args()
    if not NAME.fullmatch(args.name):
        parser.error("invalid template name")
    path = args.policy.resolve(strict=True)
    with Path(str(path) + ".lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        policy = json.loads(path.read_text())
        names = policy.setdefault("templates", {})
        if args.action == "save":
            if args.name in names:
                parser.error("template name already exists")
            with ExitStack() as rollback:
                digest = publish(args, rollback)
                names[args.name] = digest
                persist(path, policy, published=rollback)
        else:
            if args.name not in names:
                parser.error("unknown template name")
            digest = names.pop(args.name)
            persist(path, policy)
            if digest not in names.values() and digest not in policy.get(
                "allowed_images", []
            ):
                result = subprocess.run(
                    ["docker", "image", "inspect", digest],
                    text=True,
                    capture_output=True,
                )
                if result.returncode == 0:
                    image = json.loads(result.stdout)[0]
                    if (image["Config"].get("Labels") or {}).get(
                        "tiny-sandbox.template"
                    ) == args.name:
                        for tag in image.get("RepoTags") or []:
                            if tag.startswith(
                                "tiny-sandbox-template/" + args.name + ":snapshot-"
                            ):
                                docker("image", "rm", "--force", tag)
    print(json.dumps(dict(name=args.name, image=digest, action=args.action)))


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, RuntimeError, subprocess.CalledProcessError) as exc:
        sys.exit(str(exc))
