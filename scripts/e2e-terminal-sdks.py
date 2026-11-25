"""Exercise public Python/TS SDK terminals and stat, and recover sandbox after detached shell exits."""

import argparse, json, os, secrets, socket, subprocess, sys, tempfile, time
from pathlib import Path
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import threading

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "sandbox-python/src"))
from execlet import Client, CreateSandboxRequest


def run(args):
    if __debug__ is False:
        raise RuntimeError("assertions are required")
    args.out.mkdir(parents=True, exist_ok=True)
    owner = "terminal-sdks-" + secrets.token_hex(5)
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        port = s.getsockname()[1]
    domain = f"http://127.0.0.1:{port}"
    with (
        tempfile.TemporaryDirectory() as tmp,
        (args.out / "daemon.log").open("w") as log,
    ):
        proc = None

        def launch():
            nonlocal proc
            proc = subprocess.Popen(
                [
                    str(args.server.resolve()),
                    "-addr",
                    domain[7:],
                    "-owner",
                    owner,
                    "-state-dir",
                    tmp,
                ],
                stdout=log,
                stderr=log,
            )
            import urllib.request

            for _ in range(120):
                try:
                    with urllib.request.urlopen(domain + "/readyz") as r:
                        r.read()
                    return
                except OSError:
                    time.sleep(0.04)
            raise TimeoutError("execletd")

        try:
            launch()
            with Client(domain=domain, max_retries=0) as c:
                caps = c.get_capabilities().capabilities
                assert {"files.stat.v1", "terminals.v1", "sandbox.purge.v1"}.issubset(
                    caps
                )
                sb = c.create_sandbox(
                    CreateSandboxRequest(image="alpine:3.21", network="none")
                )
                path = "/tmp/document"
                entry = c.write_file(sb.id, path, b"sample")
                assert c.stat_file(sb.id, path) == entry
                tid = c.open_terminal(sb.id)
                c.resize_terminal(sb.id, tid, 40, 91)
                c.terminal_input(
                    sb.id, tid, b'printf "DEFAULT=%s " "$TERM"; stty size; exit\n'
                )
                text = b"".join(c.stream_terminal(sb.id, tid)).decode()
                assert "DEFAULT=xterm-256color 40 91" in text, text
                for i in range(10):
                    c.open_terminal(sb.id, command=["true"])
                time.sleep(0.2)
                proc.kill()
                proc.wait()
                launch()
                assert c.get_sandbox(sb.id).state == "running"
                c.destroy_sandbox(sb.id)
            js = """
import assert from 'node:assert/strict';
import {Client} from './sandbox-ts/dist/index.js';
const c=new Client({domain:process.env.DOMAIN,maxRetries:-1});
const sb=await c.createSandbox({image:'alpine:3.21',network:'none'});
try {
const stat=await c.statFile(sb.id,'/tmp');
assert.equal(stat.type,'dir');
const id=await c.openTerminal(sb.id);
await c.resizeTerminal(sb.id,id,42,89);
await c.terminalInput(sb.id,id,new TextEncoder().encode('printf "TERM=%s " "$TERM"; stty size; exit\\n'));
let data='';
for await(const chunk of c.streamTerminal(sb.id,id)) data+=new TextDecoder().decode(chunk);
assert.match(data,/TERM=xterm-256color 42 89/);
await c.closeTerminal(sb.id,id);
console.log(JSON.stringify({output:data,stat:stat.type}));
}finally{await c.destroySandbox(sb.id)}
"""
            ts = json.loads(
                subprocess.check_output(
                    ["node", "--input-type=module", "-e", js],
                    cwd=ROOT,
                    env={**os.environ, "DOMAIN": domain},
                    text=True,
                )
            )

            class Previous(BaseHTTPRequestHandler):
                def do_GET(self):
                    data = (
                        {"capabilities": []}
                        if self.path.endswith("capabilities")
                        else {"id": "old", "state": "running"}
                    )
                    self.send_response(200)
                    self.send_header("Content-Type", "application/json")
                    self.end_headers()
                    self.wfile.write(json.dumps(data).encode())

                def do_PATCH(self):
                    self.rfile.read(int(self.headers.get("Content-Length", 0)))
                    self.do_GET()

                def do_PUT(self):
                    self.rfile.read(int(self.headers.get("Content-Length", 0)))
                    self.send_response(204)
                    self.end_headers()

                def log_message(self, *args):
                    pass

            with ThreadingHTTPServer(("127.0.0.1", 0), Previous) as old:
                threading.Thread(target=old.serve_forever, daemon=True).start()
                legacy = "http://127.0.0.1:" + str(old.server_address[1])
                try:
                    from execlet.e2b import Sandbox as E2B

                    adapter = E2B.connect("old", endpoint=legacy)
                    info = adapter.files.write("/tmp/a", b"ok")
                    assert (
                        info.name == "a" and info.type is None and info.path == "/tmp/a"
                    )
                    with Client(domain=legacy) as c:
                        assert c.write_file("old", "/tmp/a", b"ok") is None
                    subprocess.run(
                        [
                            "node",
                            "--input-type=module",
                            "-e",
                            """import {Client} from './sandbox-ts/dist/index.js';import assert from 'node:assert/strict';assert.equal(await new Client({domain:process.env.DOMAIN}).writeFile('old','/tmp/a',new Uint8Array([12])),undefined); const {Sandbox}=await import('./sandbox-ts/dist/e2b.js'); const sb=await Sandbox.connect('old',{endpoint:process.env.DOMAIN});const w=await sb.files.write('/tmp/a','ok');assert.equal(w.name,'a');assert.equal(w.type,undefined)""",
                        ],
                        cwd=ROOT,
                        env={**os.environ, "DOMAIN": legacy},
                        check=True,
                    )
                finally:
                    old.shutdown()
            report = {
                "capabilities": caps,
                "python_stdout": text,
                "typescript": ts,
                "exited_without_delete": 10,
                "idle_sandbox_recovered": True,
                "legacy_204_success": True,
            }
            (args.out / "acceptance.json").write_text(
                json.dumps(report, indent=2) + "\n"
            )
        finally:
            if proc is not None and proc.poll() is None:
                proc.terminate()
                proc.wait(timeout=20)
            leftovers = subprocess.check_output(
                [
                    "docker",
                    "ps",
                    "-aq",
                    "--filter",
                    "label=execlet.owner=" + owner,
                ],
                text=True,
            ).split()
            if leftovers:
                subprocess.run(["docker", "rm", "-f", *leftovers], check=True)


if __name__ == "__main__":
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--out", type=Path, default=Path("artifacts/terminal-sdks"))
    p.add_argument("--server", type=Path, default=Path("bin/execletd"))
    run(p.parse_args())
