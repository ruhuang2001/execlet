"""Check a candidate distribution and run its documented CLI tasks on the native host."""
import argparse
import json
import platform
import re
import shlex
import shutil
import subprocess
import sys
import tarfile
import tempfile
from pathlib import Path

from host_harness import Daemon, checksum, report


FILES = {"execletd", "execletctl", "LICENSE", "README.md", "examples/mcp/Dockerfile",
         "examples/mcp/README.md", "examples/mcp/demo/calculator.py", "examples/mcp/demo/test_calculator.py"}
PLATFORMS = {f"{os}_{arch}" for os in ("linux", "darwin") for arch in ("amd64", "arm64")}
ARCHIVE = re.compile(r"execlet_([0-9][\w.+-]*)_(linux|darwin)_(arm64|amd64)\.tar\.gz", re.ASCII)


def candidates(args, native):
    dist = args.archive.parent if args.archive else args.dist
    checksums = {}
    for line in (dist / "checksums.txt").read_text().splitlines():
        entry = re.fullmatch(r"([a-fA-F0-9]{64})\s+\*?(\S+)", line)
        if not entry or not ARCHIVE.fullmatch(entry[2]) or entry[2] in checksums:
            raise ValueError(f"invalid or duplicate checksum entry: {line!r}")
        checksums[entry[2]] = entry[1].lower()
    if args.archive:
        archive = args.archive
        match = ARCHIVE.fullmatch(archive.name)
        if not match or f"{match[2]}_{match[3]}" != native:
            raise ValueError(f"{archive.name} is not a native {native} release archive")
        version = match[1]
        files = [archive]
    else:
        metadata = json.loads((dist / "metadata.json").read_text())
        version = metadata.get("version") if isinstance(metadata, dict) else None
        if (not isinstance(version, str) or metadata.get("project_name") != "execlet"
                or not re.fullmatch(r"[0-9][\w.+-]*", version, re.ASCII)):
            raise ValueError("unexpected project or version in metadata.json")
        expected = {f"execlet_{version}_{p}.tar.gz" for p in PLATFORMS}
        files = sorted([*dist.glob("*.tar.gz"), *dist.glob("*.zip")])
        if set(checksums) != expected or {f.name for f in files} != expected:
            raise ValueError("archives and checksum entries must match all four release platforms")
        archive = dist / f"execlet_{version}_{native}.tar.gz"
    if args.expect_version and args.expect_version != version:
        raise ValueError(f"expected version {args.expect_version}, found {version}")
    for path in files:
        if checksums.get(path.name) != checksum(path):
            raise ValueError(f"missing or incorrect checksum for {path.name}")
    return archive, version, files


def binary_platform(header):
    if header.startswith(b"\x7fELF\x02\x01"):
        return {62: "linux_amd64", 183: "linux_arm64"}.get(int.from_bytes(header[18:20], "little"))
    if header.startswith(b"\xcf\xfa\xed\xfe"):
        return {0x01000007: "darwin_amd64", 0x0100000C: "darwin_arm64"}.get(int.from_bytes(header[4:8], "little"))
    return None


def check_members(path, destination=None):
    seen = set()
    dirs = {str(p) for name in FILES for p in Path(name).parents}
    match = ARCHIVE.fullmatch(path.name)
    target_platform = f"{match[2]}_{match[3]}"
    with tarfile.open(path, "r:gz") as archive:
        for member in archive:
            name = member.name.removeprefix("./")
            if member.isdir() and (name or ".") in dirs:
                continue
            if name not in FILES or name in seen or not member.isreg():
                raise ValueError(f"{path.name}: unexpected member, type or duplicate: {member.name!r}")
            if member.size <= 0 or (name in ("execletctl", "execletd") and not member.mode & 0o100):
                raise ValueError(f"{path.name}: empty release member or non-executable binary: {name}")
            if name in ("execletctl", "execletd"):
                with archive.extractfile(member) as binary:
                    if binary_platform(binary.read(20)) != target_platform:
                        raise ValueError(f"{path.name}: {name} is not a {target_platform} binary")
            seen.add(name)
            if destination:
                target = destination / name
                target.parent.mkdir(parents=True, exist_ok=True)
                with archive.extractfile(member) as src, target.open("xb") as out:
                    shutil.copyfileobj(src, out)
                target.chmod(member.mode & 0o755)
    if seen != FILES:
        raise ValueError(f"{path.name}: missing release members: {sorted(FILES - seen)}")


FENCE = re.compile(r"^```(?:sh|bash)\s*\n(.*?)^```[ \t]*$", re.M | re.S)
TICKS = r"`([^`]+)`"


def promise(prose):
    prose = " ".join(prose.split())
    stdout = re.search(r"(?:expected stdout:|prints?)\s*((?:`[^`]+`(?:,?\s*(?:and\s+)?)?)+)", prose, re.I)
    stderr = re.search(r"(?:on stderr containing|reports)\s+" + TICKS, prose, re.I)
    status = re.search(r"(?:exits? (?:with )?(?:status |code )?|exit status )`(\d+)`", prose, re.I)
    if not any((stdout, stderr, status)):
        return None
    return (re.findall(TICKS, stdout[1]) if stdout else None,
            stderr[1] if stderr else None, int(status[1]) if status else 0)


def examples(path):
    text = path.read_text()
    for match in FENCE.finditer(text):
        block = match[1].strip()
        if not re.search(r"\bexecletctl\s+run\b", block):
            continue
        after = re.split(r"\n\s*\n", text[match.end():].strip(), maxsplit=1)[0]
        before = re.split(r"\n\s*\n", text[:match.start()].strip())[-1]
        expected = promise(after) or promise(before)
        if expected is None:
            raise ValueError(f"{path.name}:{text.count(chr(10), 0, match.start()) + 1}: no adjacent output or exit expectation")
        yield block, expected


def verify_example(block, expected, directory, daemon):
    lines = block.splitlines()
    env = dict(daemon.env, HOME=str(directory.parent))
    while lines and lines[0] in ('cd "$HOME/execlet-quickstart"',
                                 'export SANDBOX_DOMAIN=http://127.0.0.1:8080', './execletctl doctor'):
        if lines.pop(0) == './execletctl doctor':
            result = subprocess.run([directory / 'execletctl', 'doctor'], cwd=directory, env=env,
                                    capture_output=True, text=True, timeout=120)
            if result.returncode != 0 or result.stdout.splitlines()[-1:] != ['Ready']:
                raise ValueError(f"doctor did not finish with Ready (exit {result.returncode}): "
                                 f"{result.stdout[-400:]!r}, {result.stderr[-400:]!r}")
    if lines[-1:] == ['echo "Exit code: $?"']:
        lines.pop()
    argv = shlex.split("\n".join(lines).removeprefix('SANDBOX_DOMAIN=http://127.0.0.1:8080 '))
    flags = argv[2:-3]
    timed = len(flags) == 3 and flags[:2] == ['--rm', '--exec-timeout'] and flags[2].isdigit()
    if argv[:2] != ['./execletctl', 'run'] or argv[-3:-1] != ['--', 'code'] or (flags != ['--rm'] and not timed):
        raise ValueError(f"unsupported quickstart command: {block}")
    result = subprocess.run(argv, cwd=directory, env=env, capture_output=True, text=True, timeout=180)
    stdout, stderr, status = expected
    if ((stdout is not None and result.stdout.splitlines() != stdout)
            or (stderr is not None and stderr not in result.stderr) or result.returncode != status
            or (stderr == 'timeout' and 'completed (timeout)' not in result.stderr)):
        raise ValueError(f"{block}: expected {expected!r}; got exit {result.returncode}, "
                         f"stdout {result.stdout[-400:]!r}, stderr {result.stderr[-400:]!r}")
    daemon.require_empty()
    return {"command": argv, "stdout": result.stdout, "stderr": result.stderr[-400:], "exit": result.returncode}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    group = parser.add_mutually_exclusive_group()
    group.add_argument("--dist", type=Path, default=Path("dist"))
    group.add_argument("--archive", type=Path, help="native archive with adjacent checksums.txt")
    parser.add_argument("--docs", type=Path, action="append", default=[])
    parser.add_argument("--expect-version")
    parser.add_argument("--out", type=Path, default=Path("artifacts/release-preflight.json"))
    args = parser.parse_args()
    with report(args.out) as record, tempfile.TemporaryDirectory(prefix="execlet-preflight-") as tmp:
        if not args.docs:
            raise ValueError("pass at least one --docs guide")
        arch = {"aarch64": "arm64", "x86_64": "amd64"}.get(platform.machine(), platform.machine())
        native = f"{platform.system().lower()}_{arch}"
        if native not in PLATFORMS:
            raise ValueError(f"unsupported host: {native}")
        archive, version, files = candidates(args, native)
        package = Path(tmp) / "package"
        for path in files:
            check_members(path, package if path == archive else None)
        directory = Path(tmp) / "execlet-quickstart"
        directory.mkdir()
        for binary, flag in (("execletctl", "version"), ("execletd", "-version")):
            shutil.copy2(package / binary, directory / binary)
            actual = subprocess.check_output([directory / binary, flag], cwd=directory, text=True, timeout=10).strip()
            if actual != version:
                raise ValueError(f"{binary}: expected {version}, found {actual}")
        record.update(platform=native, version=version, archive=archive.name, checksums_verified=len(files), examples=[])
        daemon = Daemon(directory / "execletd", tmp)
        record["owner"] = daemon.owner
        with daemon.running():
            for path in (package / "README.md", *args.docs):
                cases = list(examples(path))
                if not cases:
                    raise ValueError(f"{path.name}: no execletctl run example")
                for block, expected in cases:
                    row = verify_example(block, expected, directory, daemon)
                    record["examples"].append(dict(row, document=path.name))
        record["owned_containers"] = 0
    print(f"{archive.name}: {len(files)} checksum(s), {len(record['examples'])} examples passed; {args.out}")


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, RuntimeError, subprocess.SubprocessError, tarfile.TarError) as error:
        sys.exit(f"release-preflight: {error}")
