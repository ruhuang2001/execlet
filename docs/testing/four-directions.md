# Four-direction acceptance

Build from the checkout with `make build` to embed the Linux amd64 and arm64 guest executables. The suites below start local daemons with distinct ownership labels and write their evidence under `artifacts/`, or under `$(ARTIFACTS)` when `make feature-e2e ARTIFACTS=path` or `make runtime-e2e ARTIFACTS=path` runs them, which keeps concurrent runs apart. The commands use Docker, Go, Python and Node.js. Run the runtime suite on a Linux Docker host with `runsc` installed.

| Feature | Reproduce from the repository root | Evidence |
| --- | --- | --- |
| File write metadata across SDKs and E2B adapters | `bash scripts/e2e-files.sh artifacts/files-integrated` | `files-integrated/write-metadata.json` records writes with 10001 siblings, the resulting type of a replaced link, and its original target contents |
| MCP with a development agent | `python3 scripts/e2e-amp.py` with a configured Amp CLI | `amp/acceptance.json` and `amp/mcp.jsonl` record the actual test results, host edit and cleanup. The recorded single-sandbox run used `--out artifacts/amp-final` |
| MCP wire contract and workspace sync | `python3 scripts/e2e-mcp.py` | `mcp-e2e/acceptance.json` records failing and passing tests from a temporary checkout, reconnection, output paging, TTL renewal and final cleanup |
| Guest file metadata and startup | `python3 scripts/e2e-guest.py` | `guest/report.json` compares stat, paginated lists and creation times across guest, legacy and stripped images |
| Managed execution and process limits | `python3 scripts/e2e-guest-faults.py` after the guest fixture build | `guest-faults/report.json` includes a reattached execution with one side effect, cancellation at the PID limit, and retirement after OOM |
| Interactive shell | `python3 scripts/e2e-terminal.py` | `terminal/acceptance.json` includes the retained directory and environment, resized window and child-process cleanup |
| HTTPS allowlist and credential broker | `python3 scripts/egress-e2e/run.py` | `egress/acceptance.json` records the authenticated response, refused destination, method, path and image, and direct connection failure after proxy shutdown |
| Named templates and filesystem snapshots | `python3 scripts/e2e-templates.py` | `templates/report.json` covers install, save, restart, independent clones and deletion. On the Linux Docker host, add `--runtime runsc --out artifacts/runsc-template` to exercise overlay whiteouts, opaque directories, hard links and restoration of the source pause state |
| Prewarmed capacity | `python3 scripts/e2e-prewarm.py` | `prewarm-e2e/acceptance.json` includes the raw latency samples, a reservation at capacity, concurrent claims and restart reconciliation |
| runc and gVisor | `python3 scripts/e2e-runtime.py` after building the minimal fixture | `runtime/report.json` records each runtime's actual limits, network interfaces, filesystem round trips and execution result |

The MCP example image and client launch configuration are documented in the [MCP example](../../examples/mcp/README.md). Deployment policy settings, snapshot exclusions and the per-runtime capability matrix are in [operations](../operations.md). Installed-package E2B comparisons use the [existing compatibility runner](../../scripts/e2b-compatibility/README.md).

## Coverage from a clean checkout

`make feature-e2e` builds both Linux guest agents before running the local
Docker scenarios. For Go coverage, set `GOFLAGS="-cover -covermode=atomic -coverpkg=./..."`
and point `GOCOVERDIR` at an existing absolute output directory. Child daemon,
CLI and fixture builds inherit the instrumentation. The Linux runtime job also
checks the kernel limits and captures counters for runc and runsc.
The coverage job merges those counters with race tests, the main E2E suite and
the operations suite, then runs the existing 70% block gate.

## Performance measurements

The local four-way benchmark measured the time from a create request to completion of the first echo command. Each group used ten samples. Its cold image had a unique layer in a local registry. Times include the client and daemon.

| Starting state | P50 (ms) | P95 (ms) |
| --- | ---: | ---: |
| Cold image | 395.7 | 436.6 |
| Image already local | 288.1 | 314.0 |
| Named template | 275.6 | 289.2 |
| Prewarmed container | 146.2 | 163.7 |

A separate creation-only measurement used the same local fixture image and administrator policy with 30 samples per mode. Legacy P50/P95 were 87.8/106.3 ms. Guest P50/P95 were 151.1/190.8 ms. The operations suite writes the individual observations in `baseline.json` under `fresh_container_local_image`.

## Revision audit

The following checks exercise the requirements from the revised proposal. Paths in the evidence column are relative to the local `artifacts/` directory.

| Milestone and requirement | Implementation and acceptance | Evidence |
| --- | --- | --- |
| 0. Post-write metadata | HTTP PUT returns 200 and FileEntry. Go also retains its error-only method. Python and TypeScript E2B adapters construct WriteInfo from that single response. Both languages write successfully beside 10001 siblings. | `goal-audit-files/write-metadata.json` |
| 1. MCP authority and lifecycle | The CLI owns the official Go SDK stdio server. Its eight tools expose structured results and model-visible text. Verified a default offline sandbox, explicit bridge, policy refusal, all eight offline errors, TTL beyond the original expiry, and full-list removal after destruction. | `mcp-e2e/acceptance.json` |
| 1. Workspace sync | Verified exclusions, checksum-based incremental upload, a batch deletion crossing 100 paths, reset after manifest corruption, interrupted retries, and archive-budget rejection before mutation. Synced the SDK checkout and two pinned upstream repositories and checked files inside the guest. | `mcp-e2e/acceptance.json` |
| 1. Development-agent repair loop | Amp used the eight-tool server, observed a real failing test, edited the host checkout, resynced one file, ran the passing test and destroyed the offline sandbox. | `amp-goal-audit/acceptance.json`, `amp-goal-audit/visible-tests.json` |
| 1. Output, polling and durable cleanup | A long command returns an execution ID. MCP requests newest-output retention, returns the actual diagnostic after crossing 1 MiB, and pages UTF-8 within 64 KiB. The HTTP/restart suite verifies the default prefix, the requested suffix, managed output, monotonic SSE IDs, idempotency and permanent removal. | `mcp-e2e/acceptance.json`, `output-retention/acceptance.json` |
| 2. Delivery and handshake | Real Linux AMD64 and ARM64 images received matching embedded ELF files. Checked root ownership, file/socket modes, fallback on missing architecture or incompatible protocol, relay reconnection and bounded frames. | `guest-boundaries/report.json`, `guest-transport/report.json` |
| 2. Metadata, pagination and cost | Stripped-image stat and sorted directory pages match legacy. Pages spanning 1 MiB travel in bounded frames. A scan over 10000 entries fails explicitly. Identical policy and image settings were used for 30 creation measurements in each mode. | `guest/report.json`, `guest-boundaries/report.json`, `guest-audit-legacy-30/baseline.json`, `guest-audit-guest-30/baseline.json` |
| 3. Managed execution and isolation | Verified no duplicate effect after losing a start acknowledgement, PID-limit cancellation, protected non-root and unprotected root image behavior, real OOM and conservative restart handling. All ten sampled operations on an externally paused container returned 409. | `guest-transport/report.json`, `guest-audit-faults/report.json`, `guest-boundaries/report.json` |
| 3. Interactive terminal | The CLI opens and reattaches to the same PTY, preserves cwd/environment, resizes the window and releases the child process on close. | `terminal/acceptance.json` |
| 4. Egress and credentials | Installed a wheel by digest over the approved HTTPS proxy in a Docker none network. A test upstream confirmed bearer authentication. Other destinations, paths, methods and unauthorized images were refused. Direct egress failed after proxy shutdown. | `egress/acceptance.json` |
| 4. Templates and prewarming | Verified template creation on runc and runsc, filesystem and dependency isolation across clones, named-template rollover and last-alias removal. Saving a runsc source preserves deletions, opaque directory replacements and hardlinks, and restores its original paused or running state. Standby reservations count against quota and reconcile on restart. Four start-time scenarios retain their raw measurements. | `templates-audit/report.json`, `runsc-template/report.json`, `prewarm-audit/acceptance.json` |
| 5. Optional isolation runtime | Compared explicit runc and runsc and exercised Docker-selected runsc across a restart. Checked actual memory, swap, CPU, PID and network constraints and read back guest filesystem changes. | `runtime-audit/explicit.json`, `runtime-audit/implicit.json` |

The final installed-package comparison passed all 80 Python/TypeScript scenarios and 403 reference expectations, with cleanup confirmed for every scenario. It includes empty directory listings and captured pre-attachment output. The comparison is in `goal-audit-e2b/compare.log`. The local SHA-256 evidence index is `artifacts/four-directions.json`.

The write result describes the directory entry observed **after** extraction. Docker replaces an existing symlink at the destination with a regular file and preserves the original target. A post-write observation that is still a symlink returns its symlink metadata. The launch example supplies the prebuilt test image as the first allowed image. A successful version handshake on the root-owned socket confirms guest readiness. Firecracker/vsock remains conditional on a deployment requiring a virtual-machine isolation boundary.
