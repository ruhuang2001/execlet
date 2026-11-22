"""Exercise the shell CLI, PTY reattachment, resizing, binary SSE and process cleanup."""

import json
import re
import secrets
import socket
import subprocess
import time
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
OUT = ROOT / "artifacts/terminal"


def main():
    OUT.mkdir(parents=True, exist_ok=True)
    owner = "pty-" + secrets.token_hex(5)
    with socket.socket() as probe:
        probe.bind(("127.0.0.1", 0))
        address = "127.0.0.1:" + str(probe.getsockname()[1])
    domain = "http://" + address

    def request(method, path, body=None):
        if isinstance(body, dict):
            body = json.dumps(body).encode()
        req = urllib.request.Request(domain + path, method=method, data=body)
        with urllib.request.urlopen(req, timeout=10) as response:
            raw = response.read()
            return json.loads(raw) if raw else None

    with (OUT / "daemon.log").open("w") as log:
        proc = subprocess.Popen(
            [ROOT / "bin/sandboxd", "-owner", owner, "-addr", address],
            stdout=log,
            stderr=log,
        )
        try:
            for _ in range(80):
                try:
                    request("GET", "/readyz")
                    break
                except OSError:
                    time.sleep(0.05)
            sb = request(
                "POST", "/v1/sandboxes", {"image": "alpine:3.21", "network": "none"}
            )
            assert sb["agent_mode"] == "guest" and not sb["protected"]
            sid = sb["id"]
            shell = [ROOT / "bin/sandboxctl", "--domain", domain, "shell", sid]

            def session(args, data):
                child = subprocess.run(
                    args,
                    input=data,
                    stdout=subprocess.PIPE,
                    stderr=subprocess.PIPE,
                    timeout=15,
                )
                assert child.returncode == 0, child
                return child

            first = session(shell, b"cd /tmp\nexport SAVED=forty\n\x1d")
            tid = re.search(rb"terminal (term_[0-9a-f]+)", first.stderr)[1].decode()
            path = "/v1/sandboxes/" + sid + "/terminals/" + tid
            request("POST", path + "/resize", {"rows": 39, "cols": 93})
            second = session(
                shell + [tid],
                b'printf \'state=%s_%s\\n\' "$SAVED" "$PWD"; stty size; exit\n',
            )
            output = second.stdout.decode()
            assert "state=forty_/tmp" in output and "39 93" in output, output
            next_terminal = request(
                "POST", "/v1/sandboxes/" + sid + "/terminals", {"command": ["/bin/sh"]}
            )["terminal_id"]
            tpath = "/v1/sandboxes/" + sid + "/terminals/" + next_terminal
            request(
                "POST",
                tpath + "/input",
                b"sleep 3519 & echo $! > /tmp/terminal-child\n",
            )
            container = "tiny-sandbox-" + sid
            for _ in range(50):
                if (
                    subprocess.run(
                        [
                            "docker",
                            "exec",
                            container,
                            "test",
                            "-s",
                            "/tmp/terminal-child",
                        ]
                    ).returncode
                    == 0
                ):
                    break
                time.sleep(0.05)
            request("DELETE", tpath)
            for _ in range(30):
                missing = (
                    subprocess.run(
                        [
                            "docker",
                            "exec",
                            container,
                            "sh",
                            "-c",
                            "test ! -e /proc/$(cat /tmp/terminal-child)",
                        ]
                    ).returncode
                    == 0
                )
                if missing:
                    break
                time.sleep(0.05)
            assert missing, "terminal child still present"
            request("DELETE", "/v1/sandboxes/" + sid)
            report = dict(
                terminal_id=tid,
                output=output,
                process_cleaned=True,
                protected=sb["protected"],
            )
            (OUT / "acceptance.json").write_text(json.dumps(report, indent=2) + "\n")
        finally:
            proc.terminate()
            proc.wait(timeout=15)


if __name__ == "__main__":
    main()
