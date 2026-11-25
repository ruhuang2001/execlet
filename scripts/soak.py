"""Measure concurrent create/stream/delete rounds and verify single-host recovery."""
import argparse
from concurrent.futures import FIRST_EXCEPTION, ThreadPoolExecutor, wait
import math
import statistics
import subprocess
import sys
import tempfile
import threading
import time
from pathlib import Path

from host_harness import Daemon, checksum, report

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "sandbox-python/src"))
from execlet import CancellationToken, Client, CreateSandboxRequest, Error, ExecuteRequest


def client_for(daemon):
    return Client(domain=daemon.domain, api_key=daemon.api_key, max_retries=0)


def budget():
    return CancellationToken(timeout=45)


def kill(client, sid, cancel):
    removed = client.kill_sandbox(sid, cancel=cancel)
    if removed.id != sid or removed.state != "killed":
        raise RuntimeError(f"delete of {sid} not confirmed: {removed}")


def detached_command(daemon, spec):
    with client_for(daemon) as client:
        token = budget()
        sid = client.create_sandbox(spec, cancel=token).id
        command = ExecuteRequest(command=["sh", "-c", "printf started; exec sleep 120"], output_tail=True)
        eid = client.start_execution(sid, command, cancel=token).id
        output = []

        def detach(text):
            output.append(text)
            if "".join(output) == "started":
                token.cancel()

        try:
            client.stream_execution(sid, eid, on_stdout=detach, cancel=token, cancel_remote=False)
        except Error as error:
            if error.code != "wait_timeout" or "".join(output) != "started":
                raise
        else:
            raise RuntimeError("probe stream completed before disconnect")
    with client_for(daemon) as reader:
        observed = reader.get_execution(sid, eid, cancel=budget())
        if (observed.sandbox_id, observed.id, observed.state, observed.stdout) != (sid, eid, "running", "started"):
            raise RuntimeError(f"reconnected client did not find the speaking command running: {observed}")
    return sid, eid


def disconnect_probe(daemon, spec):
    sid, eid = detached_command(daemon, spec)
    with client_for(daemon) as client:
        token = budget()
        result = client.cancel_execution(sid, eid, cancel=token)
        while result.state == "running":
            time.sleep(0.05)
            result = client.get_execution(sid, eid, cancel=token)
        if (result.sandbox_id, result.id, result.state, result.termination_reason) != (sid, eid, "completed", "canceled"):
            raise RuntimeError(f"explicit cancel of {eid} not confirmed: {result}")
        kill(client, sid, token)
    return {"sandbox": sid, "execution": eid, "state_after_disconnect": "running",
            "output_after_disconnect": "started", "cancel_confirmed": True}


def restart_probe(daemon, spec):
    sid, eid = detached_command(daemon, spec)
    containers = daemon.owned(sid)
    if len(containers) != 1:
        raise RuntimeError(f"expected one probe container for {sid}, got {containers}")
    start = time.monotonic()
    daemon.stop(crash=True)
    daemon.start()
    recovery_ms = round((time.monotonic() - start) * 1000, 1)
    with client_for(daemon) as client:
        record = client.get_execution(sid, eid, cancel=budget())
        sandbox = client.get_sandbox(sid, cancel=budget())
    observed = {"sandbox": sandbox.id, "sandbox_state": sandbox.state, "execution": record.id,
                "state": record.state, "termination_reason": record.termination_reason,
                "exit_code": record.exit_code, "output_truncated": record.output_truncated,
                "container": containers[0], "restart_to_ready_ms": recovery_ms}
    if ((sandbox.id, sandbox.state, record.sandbox_id, record.id, record.state, record.termination_reason) !=
            (sid, "killed", sid, eid, "completed", "unknown") or record.exit_code is not None or not record.output_truncated):
        raise RuntimeError(f"incorrect recovery of interrupted work: {observed}")
    daemon.require_empty()
    return dict(observed, probe_container_removed=True)


def round_trip(client, spec, expected):
    start = time.monotonic()
    token = budget()
    sb = client.create_sandbox(spec, cancel=token)
    created = time.monotonic()
    if sb.state != "running" or sb.image != spec.image or sb.network != "none":
        raise RuntimeError(f"unexpected sandbox state, image or network: {sb}")
    chunks = []
    command = ["sh", "-c", 'printf %s "$1"', "sh", expected]
    ex = client.exec_stream(sb.id, command, cancel=token,
                            on_stdout=lambda text: chunks.append((time.monotonic(), text)))
    finished = time.monotonic()
    if (ex.sandbox_id != sb.id or ex.state != "completed" or ex.termination_reason != "exited" or ex.exit_code != 0
            or ex.stdout != expected or ex.stderr or ex.output_truncated or "".join(text for _, text in chunks) != expected):
        raise RuntimeError(f"{sb.id}: execution or streamed output did not match {expected!r}: {ex}")
    kill(client, sb.id, token)
    end = time.monotonic()
    return {"create_ready_ms": (created - start) * 1000, "first_output_ms": (chunks[0][0] - created) * 1000,
            "command_ms": (finished - created) * 1000, "delete_ms": (end - finished) * 1000,
            "round_ms": (end - start) * 1000}


def steady(daemon, spec, seconds, concurrency, phase, progress):
    samples = [[] for _ in range(concurrency)]
    stop = threading.Event()
    start = time.monotonic()

    def worker(index):
        rows = samples[index]
        with client_for(daemon) as client:
            while not stop.is_set() and time.monotonic() < start + seconds:
                rows.append(round_trip(client, spec, f"ok-{phase['name']}-{index}-{len(rows)}"))
        if not rows and not stop.is_set():
            raise RuntimeError(f"worker {index} completed no round")

    with ThreadPoolExecutor(max_workers=concurrency) as pool:
        futures = {pool.submit(worker, i): i for i in range(concurrency)}
        pending = set(futures)
        try:
            while pending:
                done, pending = wait(pending, timeout=10, return_when=FIRST_EXCEPTION)
                for future in done:
                    try:
                        future.result()
                    except Exception as error:
                        raise RuntimeError(f"{phase['name']} worker {futures[future]}: {error}") from error
                if progress and pending:
                    print(f"{phase['name']}: {sum(map(len, samples))} confirmed rounds", flush=True)
        finally:
            stop.set()
            wait(futures)
            phase.update(rounds_per_worker=[len(rows) for rows in samples], elapsed_s=time.monotonic() - start)
    return [row for rows in samples for row in rows]


def clean(daemon):
    daemon.require_empty()
    gauges = daemon.gauges()
    if any(gauges.values()):
        raise RuntimeError(f"cleanup not confirmed: {gauges}")
    return gauges


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server", type=Path, default=Path("bin/execletd"))
    parser.add_argument("--image", required=True)
    parser.add_argument("--duration", type=float, default=600, help="steady-state admission time, in seconds")
    parser.add_argument("--concurrency", type=int, default=4)
    parser.add_argument("--max-sandboxes", type=int, default=16)
    parser.add_argument("--memory-mb", type=int, default=256)
    parser.add_argument("--nano-cpus", type=int, default=1_000_000_000)
    parser.add_argument("--pids-limit", type=int, default=256)
    parser.add_argument("--journal", action="store_true", help="enable durable state")
    parser.add_argument("--restart", action="store_true", help="crash a busy journaled daemon between steady phases")
    parser.add_argument("--out", type=Path, default=Path("artifacts/soak.json"))
    parser.add_argument("--progress", action="store_true")
    args = parser.parse_args()
    with report(args.out) as record, tempfile.TemporaryDirectory(prefix="execlet-soak-") as tmp:
        if not math.isfinite(args.duration) or args.duration < 30:
            raise ValueError("--duration must be finite and at least 30 seconds")
        if not 1 <= args.concurrency <= min(16, args.max_sandboxes) or min(args.memory_mb, args.nano_cpus, args.pids_limit) < 1:
            raise ValueError("need 1–16 workers, a sandbox slot per worker and positive budgets")
        journal = args.journal or args.restart
        daemon = Daemon(args.server, tmp, state_dir=Path(tmp) / "state" if journal else None)
        image = daemon.image_id(args.image)
        policy = {"max_sandboxes": args.max_sandboxes, "total_memory_mb": args.max_sandboxes * args.memory_mb,
                  "total_nano_cpus": args.max_sandboxes * args.nano_cpus, "total_pids": args.max_sandboxes * args.pids_limit,
                  "allowed_images": [image], "allowed_networks": ["none"]}
        daemon.set_policy(policy)
        spec = CreateSandboxRequest(image=image, memory_mb=args.memory_mb, nano_cpus=args.nano_cpus,
                                    pids_limit=args.pids_limit, network="none", timeout_seconds=3600, idle_timeout_seconds=0)
        record.update(owner=daemon.owner, image=image, server_sha256=checksum(daemon.server), policy=policy,
                      sandbox_spec=spec.to_dict(), journal=journal, restart_tested=args.restart,
                      concurrency=args.concurrency, duration_s=args.duration, phases=[])
        with daemon.running():
            record["disconnect"] = disconnect_probe(daemon, spec)
            record["baseline"] = clean(daemon)
            phases = ("before_restart", "after_restart") if args.restart else ("steady",)
            rows = []
            for index, name in enumerate(phases):
                if index:
                    record["restart"] = restart_probe(daemon, spec)
                phase = {"name": name}
                record["phases"].append(phase)
                rows.extend(steady(daemon, spec, args.duration / len(phases), args.concurrency, phase, args.progress))
                phase["cleanup"] = clean(daemon)
            elapsed = sum(phase["elapsed_s"] for phase in record["phases"])
            record.update(rounds=len(rows), elapsed_s=round(elapsed, 2), throughput_per_min=round(len(rows) * 60 / elapsed, 1),
                          final=phase["cleanup"], owned_after_soak=0)
            for name in rows[0]:
                values = sorted(row[name] for row in rows)
                record[name] = {"samples": len(values), "p50_ms": round(statistics.median(values), 1),
                                "p95_ms": round(values[math.ceil(0.95 * len(values)) - 1], 1), "max_ms": round(values[-1], 1)}
    print(f"{record['rounds']} confirmed rounds, {record['throughput_per_min']}/min, zero owned containers; {args.out}")


if __name__ == "__main__":
    try:
        main()
    except (Error, OSError, ValueError, RuntimeError, subprocess.SubprocessError) as error:
        sys.exit(f"soak: {error}")
