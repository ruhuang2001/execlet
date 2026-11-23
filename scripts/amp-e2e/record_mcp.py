"""Pass Amp's stdio to sandboxctl and retain only completed MCP tool results."""

import hashlib
import json
import subprocess
import sys
import threading


def compact(value):
    if isinstance(value, str) and len(value) > 2200:
        return {
            "bytes": len(value.encode()),
            "sha256": hashlib.sha256(value.encode()).hexdigest(),
            "tail": value[-900:],
        }
    if isinstance(value, dict):
        return {key: compact(v) for key, v in value.items()}
    if isinstance(value, list):
        return [compact(v) for v in value]
    return value


def main():
    logfile, *argv = sys.argv[1:]
    p = subprocess.Popen(argv, stdin=subprocess.PIPE, stdout=subprocess.PIPE)
    pending = {}

    def send():
        try:
            for line in sys.stdin.buffer:
                msg = json.loads(line)
                if msg.get("method") == "tools/call":
                    pending[msg.get("id")] = msg["params"]
                p.stdin.write(line)
                p.stdin.flush()
        finally:
            p.stdin.close()

    threading.Thread(target=send, daemon=True).start()
    with open(logfile, "a", buffering=1) as log:
        for line in p.stdout:
            msg = json.loads(line)
            call = pending.pop(msg.get("id"), None)
            if call:
                result = msg.get("result", {})
                row = {
                    "tool": call["name"],
                    "arguments": call.get("arguments", {}),
                    "value": result.get("structuredContent"),
                    "error": result.get("isError", False),
                }
                if row["error"]:
                    row["detail"] = result.get("content")
                if row["tool"] == "create_sandbox" and not row["error"]:
                    cid = "tiny-sandbox-" + row["value"]["sandbox_id"]
                    row["docker_network_mode"] = subprocess.check_output(
                        [
                            "docker",
                            "inspect",
                            "--format",
                            "{{.HostConfig.NetworkMode}}",
                            cid,
                        ],
                        text=True,
                    ).strip()
                log.write(json.dumps(compact(row)) + "\n")
            sys.stdout.buffer.write(line)
            sys.stdout.buffer.flush()
    sys.exit(p.wait())


if __name__ == "__main__":
    main()
