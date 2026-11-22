# CLI cleanup failures

The runner invokes the built CLI through a fault proxy against a local Docker-backed daemon. It checks the command's exit status, stdout, stderr and sandbox state before doing its own cleanup.

| Case | Expected evidence |
| --- | --- |
| A successful command and confirmed deletion with `--rm` | Exit 0, exact command output, no cleanup warning, sandbox killed. |
| Deletion rejected after an exit of 0 or 37 | Original status and output, warning on stderr naming the sandbox, sandbox still running. |
| Deletion succeeds but its response reports a running sandbox | Original status and output, warning on stderr, direct inspection finds the sandbox killed. |
| SIGINT after the daemon accepted a start but before the CLI received its response | Keyed recovery cancels the command. Cleanup rejection appears beside the sandbox ID, stdout is empty and the CLI exits 130. |
| The same interruption with an unconfirmed command cancellation | The CLI still attempts cleanup for the sandbox it created. Exit 1 and the original start-recovery error remain observable. |

With a local daemon running, invoke the following command from the repository root. The JSON report records each sandbox ID, exit code, stdout, stderr and observed state. Each case writes a JSON Lines trace of proxy requests and responses. The runner uses the IDs in successful create responses to clean up even if the CLI times out or prints an invalid ID.

```sh
python3 -B scripts/e2e-cli-cleanup.py ./bin/sandboxctl "${SANDBOX_DOMAIN:-http://localhost:8080}" artifacts/cli-cleanup
```
