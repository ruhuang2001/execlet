import json
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
from urllib.parse import urlsplit
from urllib.request import Request, urlopen

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "sandbox-python" / "e2e"))
from proxy import Fault, FaultProxy


def main(binary, domain, out):
    source = urlsplit(domain)
    if source.scheme != "http" or source.hostname not in {"127.0.0.1", "localhost"}:
        raise ValueError("cli cleanup E2E requires a local HTTP daemon")
    out = Path(out)
    out.mkdir(parents=True, exist_ok=True)
    proxy = FaultProxy(source.port or 80, out / "requests.jsonl")
    report = []
    path = "/v1/sandboxes/[^/]+"
    headers = {}
    if key := os.environ.get("SANDBOX_API_KEY"):
        headers["Authorization"] = "Bearer " + key

    def direct(sandbox_id, method):
        request = Request(domain + "/v1/sandboxes/" + sandbox_id, headers=headers, method=method)
        with urlopen(request, timeout=10) as response:
            return json.load(response)

    def run(name, command_exit, cleanup, interrupted=False, uncertain=False):
        faults = []
        if interrupted:
            start = Fault("POST", path + "/executions", "barrier_after")
            faults.append(start)
        if uncertain:
            faults.append(Fault("POST", path + "/executions/[^/]+/cancel", "status", status=403))
        if cleanup:
            faults.append(Fault("DELETE", path, cleanup, status=403))
        proxy.arm(name, *faults)
        args = [binary, "--domain", proxy.domain, "run"]
        if not interrupted:
            args.append("--rm")
        cmd = ["sleep", "30"] if interrupted else ["sh", "-c", f"printf result; exit {command_exit}"]
        with subprocess.Popen([*args, "--", *cmd], stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                              text=True) as process:
            try:
                if interrupted:
                    if not start.accepted.wait(20):
                        raise TimeoutError("start did not reach the daemon")
                    process.send_signal(signal.SIGINT)
                stdout, stderr = process.communicate(timeout=25)
            finally:
                if interrupted:
                    start.release.set()
                if process.poll() is None:
                    process.kill()
        match = re.search(r"^sandbox: (sbx_[a-zA-Z0-9]+) \(created;", stderr, re.MULTILINE)
        if not match:
            raise RuntimeError(stderr)
        sid = match.group(1)
        try:
            state = direct(sid, "GET")["state"]
            expected_exit = (1 if uncertain else 130) if interrupted else command_exit
            warning = [line for line in stderr.splitlines() if "cleanup unconfirmed" in line]
            valid_warning = len(warning) == 1 and sid in warning[0] if cleanup else not warning
            primary = ("could not be canceled" if uncertain else "execution canceled") if interrupted else f"exit {command_exit}"
            passed = (process.returncode == expected_exit and primary in stderr and valid_warning
                      and stdout == ("" if interrupted else "result")
                      and state == ("running" if cleanup == "status" else "killed"))
            report.append(dict(name=name, sandbox_id=sid, exit_code=process.returncode, stdout=stdout,
                               stderr=stderr, sandbox_state=state, passed=passed))
            (out / "report.json").write_text(json.dumps(report, indent=2) + "\n")
            if not passed:
                raise AssertionError(report[-1])
        finally:
            direct(sid, "DELETE")

    try:
        run("confirmed", 0, None)
        run("refused-after-success", 0, "status")
        run("refused-after-nonzero", 37, "status")
        run("nonterminal-acknowledgement", 37, "running_snapshot")
        run("interrupted-start", 0, "status", interrupted=True)
        run("uncertain-start", 0, "status", interrupted=True, uncertain=True)
    finally:
        proxy.close()


if __name__ == "__main__":
    main(*sys.argv[1:])
