---
title: "Metrics"
description: "What /metrics exposes, what it deliberately omits, and how to alert on it."
---

`GET /metrics` returns Prometheus text and uses the **same bearer
authentication** as `/v1`. When the daemon has `SANDBOX_API_KEY` configured,
use the same key in the scraper's `Authorization: Bearer` header.

## What it exposes

| Group | Signal |
| --- | --- |
| HTTP | route/status latency histograms, using bounded route templates |
| Creates | outcomes including resource refusals, and create latency |
| Executions | start latency, time to first output |
| Cleanup | failures and pending removals |
| State | live sandbox records; reserved resources in policy mode |
| Prewarm | `execlet_prewarm_ready` instances available for handoff in policy mode |

Counters reset on daemon restart. That is expected for a single-daemon design;
do not alert on a counter reset as if it were data loss.

When upgrading from tiny-sandbox, change `tiny_sandbox_*` selectors to
`execlet_*` in scrape filters, dashboards, recording rules and alerts. The
renamed daemon does not emit the old series. Verify a new sample and check
the alert queries after cutover: an empty result is not evidence of zero
failures. Historical samples remain under their original names.

## What it deliberately omits

Labels use bounded values such as route templates and outcomes, not command
text, image names, sandbox IDs, raw paths, environment variables or credentials.
To investigate a specific request, use its `X-Request-ID` to find the daemon
log entry. To track OOM outcomes, use execution results or logs; there is no
dedicated OOM metric.

## Alerts that correspond to real failure modes

| Alert on | Because |
| --- | --- |
| `429` rate on creates | Budgets are too small for the workload, or a caller is leaking sandboxes |
| Pending removals > 0 for minutes | Docker removal is failing; admission slots stay held |
| `502 backend_error` rate | Check runtime, policy verification, and image availability in daemon logs |
| `/readyz` down with `/healthz` up | Check Docker connectivity and the journal; the daemon is still running |
| Prewarm ready stuck at 0 with a configured target | Look for provisioning failures, exhausted budgets, or demand using the pool faster than it can refill |

## Verifying the numbers

From the [built checkout](/quickstart#build-the-binaries), use Python 3.11+,
a Docker Unix socket, and the local test image from `examples/e2b`. The full
suite requires a non-root test image with Python and readable guest cgroup v2
files. Use a fresh output directory and keep its path short for Unix sockets.

```sh
docker build -t execlet-e2b:local examples/e2b
IMAGE=$(docker image inspect execlet-e2b:local --format '{{.Id}}')
python3 scripts/e2e-operations.py --server bin/execletd --image "$IMAGE" \
  --out artifacts/operations --samples 5
```

This records `report.json`, Docker/proxy/server logs, `cleanup.json`,
Prometheus samples, and `baseline.json`. Faults cover resource races, removal
failures, uncertain pid handshakes, crash and graceful restart, corrupt
journals, and storage outages.

For the short-command latency workload alone, use the same `IMAGE`:

```sh
python3 scripts/e2e-operations.py --server bin/execletd --image "$IMAGE" \
  --benchmark-only --samples 60 --out artifacts/latency-60
```

Each round creates a sandbox, runs `/bin/true` and `/bin/echo ok`, and deletes
it, discarding one warmup round. Retained rows record create-to-ready, command
time to terminal result, `echo` to first stdout callback, delete, and the whole
round in `short-latency.json`.

<Warning>
These are **client-observed** times, not pure Docker exec time, and they do not
measure cold image pulls. A single warm run is not a benchmark. Compare
repeated runs on the same hardware, with the same image, on an idle host —
coverage-instrumented CI builds are a semantics check, not a latency threshold.
</Warning>

### Single-host soak

The latency workload is serial and short. To exercise concurrent clients on one
host, build the binaries and the MCP example image, then run the soak:

```sh
make build mcp-image
make soak
```

`make soak` runs four clients for 60 seconds of steady load against a private,
journaled daemon. Each round creates an offline sandbox, streams a unique output
marker, verifies the terminal result, and deletes the sandbox. Every worker must
finish rounds, and a connection error fails the run. `artifacts/soak.json`
reports confirmed rounds per minute and client-observed p50, p95 and max for
create, first output, command completion, delete and the whole round. Use the
same host, binary, image and resource limits when you compare runs.

The soak also checks recovery with two probes:

- A new client sees a command still running after its observer disconnects,
  then explicitly cancels it.
- Halfway through the load test, workers drain. The harness kills the daemon
  with one known command running and restarts it with the same journal and
  owner. The interrupted result must be `unknown`, with no exit code and
  truncated output. Its sandbox must be `killed` and its container gone.

The report separates restart-to-ready time from steady-state throughput. It
does **not** measure requests that arrive during an outage. Each load phase must
finish with zero owned containers, live sandboxes, pending cleanups and cleanup
failures.

For a longer run:

```sh
python3 scripts/soak.py --image execlet-mcp:local --duration 1800 --concurrency 4 --restart --progress
```

Omit `--restart` to measure uninterrupted load. Add `--journal` to keep
journaling enabled without a crash. The CI run is a correctness check, not a
capacity SLA.

## Keeping evidence honest

Store `environment.json` (image ID, source HEAD, server checksum, Docker and
Python versions) next to `policy.json`, the raw samples, and `cleanup.json`. A
latency number without its environment is not reproducible, and an
irreproducible number is not evidence.
