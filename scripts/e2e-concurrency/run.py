"""Real daemon/Docker concurrency matrix; run by scripts/e2e.sh."""
import concurrent.futures
import json
import pathlib
import subprocess
import sys
import time
import urllib.error
import urllib.request

base, artifact = sys.argv[1:]
report = {"checks": [], "owned": [], "cleanup": []}
owned = []


def call(method, path, body=None, expected=200):
    req = urllib.request.Request(base + path, method=method,
        data=None if body is None else json.dumps(body).encode(), headers={"Content-Type": "application/json"})
    try:
        r = urllib.request.urlopen(req, timeout=25)
    except urllib.error.HTTPError as e:
        r = e
    result = json.loads(r.read())
    assert r.status == expected, (method, path, r.status, expected, result)
    return result


def create(limit=2, **kw):
    sb = call("POST", "/v1/sandboxes", dict({"timeout_seconds": 0, "network": "none", "max_concurrent_executions": limit}, **kw), 201)
    owned.append(sb["id"])
    assert sb["max_concurrent_executions"] == limit
    return sb["id"]


def path(sb, ex=None):
    return "/v1/sandboxes/" + sb + "/executions" + ("/" + ex if ex else "")


def start(sb, command=None, expected=201, **kw):
    return call("POST", path(sb), dict(command=command or ["sleep", "30"], **kw), expected)


def get(sb, ex):
    return call("GET", path(sb, ex["id"]))


def finish(sb, ex, reason=None):
    deadline = time.monotonic() + 15
    while time.monotonic() < deadline:
        out = get(sb, ex)
        if out["state"] == "completed":
            assert reason is None or out["termination_reason"] == reason, out
            return out
        time.sleep(.03)
    raise AssertionError(("did not finish", ex))


def cancel(sb, ex):
    call("POST", path(sb, ex["id"]) + "/cancel")
    return finish(sb, ex, "canceled")


def check(name, fn):
    evidence = fn()
    report["checks"].append({"name": name, "passed": True, "evidence": evidence})
    pathlib.Path(artifact).write_text(json.dumps(report, indent=2))
    print("PASS", name, flush=True)


def bounds():
    for n in [0, -1, 9]:
        assert call("POST", "/v1/sandboxes", {"max_concurrent_executions": n}, 400)["code"] == "invalid_request"
    for n in [1, 8]:
        sb = create(n)
        assert call("GET", "/v1/sandboxes/" + sb)["max_concurrent_executions"] == n
    sb = call("POST", "/v1/sandboxes", {"timeout_seconds": 0}, 201)
    owned.append(sb["id"])
    assert sb["max_concurrent_executions"] == 1
    return [1, 8]


def admission():
    sb = create()
    a = start(sb, idempotency_key="a")
    b = start(sb)
    assert get(sb, a)["state"] == get(sb, b)["state"] == "running"
    assert start(sb, expected=200, idempotency_key="a")["id"] == a["id"]
    start(sb, expected=409)
    cancel(sb, a)
    assert get(sb, b)["state"] == "running"
    finish(sb, start(sb, ["true"]))
    cancel(sb, b)
    # Concurrent HTTP admission at eight slots never accepts a ninth.
    sb = create(8)
    def attempt(_):
        try:
            return start(sb)
        except AssertionError as e:
            assert "409" in str(e) and "busy" in str(e), e
            return None
    with concurrent.futures.ThreadPoolExecutor(max_workers=12) as pool:
        accepted = [x for x in pool.map(attempt, range(12)) if x]
    assert len(accepted) == 8, accepted
    call("DELETE", "/v1/sandboxes/" + sb)
    for ex in accepted:
        finish(sb, ex, "canceled")
    return {"accepted": len(accepted)}


def deadlines():
    sb = create(3)
    a = start(sb, timeout_seconds=1)
    b = start(sb, timeout_seconds=2)
    sibling = start(sb)
    finish(sb, a, "timeout")
    finish(sb, b, "timeout")
    assert get(sb, sibling)["state"] == "running"
    cancel(sb, sibling)
    a = start(sb, timeout_seconds=1)
    b = start(sb, timeout_seconds=1)
    call("POST", "/v1/sandboxes/" + sb + "/pause")
    time.sleep(1.2)
    assert get(sb, a)["state"] == get(sb, b)["state"] == "running"
    call("POST", "/v1/sandboxes/" + sb + "/resume")
    finish(sb, a, "timeout")
    finish(sb, b, "timeout")
    return {"deadlines": 4}


def pause_and_eviction():
    sb = create()
    command = ["sh", "-c", "while :; do echo progress; sleep .1; done"]
    a, b = start(sb, command), start(sb, command)
    time.sleep(.3)
    call("POST", "/v1/sandboxes/" + sb + "/pause")
    time.sleep(.2)
    before = [get(sb, ex)["stdout"] for ex in (a, b)]
    time.sleep(.4)
    assert before == [get(sb, ex)["stdout"] for ex in (a, b)]
    call("POST", "/v1/sandboxes/" + sb + "/resume")
    time.sleep(.3)
    assert all(len(get(sb, ex)["stdout"]) > len(old) for ex, old in zip((a, b), before))
    subprocess.run(["docker", "kill", "tiny-sandbox-" + sb], check=True, capture_output=True)
    finish(sb, a, "evicted")
    finish(sb, b, "evicted")
    return {"sandbox": sb}


def retention_idle_unknown():
    sb = create(idle_timeout_seconds=1, pids_limit=4096)
    active = start(sb, ["sleep", "300"])
    for _ in range(310):
        assert finish(sb, start(sb, ["true"]))["exit_code"] == 0
    assert get(sb, active)["state"] == "running"
    probe = finish(sb, start(sb, ["sh", "-c", "find /run/tiny-sandbox -type f | wc -l"]))
    assert probe["exit_code"] == 0 and int(probe["stdout"].strip()) <= 3, probe
    records = call("GET", path(sb))["executions"]
    assert len(records) <= 200, len(records)
    cancel(sb, active)
    time.sleep(2.2)
    assert call("GET", "/v1/sandboxes/" + sb)["state"] == "killed"
    # Keep history-count and global byte-pressure evidence independently clear.
    sb = create()
    active = start(sb, ["sleep", "300"])
    output_bytes = 0
    output_sizes = []
    for _ in range(160):
        output = finish(sb, start(sb, ["head", "-c", "1048576", "/dev/zero"]))
        assert output["exit_code"] == 0, output
        output_bytes += len(output["stdout"].encode())
        output_sizes.append({"id": output["id"], "bytes": len(output["stdout"].encode()), "stderr": output["stderr"], "exit_code": output["exit_code"]})
        if output_bytes > 70 * 1048576:
            break
    report["output_pressure"] = output_sizes
    assert output_bytes > 64 * 1048576, output_bytes
    time.sleep(3)
    assert get(sb, active)["state"] == "running", "output pruning lost active record"
    after_pressure = call("GET", path(sb))["executions"]
    assert len(after_pressure) < len(output_sizes) + 1, "global output retention did not prune history"
    out = finish(sb, start(sb, ["sh", "-c", "exit 137"]), "unknown")
    assert out["exit_code"] == 137
    cancel(sb, active)
    return {"sequential_successes": 310, "pid_files": int(probe["stdout"].strip()), "retained": len(records), "after_output_pressure": len(after_pressure), "unknown": out}


def paused_missing_cancel():
    sb = create()
    a, b = start(sb), start(sb)
    call("POST", "/v1/sandboxes/" + sb + "/pause")
    subprocess.run(["docker", "rm", "-f", "tiny-sandbox-" + sb], check=True, capture_output=True)
    # A sweep may win the race and make cancel return the terminal record.
    try:
        call("POST", path(sb, a["id"]) + "/cancel")
    except AssertionError as error:
        assert "409" in str(error) and "invalid_state" in str(error) and "evicted" in str(error), error
    finish(sb, a, "evicted")
    finish(sb, b, "evicted")
    return {"target": a["id"], "sibling": b["id"]}


def lifetime_and_fallback():
    sb = create(timeout_seconds=1)
    a, b = start(sb), start(sb)
    finish(sb, a, "timeout")
    finish(sb, b, "timeout")
    evidence = []
    for reason in ["canceled", "timeout"]:
        sb = create()
        # Remove this command's PID file before its sibling starts. The real
        # signal helper cannot find it, forcing actual container removal.
        a = start(sb, ["sh", "-c", "rm -f /run/tiny-sandbox/*; echo ready; sleep 60"],
                  **({"timeout_seconds": 1} if reason == "timeout" else {}))
        deadline = time.monotonic() + 5
        while "ready" not in get(sb, a)["stdout"]:
            assert time.monotonic() < deadline
            time.sleep(.02)
        b = start(sb, ["sleep", "60"])
        if reason == "canceled":
            call("POST", path(sb, a["id"]) + "/cancel")
        # Timeout's signal attempts each retry a missing PID for three seconds,
        # plus the normal grace/deadline. Allow the complete escalation budget.
        deadline = time.monotonic() + 25
        while get(sb, b)["state"] != "completed":
            assert time.monotonic() < deadline, get(sb, b)
            time.sleep(.1)
        finish(sb, a, reason)
        finish(sb, b, reason)
        assert call("GET", "/v1/sandboxes/" + sb)["state"] == "killed"
        evidence.append({"reason": reason, "target": a["id"], "sibling": b["id"]})
    return evidence


try:
    check("bounds-and-default", bounds)
    check("admission-overlap-replay-cancel-kill", admission)
    check("all-deadlines-and-paused-deadlines", deadlines)
    check("freeze-resume-external-eviction", pause_and_eviction)
    check("active-retention-idle-unknown137", retention_idle_unknown)
    check("lifetime-and-container-removal-fallback", lifetime_and_fallback)
    check("paused-missing-container-cancel", paused_missing_cancel)
    report["passed"] = True
except Exception as error:
    report["passed"] = False
    report["error"] = str(error)
    raise
finally:
    for sb in owned:
        report["cleanup"].append({"sandbox": sb, "state": call("DELETE", "/v1/sandboxes/" + sb)["state"]})
    report["owned"] = owned
    pathlib.Path(artifact).write_text(json.dumps(report, indent=2) + "\n")
