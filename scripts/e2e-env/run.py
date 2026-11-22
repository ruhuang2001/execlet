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


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def request(method, path, body=None, expected=200, timeout=15):
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(base + path, data=data, method=method,
                                 headers={"Content-Type": "application/json"})
    try:
        response = urllib.request.urlopen(req, timeout=timeout)
    except urllib.error.HTTPError as error:
        response = error
    raw = response.read().decode()
    require(response.code == expected, "environment contract check failed at line 23")
    return json.loads(raw)


def completed(result, expected_exit=0, executions=None):
    deadline = time.monotonic() + 10
    while result["state"] != "completed":
        remaining = deadline - time.monotonic()
        require(remaining > 0, "execution completion deadline exceeded")
        result = request("GET", (executions or path) + "/" + result["id"], timeout=remaining)
        if result["state"] != "completed":
            time.sleep(min(.02, max(0, deadline - time.monotonic())))
    require(result["exit_code"] == expected_exit, f"execution exit code differs from {expected_exit}")
    return result


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
        require("env" not in result, "environment contract check failed at line 37")
        if env and "SECRET" in env:
            require(env["SECRET"] not in json.dumps(result), "environment contract check failed at line 39")
    if expected >= 400:
        require(result["code"] == ("invalid_request" if expected == 400 else "idempotency_conflict"), "environment contract check failed at line 41")
    else:
        result = completed(result)
    report["checks"].append({"name": name, "status": expected, "id": result.get("id")})
    return result


def equals_command(image):
    sandbox_id = request("POST", "/v1/sandboxes", {"image": image, "network": "none"}, 201)["id"]
    executions = "/v1/sandboxes/" + sandbox_id + "/executions"
    try:
        def run(command, expected_exit=0, **options):
            result = request("POST", executions, {"command": command, **options}, 201)
            return completed(result, expected_exit, executions)

        run(["/bin/ln", "-s", "/usr/local/bin/python3", "/tmp/py=probe"])
        run(["/bin/ln", "-s", "/usr/local/bin/python3", "/tmp/-py=probe"])
        env = {"IFS": "space*?", "SHLVL": "73", "PWD": "/logical", "MY-VAR": "example",
               "CONFIG.KEY": "literal", "PATH": "/tmp", "PRIVATE_TEST_VALUE": "synthetic-only"}
        probe = (
            "import os, sys; "
            "assert [os.environ[k] for k in ('IFS','SHLVL','PWD','MY-VAR','CONFIG.KEY')] "
            "== ['space*?', '73', '/logical', 'example', 'literal']; "
            "assert os.getcwd() == '/tmp'; "
            "assert open('/proc/self/cmdline','rb').read().split(bytes([0]))[0].decode() == sys.argv[1]; "
            "assert sys.argv[2:] == ['', 'literal $ words', 'a=b']; "
            "assert sys.stdin.read() == 'input data'; print('ok')"
        )
        for executable in ("py=probe", "./py=probe", "/tmp/py=probe", "-py=probe"):
            done = run([executable, "-c", probe, executable, "", "literal $ words", "a=b"],
                       env=env, cwd="/tmp", stdin="input data")
            require(done["stdout"] == "ok\n" and not done["stderr"], "equals command output differed")
        if image.endswith("-slim"):
            run(["/bin/mv", "/usr/bin/nice", "/tmp/unused-nice"])
            simple_env = {"IFS": "fallback", "PWD": "/logical", "SHLVL": "73", "PATH": "/tmp"}
            probe = ("import os; assert [os.environ[k] for k in ('IFS','PWD','SHLVL')] "
                     "== ['fallback','/logical','73']; print('ok')")
            minimal = run(["py=probe", "-c", probe], env=simple_env, cwd="/tmp")
            require(minimal["stdout"] == "ok\n" and not minimal["stderr"],
                    "minimal image rewrote shell variables")
        missing = run(["/tmp/missing=command"], 127, env={"IFS": "dummy"})
        require(not missing["stdout"] and env["PRIVATE_TEST_VALUE"] not in missing["stderr"],
                "missing equals command exposed environment")
        report["checks"].append({"name": "equals-command", "image": image, "status": "passed"})
    finally:
        request("DELETE", "/v1/sandboxes/" + sandbox_id)


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
    require(replay["id"] == original["id"], "environment contract check failed at line 68")
    start("identity-value", {"A": "changed", "B": "2"}, 409, "identity", "/tmp")
    start("identity-count", {"A": "1"}, 409, "identity", "/tmp")
    start("identity-name", {"C": "1", "B": "2"}, 409, "identity", "/tmp")
    start("identity-cwd", {"A": "1", "B": "2"}, 409, "identity", "/")
    empty = start("empty-omitted", key="empty")
    require(start("empty-map", {}, 200, "empty")["id"] == empty["id"], "environment contract check failed at line 74")
    start("framing-original", {"A": "xB=y", "C": "z"}, key="framing")
    start("framing-different", {"A": "x", "B": "yC=z"}, 409, "framing")
    secret = "env-retention-e2e-secret"
    done = start("secret", {"SECRET": secret, "PAYLOAD": "$(touch /tmp/injected); false"})
    views = [done, request("GET", path), request("GET", path + "/" + done["id"])]
    response = urllib.request.urlopen(base + path + "/" + done["id"] + "/events", timeout=15)
    views.append(response.read().decode())
    require(secret not in json.dumps(views) and '"env"' not in json.dumps(views), "environment contract check failed at line 82")
    probe = request("POST", path, {"command": ["sh", "-c", 'test ! -e /tmp/injected && test -z "' + '$' + '{SECRET+x}"']}, 201)
    completed(probe)
    report["checks"].append({"name": "secrecy-isolation-no-injection", "status": "passed"})
    for image in ("python:3.12-alpine", "python:3.11-slim"):
        equals_command(image)
    report["passed"] = True
finally:
    try:
        if sandbox:
            report["cleanup"] = request("DELETE", "/v1/sandboxes/" + sandbox)["state"]
    finally:
        pathlib.Path(output).write_text(json.dumps(report, indent=2) + "\n")
print("PASS environment boundaries, identity and secrecy")
