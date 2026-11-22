# Four-direction acceptance

Build from the checkout with `make build` to embed the Linux amd64 and arm64 guest executables. The suites below start local daemons with distinct ownership labels and write their evidence under `artifacts/`. The commands use Docker, Go, Python and Node.js. Run the runtime suite on a Linux Docker host with `runsc` installed.

| Feature | Reproduce from the repository root | Evidence |
| --- | --- | --- |
| File write metadata across SDKs and E2B adapters | `bash scripts/e2e-files.sh artifacts/files-integrated` | `files-integrated/write-metadata.json` records writes with 10001 siblings, the resulting type of a replaced link, and its original target contents |
| MCP with a development agent | `python3 scripts/e2e-amp.py` with a configured Amp CLI | `amp/acceptance.json` and `amp/mcp.jsonl` record the actual test results, host edit and cleanup. The recorded single-sandbox run used `--out artifacts/amp-final` |
| MCP wire contract and workspace sync | `python3 scripts/e2e-mcp.py` | `mcp-e2e/acceptance.json` records failing and passing tests from a temporary checkout, reconnection, output paging, TTL renewal and final cleanup |
| Guest file metadata and startup | `python3 scripts/e2e-guest.py` | `guest/report.json` compares stat, paginated lists and creation times across guest, legacy and stripped images |
| Managed execution and process limits | `python3 scripts/e2e-guest-faults.py` after the guest fixture build | `guest-faults/report.json` includes a reattached execution with one side effect, cancellation at the PID limit, and retirement after OOM |
| Interactive shell | `python3 scripts/e2e-terminal.py` | `terminal/acceptance.json` includes the retained directory and environment, resized window and child-process cleanup |
| HTTPS allowlist and credential broker | `python3 scripts/egress-e2e/run.py` | `egress/acceptance.json` records the authenticated response, refused destination, method, path and image, and direct connection failure after proxy shutdown |
| Named templates and filesystem snapshots | `python3 scripts/e2e-templates.py` | `templates/report.json` covers install, save, restart, independent clones and deletion |
| Prewarmed capacity | `python3 scripts/e2e-prewarm.py` | `prewarm-e2e/acceptance.json` includes the raw latency samples, a reservation at capacity, concurrent claims and restart reconciliation |
| runc and gVisor | `python3 scripts/e2e-runtime.py` after building the minimal fixture | `runtime/report.json` records each runtime's actual limits, network interfaces, filesystem round trips and execution result |

The MCP example image and client launch configuration are documented in the [MCP example](../../examples/mcp/README.md). Deployment policy settings, snapshot exclusions and the per-runtime capability matrix are in [operations](../operations.md). Installed-package E2B comparisons use the [existing compatibility runner](../../scripts/e2b-compatibility/README.md).

## Performance measurements

The local four-way benchmark measured the time from a create request to completion of the first echo command. Each group used ten samples. Its cold image had a unique layer in a local registry. Times include the client and daemon.

| Starting state | P50 (ms) | P95 (ms) |
| --- | ---: | ---: |
| Cold image | 395.7 | 436.6 |
| Image already local | 288.1 | 314.0 |
| Named template | 275.6 | 289.2 |
| Prewarmed container | 146.2 | 163.7 |

A separate creation-only measurement used the same local fixture image and administrator policy with 30 samples per mode. Legacy P50/P95 were 87.8/106.3 ms. Guest P50/P95 were 151.1/190.8 ms. The operations suite writes the individual observations in `baseline.json` under `fresh_container_local_image`.
