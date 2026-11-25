"""Measure a failing-test repair through execletctl mcp in an offline sandbox."""
import argparse
import contextlib
import errno
import json
import math
import os
import select
import shlex
import shutil
import stat
import statistics
import subprocess
import sys
import tempfile
import time
from pathlib import Path

from host_harness import Daemon, checksum, report


FIXTURE = Path(__file__).resolve().parents[1] / "examples/mcp/demo"


def copy_regular(root, rel, dest):
    with contextlib.ExitStack() as stack:
        fd = root
        try:
            for part in rel.parts[:-1]:
                fd = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd)
                stack.callback(os.close, fd)
            fd = os.open(rel.name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=fd)
        except OSError as error:
            if error.errno in (errno.ENOENT, errno.ELOOP, errno.ENOTDIR):
                return
            raise
        stack.callback(os.close, fd)
        mode = os.fstat(fd).st_mode
        if not stat.S_ISREG(mode):
            return
        target = dest / rel
        target.parent.mkdir(parents=True, exist_ok=True)
        with os.fdopen(fd, "rb", closefd=False) as src, target.open("xb") as out:
            shutil.copyfileobj(src, out)
        target.chmod(0o700 if mode & 0o111 else 0o600)


def copy_workspace(origin, dest, *, team):
    env = {k: v for k, v in os.environ.items() if not k.startswith("GIT_")}
    if team:
        git = ["git", "-C", origin, "-c", "core.fsmonitor=false"]
        root = subprocess.check_output([*git, "rev-parse", "--show-toplevel"], env=env, text=True, timeout=10)
        if Path(root.rstrip("\n")).resolve() != origin:
            raise ValueError("--workspace must be the root of a Git checkout")
        raw = subprocess.check_output([*git, "ls-files", "-coz", "--exclude-standard"], env=env, timeout=60)
        paths = {Path(os.fsdecode(p)) for p in raw.split(b"\0") if p}
    else:
        paths = [Path("calculator.py"), Path("test_calculator.py")]
    dest.mkdir(mode=0o700)
    root_fd = os.open(origin, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        for rel in paths:
            if not rel.is_absolute() and not ({p.casefold() for p in rel.parts} & {"..", ".git", ".venv", "node_modules", ".env"}):
                copy_regular(root_fd, rel, dest)
    finally:
        os.close(root_fd)
    if team:
        env.update(GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL=os.devnull)
        for args in (["init", "-q", "--template="], ["add", "--intent-to-add", "--force", "."]):
            subprocess.run(["git", "-C", dest, *args], env=env, check=True, timeout=60)


def fix_path(workspace, value):
    rel = Path(value)
    if rel.is_absolute() or {p.casefold() for p in rel.parts} & {"..", ".git"}:
        raise ValueError("--fix-file must be relative to the private workspace copy")
    path = workspace / rel
    if path.resolve() != path or not path.is_file():
        raise ValueError("--fix-file must be a regular, synced file in the workspace copy")
    return path


class Session:
    def __init__(self, proc, timeout):
        self.proc, self.timeout = proc, timeout
        self.identifier = 0
        self.buffer = bytearray()
        os.set_blocking(proc.stdin.fileno(), False)

    def request(self, method, params=None, *, notify=False, deadline=None):
        deadline = deadline if deadline is not None else time.monotonic() + self.timeout
        message = {"jsonrpc": "2.0", "method": method}
        if params is not None:
            message["params"] = params
        if not notify:
            self.identifier += 1
            message["id"] = self.identifier
        data = memoryview((json.dumps(message) + "\n").encode())
        while data:
            if not select.select([], [self.proc.stdin], [], max(0, deadline - time.monotonic()))[1]:
                raise TimeoutError(f"MCP {method}: request write timed out")
            data = data[os.write(self.proc.stdin.fileno(), data):]
        if notify:
            return
        while time.monotonic() < deadline:
            if len(self.buffer) > 2 << 20:
                raise ValueError(f"MCP {method}: oversized response")
            if b"\n" not in self.buffer:
                if not select.select([self.proc.stdout], [], [], max(0, deadline - time.monotonic()))[0]:
                    break
                chunk = os.read(self.proc.stdout.fileno(), 65536)
                if not chunk:
                    raise RuntimeError(f"MCP {method}: unexpected EOF")
                self.buffer.extend(chunk)
                continue
            line, _, self.buffer = self.buffer.partition(b"\n")
            reply = json.loads(line)
            if "id" not in reply and reply.get("method", "").startswith("notifications/"):
                continue
            if reply.get("jsonrpc") != "2.0" or reply.get("id") != self.identifier or "result" not in reply:
                raise ValueError(f"MCP {method}: invalid or failed response: {line[:300]!r}")
            return reply["result"]
        raise TimeoutError(f"MCP {method}: incomplete response at deadline")

    def call(self, name, *, deadline=None, **args):
        result = self.request("tools/call", {"name": name, "arguments": args}, deadline=deadline)
        if result.get("isError") or not isinstance(result.get("structuredContent"), dict):
            raise RuntimeError(f"MCP {name}: {str(result.get('content'))[:400]}")
        return result["structuredContent"]

    def test(self, sid, command):
        deadline = time.monotonic() + self.timeout
        row = self.call("run_command", sandbox_id=sid, command=command, deadline=deadline)
        while row.get("state") == "running" and time.monotonic() < deadline:
            time.sleep(0.2)
            row = self.call("get_execution", sandbox_id=sid, execution_id=row["execution_id"], deadline=deadline)
        if (row.get("state") != "completed" or row.get("termination_reason") != "exited"
                or type(row.get("exit_code")) is not int):
            raise RuntimeError(f"test did not return a normal exit: {row.get('state')}, {row.get('termination_reason')}")
        return row


@contextlib.contextmanager
def connect(cli, workspace, image, daemon, home, timeout):
    env = {k: v for k, v in daemon.env.items() if not k.startswith("GIT_")}
    env.update(HOME=str(home), XDG_CACHE_HOME=str(home / ".cache"), XDG_CONFIG_HOME=str(home / ".config"),
               GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL=os.devnull)
    with (home / "mcp.log").open("ab") as log:
        proc = subprocess.Popen([cli, "mcp", "--workspace", workspace, "--image", image], env=env,
                                stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=log, bufsize=0)
        try:
            session = Session(proc, timeout)
            session.request("initialize", {"protocolVersion": "2024-11-05", "capabilities": {},
                                          "clientInfo": {"name": "execlet-pilot", "version": "1"}})
            session.request("notifications/initialized", notify=True)
            yield session
            proc.stdin.close()
            if proc.wait(timeout=10) != 0:
                raise RuntimeError(f"MCP exited with status {proc.returncode}")
        finally:
            if proc.poll() is None:
                proc.kill()
            proc.wait(timeout=10)
            proc.stdin.close()
            proc.stdout.close()


def check_sync(row, uploaded):
    count = row.get("uploaded")
    if (type(count) is not int or (count < 1 if uploaded is None else count != uploaded)
            or type(row.get("deleted")) is not int or row["deleted"] != 0):
        raise ValueError(f"unexpected sync: uploaded={count}, deleted={row.get('deleted')}; expected "
                         f"{uploaded if uploaded is not None else 'at least one'} upload(s) and no deletes")
    return count


@contextlib.contextmanager
def offline_sandbox(session, daemon, row):
    start = time.monotonic()
    created = session.call("create_sandbox")
    sid = created["sandbox_id"]
    row.update(sandbox=sid, create_ms=round((time.monotonic() - start) * 1000, 1))
    try:
        containers = daemon.owned(sid)
        if (created.get("network") != "none" or len(containers) != 1 or
                daemon.docker("inspect", "--format", "{{.HostConfig.NetworkMode}}", containers[0]) != "none"):
            raise RuntimeError("did not find exactly one offline container for the new sandbox")
        row["network"] = "none"
        yield sid, start
    finally:
        failing = sys.exc_info()[0] is not None
        try:
            result = session.call("destroy_sandbox", sandbox_id=sid, deadline=time.monotonic() + min(10, session.timeout))
            if result.get("state") != "killed":
                raise RuntimeError("destroy not confirmed")
            daemon.require_empty()
        except Exception as error:
            row["cleanup_error"] = str(error)
            if not failing:
                raise


def repair(session, sid, start, source, fixed, test, marker, row):
    row["initial_uploaded"] = check_sync(session.call("sync_workspace", sandbox_id=sid), None)
    failing = session.test(sid, test)
    if failing["exit_code"] <= 0 or not any(marker in failing[stream] for stream in ("stdout", "stderr")):
        raise RuntimeError("test did not fail with the expected diagnostic before the fix")
    row.update(failing_exit=failing["exit_code"], first_result_ms=round((time.monotonic() - start) * 1000, 1))
    source.write_bytes(fixed)
    row["fixed_uploaded"] = check_sync(session.call("sync_workspace", sandbox_id=sid), 1)
    passing = session.test(sid, test)
    if passing["exit_code"] != 0:
        raise RuntimeError(f"test still fails after the host edit (exit {passing['exit_code']})")
    row.update(passing_exit=0, repair_ms=round((time.monotonic() - start) * 1000, 1))
    row["unchanged_uploaded"] = check_sync(session.call("sync_workspace", sandbox_id=sid), 0)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cli", type=Path, default=Path("bin/execletctl"))
    parser.add_argument("--server", type=Path, default=Path("bin/execletd"))
    parser.add_argument("--image", required=True)
    group = parser.add_mutually_exclusive_group()
    group.add_argument("--workspace", type=Path, help="team Git checkout; only selected regular files are copied")
    group.add_argument("--demo", type=Path, default=FIXTURE, help="directory containing the packaged pytest demo")
    parser.add_argument("--test", default="python3 -B -m pytest -q -p no:cacheprovider")
    parser.add_argument("--expect-failure")
    parser.add_argument("--fix-file")
    parser.add_argument("--fix-before")
    parser.add_argument("--fix-after")
    parser.add_argument("--runs", type=int, default=1)
    parser.add_argument("--timeout", type=float, default=180, help="MCP operation deadline in seconds")
    parser.add_argument("--out", type=Path, default=Path("artifacts/pilot.json"))
    parser.add_argument("--keep", action="store_true", help="retain the private workspace and logs")
    args = parser.parse_args()
    origin = (args.workspace or args.demo).resolve()
    if (args.out.parent.resolve() / args.out.name).is_relative_to(origin):
        parser.error("choose an --out outside the input workspace; it will not be modified")
    with report(args.out) as record, contextlib.ExitStack() as stack:
        test = shlex.split(args.test)
        if args.runs < 1 or not math.isfinite(args.timeout) or args.timeout <= 0 or not test:
            raise ValueError("positive runs, finite positive timeout and non-empty test command required")
        supplied = (args.fix_file, args.fix_before, args.fix_after)
        if args.workspace or any(x is not None for x in supplied):
            if not (args.fix_file and args.fix_before and args.fix_after is not None):
                raise ValueError("--fix-file, --fix-before and --fix-after must be supplied together")
            filename, before, after = supplied
        else:
            filename, before, after = "calculator.py", "value * 3", "value * 2"
        marker = args.expect_failure if args.workspace or args.expect_failure is not None else "assert 21 == 14"
        if not marker:
            raise ValueError("--expect-failure must identify the failing test in a team checkout")
        temp_root = Path(tempfile.gettempdir()).resolve()
        if temp_root.is_relative_to(origin):
            raise ValueError("choose a TMPDIR outside the input workspace")
        work = Path(tempfile.mkdtemp(prefix="execlet-pilot-", dir=temp_root))
        if args.keep:
            print(f"kept workspace: {work}", file=sys.stderr)
        else:
            stack.callback(shutil.rmtree, work)
        workspace = work / "workspace"
        copy_workspace(origin, workspace, team=args.workspace is not None)
        source = fix_path(workspace, filename)
        original = source.read_bytes()
        old, new = before.encode(), after.encode()
        if old == new or original.count(old) != 1:
            raise ValueError("the repair must replace one unambiguous occurrence with different text")
        fixed = original.replace(old, new, 1)
        home = work / "home"
        home.mkdir(mode=0o700)
        daemon = Daemon(args.server, work)
        image = daemon.image_id(args.image)
        record.update(owner=daemon.owner, image=image, server_sha256=checksum(daemon.server), cli_sha256=checksum(args.cli),
                      fixture="team checkout" if args.workspace else "pytest demo", runs=[])
        with daemon.running():
            for attempt in range(args.runs):
                row = {"attempt": attempt + 1}
                record["runs"].append(row)
                if attempt:
                    source.write_bytes(original)
                with connect(args.cli.resolve(), workspace, image, daemon, home, args.timeout) as session:
                    with offline_sandbox(session, daemon, row) as (sid, start):
                        repair(session, sid, start, source, fixed, test, marker, row)
        record.update(owned_containers=0,
                      median_first_result_ms=statistics.median(r["first_result_ms"] for r in record["runs"]),
                      median_repair_ms=statistics.median(r["repair_ms"] for r in record["runs"]))
    print(f"{args.runs} repair loop(s) passed; {args.out}")


if __name__ == "__main__":
    try:
        main()
    except (OSError, RuntimeError, ValueError, subprocess.SubprocessError) as error:
        sys.exit(f"pilot: {error}")
