# Acceptance failure models

Each suite below states which wrong implementation it must catch. Assertions live in the suite. Use this page to find a suite and the failure it exposes. Unless a row says otherwise, every suite runs against a private `execletd` with real Docker, checks cleanup by its own owner label, and writes evidence under `artifacts/`.

| Area | Wrong implementation the suite must expose | Entry point |
| --- | --- | --- |
| Lifecycle, execution, CLI | Pause that only changes a state field, exit 137 reported as OOM, cancellation that leaves child processes, orphans that survive a restart | `bash scripts/e2e.sh artifacts/e2e` |
| Real Docker backend | The same lifecycle and environment failures, isolated from HTTP; a conflicting container name removed without verifying ownership | `go test -tags docker ./internal/dockerbackend -run '^TestReal'` (run by `e2e.sh`) |
| Environment and working directory | Limits off by one byte, replay that ignores env or cwd, raw env retained in records, an executable named `A=B` treated as an assignment | `scripts/e2e-env/run.py` (run by `e2e.sh`) |
| CLI `--rm` cleanup | A failed or unconfirmed delete hidden behind the command's exit status; SIGINT during an accepted start that skips cleanup | `scripts/e2e-cli-cleanup.py` (run by `e2e.sh`) |
| [Execution reliability](reliability-failures.md) | A lost start response that runs the command twice, a stalled Docker handshake that blocks other sandboxes, failed removal that frees admission early | `scripts/e2e-operations.py` |
| One-shot stdin | Input interpreted by a shell, echoed in JSON or SSE, left staged after exit, cancel or timeout, or accepted above 1 MiB | `bash scripts/e2e-stdin.sh artifacts/stdin-e2e` |
| Files and tar archives | Listings that follow child symlinks or skip pages, partial writes from a tar that fails validation later, unsafe links, unbounded scans | `bash scripts/e2e-files.sh artifacts/files-e2e` |
| Request IDs, readiness and logs | Readiness that passes when Docker is down, IDs missing on SSE or panics, logs containing commands, env or paths | `bash scripts/e2e-observability.sh artifacts/observability-e2e` |
| Guest control transport | A lost start acknowledgement that runs twice, an oversized frame that wedges the relay, an unknown architecture or protocol version reported as protected | `python3 scripts/guest-transport-probe.py` (run by `make feature-e2e`) |
| Python SDK | Retried sandbox creation, a reused client after `fork`, cursors advanced by partial SSE frames, callback errors that cancel remote work | `bash scripts/e2e-python.sh artifacts/python-e2e` |
| TypeScript SDK | Lost or duplicated output after reconnect, `AbortSignal` that does not confirm cancellation, a browser bundle that requires Node globals | `bash scripts/e2e-typescript.sh artifacts/typescript-e2e` |
| E2B compatibility harness | A missing adapter or narrowed selection reported as passed, cleanup outside its owner label, provider differences hidden by normalization | `scripts/e2b-compatibility/installed.sh` ([instructions](../../scripts/e2b-compatibility/README.md)) |
| [E2B background commands](e2b-background-failures.md) | Work that dies with its client, a reconnect that changes the deadline, a stale handle that signals a reused PID | `installed.sh … --profile background` |
| MCP workspace | Sync that misses deletions or type changes, recovery that adopts another workspace's sandbox, unchanged syncs that still touch the guest | `scripts/e2e-mcp.py`, `scripts/e2e-mcp-workflow.py` ([MCP checks](../../CONTRIBUTING.md#mcp-acceptance-and-measurements)) |

A suite that cannot reach its prerequisite (Docker, an image, a pinned reference) reports `not run`, not success. Fault proxies wrap real server or Docker responses; there is no fake-only acceptance path.
