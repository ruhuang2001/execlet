"""Verify policy and workload behavior on runc/runsc with Linux host cgroup v2 evidence."""

import argparse
import hashlib
import io
import json
import math
import os
import platform
import shutil
import socket
import subprocess
import tarfile
import threading
import time
import uuid
from contextlib import contextmanager
from datetime import datetime, timezone
from http.client import HTTPConnection
from pathlib import Path


RUNTIMES = ("runc", "runsc")
DOCKER_HOST = os.environ.get("DOCKER_HOST") or "unix:///var/run/docker.sock"
PAUSE_TICKS = 50
PAUSE_SECONDS = 3.5
ARCHIVE_BYTES = (12 << 20) + 37
PID_HOLD_SECONDS = 8
OOM_MIB = 384

HEARTBEAT = r"""
set -eu
i=0
while [ "$i" -lt "$1" ]; do
    i=$((i + 1))
    printf '%s %s\n' "$i" "$(date +%s)"
    sleep .1
done
"""

ALLOCATE = r"""
BEGIN {
    block = "x"
    for (i = 0; i < 20; i++) block = block block
    retained[1] = sprintf("1:%s", block)
    print "allocation-started"
    fflush()
    if (system("sleep 1.5") != 0) exit 3
    for (i = 2; i <= mib; i++) retained[i] = sprintf("%d:%s", i, block)
    printf "allocation-completed: %d MiB\n", mib
    exit result
}
"""

PID_PRESSURE = r"""
printf 'attempt-cap %s\n' "$1"
hold=$2
i=0
children=
while [ "$i" -lt "$1" ]; do
    sleep "$hold" &
    children="$children $!"
    printf 'child %s\n' "$!"
    i=$((i + 1))
done
for pid in $children; do
    if IFS= read -r row 2>/dev/null <"/proc/$pid/stat"; then
        set -- ${row##*) }
        if [ "$2" = "$$" ]; then
            case "$1" in Z|X) ;; *) printf 'live %s\n' "$pid" ;; esac
        fi
    fi
done
printf 'survey-complete\n'
wait
"""

REAP_CHILDREN = r"""
set -eu
until_released() {
    i=0
    while [ ! -e "$1" ] && [ "$i" -lt 150 ]; do
        sleep .1
        i=$((i + 1))
    done
    [ -e "$1" ]
}
until_released /tmp/p3-release-zombie </dev/null >/dev/null 2>&1 &
zombie=$!
(until_released /tmp/p3-release-orphan && printf 'orphan-exited\n') </dev/null &
orphan=$!
printf '%s %s %s\n' "$$" "$zombie" "$orphan"
kill -STOP "$$"
"""

READ_PROCESSES = r"""
for pid do
    if IFS= read -r row 2>/dev/null <"/proc/$pid/stat"; then
        printf '%s\n' "$row"
    elif [ -d "/proc/$pid" ]; then
        exit 1
    fi
done
"""


def cli(*cmd):
    return subprocess.check_output(cmd, text=True, timeout=30).strip()


def docker(*args):
    return cli("docker", "--host", DOCKER_HOST, *args)


def wait_for(description, probe, timeout=15):
    deadline = time.monotonic() + timeout
    while True:
        value = probe()
        if value is not None:
            return value
        if time.monotonic() >= deadline:
            raise AssertionError(f"timed out waiting for {description}")
        time.sleep(.05)


def dock_state(cid):
    return json.loads(docker("inspect", "--format", "{{json .State}}", cid))


def counters(path):
    return {key: int(value) for key, value in (line.split() for line in path.read_text().splitlines())}


class SandboxChecks:
    def __init__(self, request, base, cid, scope):
        self.request, self.base, self.cid, self.scope = request, base, cid, scope

    def exec(self, *command, timeout_ms=20000):
        return self.request(
            "POST", self.base + "/executions", dict(command=command, timeout_ms=timeout_ms)
        )

    def execution(self, initial):
        return self.request("GET", self.base + "/executions/" + initial["id"])

    def completed(self, initial, observe=lambda ex: None):
        def poll():
            ex = self.execution(initial)
            observe(ex)
            return ex if ex["state"] == "completed" else None

        return wait_for(f"execution {initial['id']} to complete", poll, timeout=30)

    def docker_exec(self, *command):
        return docker("exec", self.cid, *command)

    def processes(self, pids):
        result = {}
        raw = self.docker_exec("sh", "-c", READ_PROCESSES, "proc-stat", *map(str, pids))
        for row in raw.splitlines():
            pid, rest = row.split(" ", 1)
            fields = rest.rsplit(") ", 1)[1].split()
            result[int(pid)] = dict(state=fields[0], ppid=int(fields[1]), start_ticks=int(fields[19]))
        return result

    def archive(self, evidence):
        content = hashlib.shake_256(b"execlet-runtime-archive").digest(ARCHIVE_BYTES)
        digest = hashlib.sha256(content).hexdigest()
        uid, gid = (int(self.docker_exec("id", "-" + part)) for part in ("u", "g"))
        directory = tarfile.TarInfo("workspace")
        directory.type, directory.mode, directory.mtime = tarfile.DIRTYPE, 0o755, 1735689600
        sample = tarfile.TarInfo("workspace/sample")
        sample.size, sample.mode, sample.mtime = len(content), 0o640, 1700000000
        link = tarfile.TarInfo("workspace/link")
        link.type, link.linkname = tarfile.SYMTYPE, "sample"
        link.mode, link.mtime = 0o777, 1700000123
        link.uid, link.gid = uid + 1, gid + 2

        def metadata(header):
            return dict(
                type=header.type.decode(), target=header.linkname, mode=header.mode,
                uid=header.uid, gid=header.gid, mtime=header.mtime, size=header.size,
            )

        evidence.update(bytes=len(content), sha256=digest, link_upload=metadata(link))
        raw = io.BytesIO()
        with tarfile.open(fileobj=raw, mode="w") as tf:
            tf.addfile(directory)
            tf.addfile(sample, io.BytesIO(content))
            tf.addfile(link)
        self.request("PUT", self.base + "/files/archive?path=/tmp", raw.getvalue())
        downloaded = self.request("GET", self.base + "/files/archive?path=/tmp/workspace")
        with tarfile.open(fileobj=io.BytesIO(downloaded), mode="r:") as tf:
            headers = tf.getmembers()
            members = {member.name: metadata(member) for member in headers}
            evidence["download_members"] = members
            evidence["download_sha256"] = hashlib.sha256(tf.extractfile(sample.name).read()).hexdigest()

        expected_members = {h.name: dict(metadata(h), uid=0, gid=0) for h in (directory, sample, link)}
        link_stat = self.request("GET", self.base + "/files/stat?path=/tmp/workspace/link")
        evidence["link_stat"] = link_stat
        expected_link = evidence["link_expected_on_disk"] = dict(
            type="symlink", symlink_target=link.linkname, uid=uid, gid=gid, mode=link.mode,
            mtime=datetime.fromtimestamp(link.mtime, timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
            size=len(link.linkname), is_dir=False,
        )
        guest_uid, guest_gid, guest_mode, guest_mtime = self.docker_exec(
            "busybox", "stat", "-c", "%u %g %a %Y", "/tmp/workspace/link",
        ).split()
        evidence["guest_lstat"] = dict(
            uid=int(guest_uid), gid=int(guest_gid), mode=int(guest_mode, 8), mtime=int(guest_mtime),
        )
        evidence["guest_sha256"] = self.docker_exec(
            "sha256sum", "/tmp/workspace/sample", "/tmp/workspace/link"
        ).splitlines()
        guest_hashes = [line.split()[0] for line in evidence["guest_sha256"]]
        assert len(headers) == len(expected_members) and members == expected_members, members
        assert evidence["download_sha256"] == digest, "archive download bytes changed"
        assert guest_hashes == [digest, digest], "guest file or symlink target bytes changed"
        assert {key: link_stat.get(key) for key in expected_link} == expected_link, (link_stat, expected_link)
        link_metadata = dict(uid=uid, gid=gid, mode=link.mode, mtime=link.mtime)
        assert evidence["guest_lstat"] == link_metadata, (evidence["guest_lstat"], link_metadata)

    def pause_resume(self, evidence, runtime):
        def ticks(ex):
            return [tuple(map(int, line.split())) for line in ex["stdout"].split("\n")[:-1]]

        started_at = time.time()
        initial = self.exec("sh", "-c", HEARTBEAT, "heartbeat", str(PAUSE_TICKS))
        evidence["execution_id"] = initial["id"]

        def advancing():
            ex = self.execution(initial)
            assert ex["state"] == "running", ex
            progress = ticks(ex)
            return progress if len(progress) >= 4 else None

        evidence["before_pause"] = wait_for("heartbeat to advance", advancing)
        evidence["before_host_cgroup"] = counters(self.scope / "cgroup.events")
        evidence["pause_api"] = self.request("POST", self.base + "/pause")["state"]
        paused_from, paused_clock = time.time(), time.monotonic()
        try:
            evidence["pause_docker"] = dock_state(self.cid)["Paused"]
            evidence["pause_host_cgroup"] = counters(self.scope / "cgroup.events")
            snapshots = evidence["during_pause"] = []
            for delay in (.25, PAUSE_SECONDS):
                time.sleep(delay)
                ex = self.execution(initial)
                snapshots.append(dict(
                    state=ex["state"], last_tick=ticks(ex)[-1],
                    host_frozen=counters(self.scope / "cgroup.events")["frozen"],
                ))
        finally:
            paused_until = time.time()
            evidence["paused_seconds"] = round(time.monotonic() - paused_clock, 3)
            evidence["resume_api"] = self.request("POST", self.base + "/resume")["state"]
        evidence["resume_docker"] = dock_state(self.cid)["Paused"]
        evidence["resume_host_cgroup"] = counters(self.scope / "cgroup.events")
        ex = evidence["execution"] = self.completed(initial)
        produced = evidence["ticks"] = ticks(ex)
        evidence["pause_epoch_seconds"] = [paused_from, paused_until]
        first, end = evidence["fully_paused_epoch_seconds"] = [math.ceil(paused_from), math.floor(paused_until)]
        during = evidence["produced_while_paused"] = [tick for tick in produced if first <= tick[1] < end]
        assert evidence["pause_api"] == "paused" and evidence["pause_docker"], "pause not confirmed"
        assert evidence["resume_api"] == "running" and not evidence["resume_docker"], "resume not confirmed"
        frozen = evidence["host_frozen_sequence"] = [
            evidence[f"{phase}_host_cgroup"]["frozen"] for phase in ("before", "pause", "resume")
        ]
        assert frozen[0] == frozen[2] == 0 and frozen[1] in (0, 1), frozen
        if runtime != "runsc":
            assert frozen[1] == 1 and all(snap["host_frozen"] == 1 for snap in snapshots), frozen
        assert all(snap["state"] == "running" for snap in snapshots), snapshots
        assert snapshots[0]["last_tick"] == snapshots[1]["last_tick"], f"heartbeat advanced during pause: {snapshots}"
        assert ex["pid"] == initial["pid"] and ex["exit_code"] == 0 and not ex["output_truncated"], ex
        assert ex["termination_reason"] == "exited", ex
        assert [tick[0] for tick in produced] == list(range(1, PAUSE_TICKS + 1)), produced
        timestamps = [stamp for _, stamp in produced]
        assert timestamps == sorted(timestamps) and timestamps[-1] <= int(time.time()), timestamps
        assert int(started_at) <= produced[0][1] <= int(paused_from), produced
        assert end - first >= 2, "pause window did not cover two whole guest clock seconds"
        assert not during, f"workload ticks in pause window [{first}, {end}): {during}"
        evidence["ticks_after_resume"] = sum(t >= math.ceil(paused_until) for t in timestamps)
        assert evidence["ticks_after_resume"] >= 3, "insufficient workload progress after resume"

    def pid_limit(self, evidence, runtime, limit):
        current = self.scope / "pids.current"
        events = self.scope / "pids.events"
        attempts = limit + 16
        evidence.update(
            host_source=str(events), attempt_cap=attempts, hold_seconds=PID_HOLD_SECONDS,
            baseline_current=int(current.read_text()), before=counters(events),
        )
        evidence["peak_sampled_current"] = evidence["baseline_current"]
        evidence["after"] = evidence["before"]
        stop_sampling = threading.Event()

        def monitor():
            while True:
                try:
                    evidence["peak_sampled_current"] = max(evidence["peak_sampled_current"], int(current.read_text()))
                    evidence["after"] = counters(events)
                except OSError as err:
                    evidence["host_scope_read_error"] = str(err)
                    return
                if stop_sampling.wait(.005):
                    return

        def sample(ex):
            evidence["execution"] = ex
            if "survey-complete\n" in ex["stdout"]:
                evidence.setdefault("survey_observed_after_seconds", time.monotonic() - started)

        started = time.monotonic()
        sampling = threading.Thread(target=monitor)
        sampling.start()
        try:
            initial = self.exec("sh", "-c", PID_PRESSURE, "pid-pressure", str(attempts), str(PID_HOLD_SECONDS))
            ex = self.completed(initial, sample)
        finally:
            stop_sampling.set()
            sampling.join()
        if current.exists():
            evidence["after_current"] = int(current.read_text())
            evidence["after"] = counters(events)
        delta = evidence["max_event_delta"] = evidence["after"]["max"] - evidence["before"]["max"]
        lines = ex["stdout"].splitlines()
        children = [int(line.split()[1]) for line in lines if line.startswith("child ")]
        live = [int(line.split()[1]) for line in lines if line.startswith("live ")]
        survey_during_hold = evidence.get("survey_observed_after_seconds", math.inf) < PID_HOLD_SECONDS
        evidence.update(children_started=len(children),
                        concurrent_guest_children=len(live) if survey_during_hold else None)
        peak = evidence["peak_sampled_current"]
        assert lines and lines[0] == f"attempt-cap {attempts}", ex
        assert 0 < len(children) <= attempts and len(children) == len(set(children)), ex
        assert set(live) <= set(children) and len(live) == len(set(live)), ex
        assert 0 < peak <= limit and delta >= 0, (peak, limit, delta)
        denied = evidence["host_denial_verified"] = delta > 0 and peak == limit
        if runtime == "runsc" and "after_current" not in evidence:
            evidence["sandbox_state"] = wait_for(
                "PID pressure sandbox to be retired",
                lambda: "killed" if self.request("GET", self.base)["state"] == "killed" else None,
            )
            assert denied and ex["exit_code"] != 0, (peak, limit, delta, ex)
            assert ex["termination_reason"] in ("evicted", "exited", "unknown"), ex
            evidence["finding"] = "host_pids_limit_reached_and_sandbox_lost"
            return
        assert ex["termination_reason"] == "exited" and not ex["output_truncated"], ex
        assert 0 < evidence["after_current"] < limit, evidence
        beyond = evidence["guest_children_exceeded_host_limit"] = len(live) > limit if survey_during_hold else None
        if runtime == "runsc" and beyond:
            assert ex["exit_code"] == 0, ex
            evidence["finding"] = "virtual_guest_children_exceeded_host_pids_max"
        elif denied:
            assert not beyond and len(children) < attempts and ex["exit_code"] != 0 and "fork" in ex["stderr"], ex
            evidence["finding"] = "host_pids_limit_reached_and_denied_forks"
        else:
            raise AssertionError("did not verify host PID denial or measure a runsc virtual-task distinction")

        def pressure_children_gone():
            evidence["remaining_children"] = self.processes(children)
            return True if not evidence["remaining_children"] else None

        wait_for("PID pressure children to disappear", pressure_children_gone, timeout=5)

    def reap(self, evidence):
        initial = self.exec("sh", "-c", REAP_CHILDREN)
        evidence["execution_id"] = initial["id"]

        def get_pids():
            ex = self.execution(initial)
            assert ex["state"] == "running", ex
            lines = ex["stdout"].split("\n")[:-1]
            return tuple(map(int, lines[0].split())) if lines else None

        parent, zombie, orphan = wait_for("reaping fixture PIDs", get_pids)
        pids = evidence["pids"] = dict(parent=parent, zombie=zombie, orphan=orphan)
        assert parent == initial["pid"] and min(pids.values()) > 1 and len(set(pids.values())) == 3, pids

        def observe(phase, predicate):
            def poll():
                result = evidence[phase] = self.processes(pids.values())
                return True if predicate(result) else None

            wait_for(phase, poll, timeout=5)

        observe("stopped_parent", lambda ps: ps.get(parent, {}).get("state") == "T")
        self.request("PUT", self.base + "/files?path=/tmp/p3-release-zombie", b"exit")
        observe(
            "zombie_with_parent",
            lambda ps: ps.get(zombie, {}).get("state") == "Z" and ps[zombie]["ppid"] == parent
            and ps.get(orphan, {}).get("ppid") == parent and ps[orphan]["state"] not in ("Z", "X"),
        )
        self.docker_exec("sh", "-c", 'kill -KILL "$1"', "kill-parent", str(parent))
        observe(
            "adopted_orphan_and_reaped_zombie",
            lambda ps: zombie not in ps and orphan in ps and ps[orphan]["ppid"] not in (0, parent)
            and ps[orphan]["state"] not in ("Z", "X")
            and ps[orphan]["start_ticks"] == evidence["zombie_with_parent"][orphan]["start_ticks"],
        )
        self.request("PUT", self.base + "/files?path=/tmp/p3-release-orphan", b"exit")
        ex = evidence["execution"] = self.completed(initial)
        observe("remaining_fixture_pids", lambda ps: not ps)
        assert ex["stdout"] == f"{parent} {zombie} {orphan}\norphan-exited\n", ex
        assert ex["exit_code"] == 137 and ex["termination_reason"] in ("exited", "unknown"), ex
        followup = evidence["followup_execution"] = self.completed(self.exec(
            "sh", "-c", 'for i in 1 2 3 4; do (sleep .1; echo "slot $i") & done; wait',
        ))
        assert followup["exit_code"] == 0 and sorted(followup["stdout"].splitlines()) == [
            "slot 1", "slot 2", "slot 3", "slot 4",
        ], followup

    def oom(self, evidence, runtime):
        events = self.scope / "memory.events"
        evidence["host_source"] = str(events)
        keys = ("max", "oom", "oom_kill")

        def probe(name, mib, exit_code):
            run = evidence[name] = dict(allocation_cap_mib=mib, before=counters(events), samples=[])
            assert all(key in run["before"] for key in keys), "incomplete baseline memory.events counters"
            stop_sampling = threading.Event()

            def monitor():
                while True:
                    try:
                        values = counters(events)
                    except (OSError, ValueError) as err:
                        run["host_read_error"] = str(err)
                        return
                    if not run["samples"] or run["samples"][-1]["events"] != values:
                        run["samples"].append(dict(epoch_seconds=time.time(), events=values))
                    if stop_sampling.wait(.01):
                        return

            def observe(ex):
                run["execution"] = ex
                if ex["state"] == "running" and ex["stdout"].startswith("allocation-started\n"):
                    run.setdefault("running_sample", dict(
                        pid=ex["pid"], stdout=ex["stdout"], epoch_seconds=time.time(),
                    ))

            sampling = threading.Thread(target=monitor)
            sampling.start()
            try:
                initial = self.exec("awk", "-v", f"mib={mib}", "-v", f"result={exit_code}", ALLOCATE)
                run["execution_id"] = initial["id"]
                ex = self.completed(initial, observe)
            finally:
                stop_sampling.set()
                sampling.join()
                try:
                    after = counters(events)
                except OSError as err:
                    run["final_host_read_error"] = str(err)
                    after = run["samples"][-1]["events"] if run["samples"] else None
                run["after"] = after
                if after is not None:
                    run["delta"] = {key: after[key] - run["before"][key] for key in keys}

            assert run.get("running_sample", {}).get("pid") == initial["pid"], "allocation was not observed live"
            assert ex["pid"] == initial["pid"], "allocation execution PID changed"
            return run, ex

        control, ex = probe("exit_137_control", 4, 137)
        assert ex["stdout"] == "allocation-started\nallocation-completed: 4 MiB\n" and not ex["output_truncated"], ex
        assert "final_host_read_error" not in control, control
        assert control["delta"]["oom"] == control["delta"]["oom_kill"] == 0, control
        assert ex["exit_code"] == 137 and ex["termination_reason"] in ("exited", "unknown"), ex

        pressure, ex = probe("memory_pressure", OOM_MIB, 0)
        reason = ex["termination_reason"]
        pressure["api_reported_oom"] = reason == "oom"
        delta = pressure.get("delta", {})
        assert all(delta.get(key, 0) > 0 for key in keys), f"no scoped host OOM kill verified: {delta}"
        assert "allocation-completed:" not in ex["stdout"], ex
        if reason == "evicted" or not self.scope.exists():
            wait_for("lost sandbox to be retired", lambda: True if self.request("GET", self.base)["state"] == "killed" else None)
        sandbox = self.request("GET", self.base)
        state = pressure["sandbox_state"] = sandbox["state"]
        if state == "killed":
            pressure["kill_reason"] = sandbox["kill_reason"]
            assert sandbox["kill_reason"] == "evicted" and reason in ("oom", "unknown", "evicted"), (sandbox, ex)
            if ex["exit_code"] is None:
                assert reason == "evicted" or (reason == "unknown" and ex["output_truncated"]), ex
            else:
                assert ex["exit_code"] != 0, ex
            live = pressure["running_containers"] = docker("ps", "-q", "--no-trunc", "--filter", "id=" + self.cid).splitlines()
            assert not live, f"retired OOM sandbox is still running: {live}"
            pressure["finding"] = "sandbox_cgroup_oom_and_sandbox_lost"
            return
        assert state == "running" and reason != "evicted" and ex["exit_code"] not in (None, 0), (state, ex)
        survivor = pressure["survivor_execution"] = self.completed(self.exec("/bin/true"))
        assert survivor["termination_reason"] == "exited" and survivor["exit_code"] == 0, survivor
        if runtime == "runsc":
            assert reason in ("oom", "exited", "unknown"), ex
            pressure["finding"] = "sandbox_cgroup_oom_without_guest_victim_proof"
        else:
            assert reason == "oom" and ex["exit_code"] == 137 and not ex["output_truncated"], ex
            pressure["finding"] = "sandbox_cgroup_oom_and_execution_oom"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server", type=Path, default=Path("bin/execletd"))
    parser.add_argument("--image", default="execlet-guest:minimal", help="preloaded non-root Alpine guest fixture")
    parser.add_argument("--out", type=Path, default=Path("artifacts/runtime"))
    runtime_mode = parser.add_mutually_exclusive_group()
    runtime_mode.add_argument(
        "--runtime", choices=RUNTIMES, help="run only the selected runtime (default: both)",
    )
    runtime_mode.add_argument(
        "--engine-default",
        action="store_true",
        help="verify the Docker daemon default runtime without a policy override",
    )
    args = parser.parse_args()
    if not __debug__:
        parser.error("this suite requires Python assertions to be enabled")
    args.out.mkdir(parents=True, exist_ok=True)
    owner = "runtime-e2e-" + uuid.uuid4().hex
    spec = dict(
        image=args.image,
        memory_mb=256,
        pids_limit=128,
        nano_cpus=500000000,
        network="none",
        max_concurrent_executions=1,
    )
    policy = dict(
        max_sandboxes=2,
        total_memory_mb=512,
        total_nano_cpus=1000000000,
        total_pids=256,
        allowed_networks=["none"],
        allowed_images=[args.image],
        allowed_runtimes=RUNTIMES,
    )
    if args.engine_default:
        del policy["allowed_runtimes"]
    policy_path = args.out / "policy.json"
    policy_path.write_text(json.dumps(policy))
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        port = sock.getsockname()[1]

    def http(method, path, body=None):
        c = HTTPConnection("127.0.0.1", port, timeout=30)
        try:
            data = (
                body
                if isinstance(body, bytes)
                else json.dumps(body).encode()
                if body is not None
                else None
            )
            c.request(
                method,
                path,
                data,
                {
                    "Content-Type": "application/octet-stream"
                    if isinstance(body, bytes)
                    else "application/json"
                },
            )
            r = c.getresponse()
            raw = r.read()
            return r.status, (
                json.loads(raw)
                if r.getheader("Content-Type", "").startswith("application/json")
                else raw
            )
        finally:
            c.close()

    def request(method, path, body=None):
        code, obj = http(method, path, body)
        assert 200 <= code < 300, (method, path, code, obj)
        return obj

    report = dict(
        owner=owner,
        kernel=platform.release(),
        arch=platform.machine(),
        kvm=os.path.exists("/dev/kvm"),
        runtimes={},
        failures=[],
    )

    def failure(check, err, runtime=None):
        problem = dict(check=check, runtime=runtime, error=f"{type(err).__name__}: {err}")
        report["failures"].append(problem)
        print(f"FAIL {runtime or 'suite'}/{check}: {problem['error']}", flush=True)
        return problem["error"]

    srv = None
    with (args.out / "runtime.log").open("w") as log:
        def start():
            nonlocal srv
            srv = subprocess.Popen(
                [
                    str(args.server.resolve()), "-addr", f"127.0.0.1:{port}", "-owner", owner,
                    "-docker", DOCKER_HOST.removeprefix("unix://"), "-policy", str(policy_path),
                    "-state-dir", str(args.out / ("state-" + owner)),
                ],
                stdout=log, stderr=log,
            )

            def ready():
                if srv.poll() is not None:
                    raise RuntimeError(f"execletd exited during startup: {srv.returncode}; see runtime.log")
                try:
                    code, body = http("GET", "/readyz")
                except OSError:
                    return None
                return body if code == 200 else None

            return wait_for("execletd readiness", ready, timeout=10)

        def stop():
            if srv is not None and srv.poll() is None:
                srv.terminate()
                try:
                    srv.wait(timeout=25)
                except subprocess.TimeoutExpired:
                    srv.kill()
                    srv.wait(timeout=5)
                    raise

        @contextmanager
        def sandbox_for_check(runtime, inputs, result):
            sb = result["sandbox"] = request("POST", "/v1/sandboxes", inputs)
            base = "/v1/sandboxes/" + sb["id"]
            try:
                assert (
                    (args.engine_default or sb["runtime"] == runtime)
                    and sb["agent_mode"] == "guest"
                    and sb["protected"]
                ), sb
                assert sb["max_concurrent_executions"] == 1, sb
                inspected = json.loads(docker("inspect", "--format", "{{json .}}", "execlet-" + sb["id"]))
                cid = result["container_id"] = inspected["Id"]
                assert inspected["Config"]["Labels"]["execlet.owner"] == owner, cid
                actual = result["docker_runtime"] = inspected["HostConfig"]["Runtime"]
                assert actual == runtime, (runtime, actual)
                pid = result["host_pid"] = inspected["State"]["Pid"]
                assert pid > 0, f"container has no host PID: {cid}"
                membership = Path("/proc", str(pid), "cgroup").read_text().splitlines()
                cg = result["cgroup"] = next((line[3:] for line in membership if line.startswith("0::/")), "")
                scope = Path("/sys/fs/cgroup") / cg.lstrip("/")
                assert ".." not in scope.parts and scope.name in (cid, f"docker-{cid}.scope"), (pid, membership, cid)
                expected = {
                    "memory.max": str(spec["memory_mb"] << 20),
                    "memory.swap.max": "0",
                    "pids.max": str(spec["pids_limit"]),
                    "cpu.max": "50000 100000",
                }
                limits = result["host_limits"] = {}
                for name in expected:
                    limits[name] = (scope / name).read_text().strip()
                assert limits == expected, (limits, expected)
                yield SandboxChecks(request, base, cid, scope)
            finally:
                try:
                    request("DELETE", base)
                except Exception as err:
                    failure("delete", err, runtime)

        def verify_runtime(runtime, inputs, result):
            def run_check(name, check, *parameters):
                evidence = result.setdefault(name, {})
                try:
                    check(evidence, *parameters)
                    evidence["status"] = "passed"
                except Exception as err:
                    evidence.update(status="failed", error=failure(name, err, runtime))

            with sandbox_for_check(runtime, inputs, result) as checks:
                base = checks.base
                net = result["guest_net"] = checks.docker_exec("cat", "/proc/net/dev")
                assert [
                    line.split(":")[0].strip()
                    for line in net.splitlines()
                    if ":" in line
                ] == ["lo"], net
                path = base + "/files?path=/tmp/probe"
                entry = result["file"] = request("PUT", path, b"guest-ok")
                assert entry == request("GET", base + "/files/stat?path=/tmp/probe") and entry["size"] == 8, entry
                ex = result["execution"] = checks.completed(checks.exec("cat", "/tmp/probe", timeout_ms=2000))
                assert ex["exit_code"] == 0 and ex["stdout"] == "guest-ok", ex
                assert request("GET", path) == b"guest-ok", "file GET did not return the uploaded bytes"
                run_check("archive", checks.archive)
                result["archive_sha256"] = result["archive"].get("sha256")
                run_check("pause_resume", checks.pause_resume, runtime)
                run_check("reaping", checks.reap)

                checks.docker_exec("sh", "-c", "echo changed > /tmp/probe")
                assert request("GET", path) == b"changed\n", "file GET did not observe the guest write"
                result["observed_guest_write"] = True
                stop()
                start()
                assert request("GET", base)["state"] == "running", "sandbox was not restored as running"
                assert request("GET", path) == b"changed\n", "restored sandbox lost the guest write"
                assert dock_state(checks.cid)["Pid"] == result["host_pid"], "restored sandbox is not the same running container"
                result["restored_filesystem"] = True

            def check_pid_limit(evidence):
                with sandbox_for_check(runtime, inputs, evidence) as checks:
                    checks.pid_limit(evidence, runtime, spec["pids_limit"])

            def check_oom(evidence):
                with sandbox_for_check(runtime, inputs, evidence) as checks:
                    checks.oom(evidence, runtime)

            run_check("pid_limit", check_pid_limit)
            run_check("oom", check_oom)

        try:
            assert platform.system() == "Linux", "requires Linux host /proc and cgroup v2"
            assert DOCKER_HOST.startswith("unix:///"), "requires a local Docker Unix socket (DOCKER_HOST)"
            info = json.loads(docker("info", "--format", "{{json .}}"))
            report.update(
                docker=info["ServerVersion"], docker_host_arch=info["Architecture"],
                engine_default=info["DefaultRuntime"], cgroup_version=info["CgroupVersion"],
                installed_runtimes=sorted(info["Runtimes"]),
            )
            assert str(info["CgroupVersion"]) == "2", "Docker must use cgroup v2"
            report["image_id"] = docker("image", "inspect", "--format", "{{.Id}}", args.image)
            if args.engine_default:
                selected = (report["engine_default"],)
            else:
                selected = (args.runtime,) if args.runtime else RUNTIMES
            report["selected_runtimes"] = selected
            report["runsc"] = cli("runsc", "--version") if "runsc" in selected and shutil.which("runsc") else None
            report["server_sha256"] = hashlib.sha256(args.server.read_bytes()).hexdigest()
            report["ready"] = start()
            code, refusal = http("POST", "/v1/sandboxes", {**spec, "runtime": "unapproved"})
            report["unapproved"] = refusal
            assert code == 400, (code, refusal)
            for runtime in selected:
                inputs = spec if args.engine_default else {**spec, "runtime": runtime}
                result = report["runtimes"][runtime] = dict(requested_runtime=inputs.get("runtime"))
                try:
                    assert runtime in info["Runtimes"], f"Docker runtime {runtime} is not configured"
                    verify_runtime(runtime, inputs, result)
                except Exception as err:
                    failure("sandbox", err, runtime)
        except KeyboardInterrupt as err:
            failure("interrupted", err)
        except Exception as err:
            failure("setup", err)
        finally:
            try:
                stop()
            except Exception as err:
                failure("stop", err)
            if srv is not None:
                try:
                    owned = ("ps", "-aq", "--no-trunc", "--filter", "label=execlet.owner=" + owner)
                    remaining = report["containers_left_by_api"] = docker(*owned).splitlines()
                    if remaining:
                        failure("cleanup", RuntimeError(f"daemon left own containers: {remaining}"))
                        docker("rm", "-f", *remaining)
                    report["remaining_owned_containers"] = docker(*owned).splitlines()
                    if report["remaining_owned_containers"]:
                        failure("cleanup", RuntimeError(f"owned containers remain: {report['remaining_owned_containers']}"))
                except Exception as err:
                    failure("cleanup", err)
            report["status"] = "failed" if report["failures"] else "passed"
            (args.out / "report.json").write_text(json.dumps(report, indent=2) + "\n")

    print(f"{report['status']}: {args.out / 'report.json'}", flush=True)
    return int(bool(report["failures"]))


if __name__ == "__main__":
    raise SystemExit(main())
