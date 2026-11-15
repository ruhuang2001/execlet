"""Real HTTP/Docker checks for env bounds and retained invocation identity."""
import json
import pathlib
import sys
import time
import urllib.error
import urllib.request

base, output = sys.argv[1:]
report = {"command": "python3 scripts/e2e-env/run.py <daemon-url> <report.json>", "checks": []}
sandbox = None


def request(method, path, body=None, expected=200):
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(base + path, data=data, method=method,
                                 headers={"Content-Type": "application/json"})
    try:
        response = urllib.request.urlopen(req, timeout=15)
    except urllib.error.HTTPError as error:
        response = error
    raw = response.read().decode()
    assert response.code == expected, (path, response.code, expected, raw)
    return json.loads(raw)


def start(name, env=None, expected=201, key=None, cwd=None):
    body = {"command": ["true"]}
    if env is not None:
        body["env"] = env
    if key is not None:
        body["idempotency_key"] = key
    if cwd is not None:
        body["cwd"] = cwd
    result = request("POST", path, body, expected)
    if expected < 400:
        assert "env" not in result, result
        if env and "SECRET" in env:
            assert env["SECRET"] not in json.dumps(result), result
    if expected >= 400:
        assert result["code"] == ("invalid_request" if expected == 400 else "idempotency_conflict"), result
    else:
        for _ in range(200):
            result = request("GET", path + "/" + result["id"])
            if result["state"] == "completed":
                break
            time.sleep(.02)
        assert result["state"] == "completed" and result["exit_code"] == 0, result
    report["checks"].append({"name": name, "status": expected, "id": result.get("id")})
    return result


try:
    sandbox = request("POST", "/v1/sandboxes", {"timeout_seconds": 0, "network": "none"}, 201)["id"]
    path = "/v1/sandboxes/" + sandbox + "/executions"
    for label, good, bad in [
        ("key-bytes", {"K" * 64: "v"}, {"K" * 65: "v"}),
        ("utf8-key-bytes", {"é" * 32: "v"}, {"é" * 32 + "a": "v"}),
        ("value-bytes", {"K": "v" * 8192}, {"K": "v" * 8193}),
        ("entries", {f"K{i}": "" for i in range(64)}, {f"K{i}": "" for i in range(65)}),
        ("total-bytes", {f"K{i}": "v" * (8192 - 3) for i in range(4)},
         {**{f"K{i}": "v" * (8192 - 3) for i in range(4)}, "K0": "v" * (8192 - 2)}),
    ]:
        start(label + "-exact", good)
        start(label + "-over", bad, 400)
    original = start("identity-original", {"B": "2", "A": "1"}, key="identity", cwd="/tmp")
    replay = start("identity-order", {"A": "1", "B": "2"}, 200, "identity", "/tmp")
    assert replay["id"] == original["id"]
    start("identity-value", {"A": "changed", "B": "2"}, 409, "identity", "/tmp")
    start("identity-count", {"A": "1"}, 409, "identity", "/tmp")
    start("identity-name", {"C": "1", "B": "2"}, 409, "identity", "/tmp")
    start("identity-cwd", {"A": "1", "B": "2"}, 409, "identity", "/")
    empty = start("empty-omitted", key="empty")
    assert start("empty-map", {}, 200, "empty")["id"] == empty["id"]
    start("framing-original", {"A": "xB=y", "C": "z"}, key="framing")
    start("framing-different", {"A": "x", "B": "yC=z"}, 409, "framing")
    secret = "env-retention-e2e-secret"
    done = start("secret", {"SECRET": secret, "PAYLOAD": "$(touch /tmp/injected); false"})
    views = [done, request("GET", path), request("GET", path + "/" + done["id"])]
    response = urllib.request.urlopen(base + path + "/" + done["id"] + "/events", timeout=15)
    views.append(response.read().decode())
    assert secret not in json.dumps(views) and '"env"' not in json.dumps(views)
    probe = request("POST", path, {"command": ["sh", "-c", 'test ! -e /tmp/injected && test -z "' + '$' + '{SECRET+x}"']}, 201)
    for _ in range(200):
        probe = request("GET", path + "/" + probe["id"])
        if probe["state"] == "completed":
            break
        time.sleep(.02)
    assert probe["exit_code"] == 0, probe
    report["checks"].append({"name": "secrecy-isolation-no-injection", "status": "passed"})
    report["passed"] = True
finally:
    try:
        if sandbox:
            report["cleanup"] = request("DELETE", "/v1/sandboxes/" + sandbox)["state"]
    finally:
        pathlib.Path(output).write_text(json.dumps(report, indent=2) + "\n")
print("PASS environment boundaries, identity and secrecy")
