# Acceptance failure models

Each suite below states which wrong implementation it must catch. Assertions live in the suite. Use this page to find a suite and the failure it exposes. Unless a row says otherwise, every suite runs against a private `execletd` with real Docker, checks cleanup by its own owner label, and writes evidence under `artifacts/`.

| Area | Wrong implementation the suite must expose | Entry point |
| --- | --- | --- |
| Lifecycle, execution, CLI | Pause that only changes a state field, exit 137 reported as OOM, cancellation that leaves child processes, orphans that survive a restart | `bash scripts/e2e.sh artifacts/e2e` |
| CLI Python image | Default `run -- code` needs a network download inside the guest to create a venv, or no longer includes pip | `scripts/e2e.sh` (`python-image`) |
| Real Docker backend | The same lifecycle and environment failures, isolated from HTTP; a conflicting container name removed without verifying ownership | `go test -tags docker ./internal/dockerbackend -run '^TestReal'` (run by `e2e.sh`) |
| Environment and working directory | Limits off by one byte, replay that ignores env or cwd, raw env retained in records, an executable named `A=B` treated as an assignment | `scripts/e2e-env/run.py` (run by `e2e.sh`) |
| CLI `--rm` cleanup | A failed or unconfirmed delete hidden behind the command's exit status; SIGINT during an accepted start that skips cleanup | `scripts/e2e-cli-cleanup.py` (run by `e2e.sh`) |
| [Execution reliability](reliability-failures.md) | A lost start response that runs the command twice, a stalled Docker handshake that blocks other sandboxes, failed removal that frees admission early | `scripts/e2e-operations.py` |
| One-shot stdin | Input interpreted by a shell, echoed in JSON or SSE, left staged after exit, cancel or timeout, or accepted above 1 MiB | `bash scripts/e2e-stdin.sh artifacts/stdin-e2e` |
| Files and tar archives | Listings that follow child symlinks or skip pages, partial writes from a tar that fails validation later, unsafe links, unbounded scans | `bash scripts/e2e-files.sh artifacts/files-e2e` |
| Request IDs, readiness and logs | Readiness that passes when Docker is down, IDs missing on SSE or panics, logs containing commands, env or paths | `bash scripts/e2e-observability.sh artifacts/observability-e2e` |
| Guest control transport | A lost start acknowledgement that runs twice, an oversized frame that wedges the relay, an unknown architecture or protocol version reported as protected | `python3 scripts/guest-transport-probe.py` (run by `make feature-e2e`) |
| Docker runtimes | Guest progress during pause, missing archive data, wrong task-limit or OOM attribution, unreaped guest subprocesses | `python3 scripts/e2e-runtime.py` on a Linux Docker host with cgroup v2 |
| Persistent volumes | Shared or lost data, writes to a read-only mount, old data destroyed by a failed overwrite, a delayed create that releases its volume early, backing blocks or loop devices left behind | `python3 scripts/e2e-volumes.py` as root on the Linux Docker host |
| Offline Chromium | Screenshot already present before execution, a bridge network, corrupted or mismatched PNG | `make browser-e2e` |
| Python SDK | Retried sandbox creation, a reused client after `fork`, cursors advanced by partial SSE frames, callback errors that cancel remote work | `bash scripts/e2e-python.sh artifacts/python-e2e` |
| TypeScript SDK | Lost or duplicated output after reconnect, `AbortSignal` that does not confirm cancellation, a browser bundle that requires Node globals | `bash scripts/e2e-typescript.sh artifacts/typescript-e2e` |
| E2B compatibility harness | A missing adapter or narrowed selection reported as passed, cleanup outside its owner label, provider differences hidden by normalization | `scripts/e2b-compatibility/installed.sh` ([instructions](../../scripts/e2b-compatibility/README.md)) |
| [E2B background commands](e2b-background-failures.md) | Work that dies with its client, a reconnect that changes the deadline, a stale handle that signals a reused PID | `installed.sh … --profile background` |
| MCP workspace | Sync that misses deletions or type changes, recovery that adopts another workspace's sandbox, unchanged syncs that still touch the guest | `scripts/e2e-mcp.py`, `scripts/e2e-mcp-workflow.py` ([MCP checks](../../CONTRIBUTING.md#mcp-acceptance-and-measurements)) |
| Release archive | An archive that needs a source checkout to start, prints something other than what its documentation promises, exits 0 where it promises failure, ships an extra file, or pairs binaries whose versions disagree | `make release-preflight` |
| Installed repair loop | A failing test that already passes, a repair applied only inside the sandbox, a sync that re-uploads unchanged files, a session that leaves a container | `make pilot` |
| Capacity and recovery | A command that stops when its client disconnects, a crash that reports a real exit code instead of unknown, a sandbox adopted with work in flight, a soak that leaves cleanup pending | `make soak` |

A suite that cannot reach its prerequisite (Docker, an image, a pinned reference) reports `not run`, not success. Fault proxies wrap real server or Docker responses; there is no fake-only acceptance path.

## Check a release candidate

From the checkout matching the candidate, run `make release-preflight` with a
GoReleaser `dist/` (or set `DIST` to another snapshot directory). This checks
the SHA-256 manifest, member list and binary architecture of all four archives.
Only the native pair is executed: from a directory containing just the two
binaries, the check runs the `execletctl run` examples from the packaged README
and the matching quickstart, including the documented `doctor` and timeout.
`ARCHIVE=/path/to/native.tar.gz` limits the check to that archive; it still
requires an adjacent `checksums.txt`. A matching checksum is an integrity check,
not a signature. The installer and its release selection are tested separately
with `python3 scripts/test-install.py -v`.

CI reuses the same snapshot's binaries, pytest fixture and the image ID built
by the offline MCP check for the pilot and [soak](../operations.md#single-host-soak).
For local source builds run `make build mcp-image` first. To use unpacked
binaries instead, set `CLI`, `SERVER` and `MCP_EXAMPLES`, then run
`make mcp-image pilot soak`. With the snapshot and image available, run the
failure controls with `python3 -B scripts/test-host-acceptance.py -v`.

The three JSON reports default to `artifacts/` (`ARTIFACTS` overrides it).
After parsing the arguments and checking the output location, each run replaces
its report with `passed: false`. The report changes to `passed: true` only after
the run and cleanup both succeed. Always check the command's exit status: a
usage error can occur before a report is written.

## Try a team's failing test

`make pilot` measures the packaged pytest fixture, not adoption by a team. Use
a Git checkout with a reproducible failure and a local image that already has
the project's dependencies. For a checkout containing the example defect:

```sh
python3 scripts/pilot.py --workspace /path/to/team-checkout --image team-tests:local \
  --test 'python3 -B -m pytest -q -p no:cacheprovider' --expect-failure 'assert 21 == 14' \
  --fix-file calculator.py --fix-before 'value * 3' --fix-after 'value * 2' \
  --runs 3 --out artifacts/team-pilot.json
```

The pilot copies Git-selected regular files, including uncommitted edits, to a
private directory. It does not copy Git history, ignored untracked files,
symlinks, `.env`, `.venv` or `node_modules`. Keep the report outside the input
checkout. The replacement must match exactly once. Each run requires a normal
nonzero exit with the expected diagnostic before the repair, a zero exit after
it, exactly one uploaded file for the repair, then no changes on resync. The
report records time to the first failing result and to the verified fix.
