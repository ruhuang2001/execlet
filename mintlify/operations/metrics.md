---
title: "Metrics"
description: "What /metrics exposes, what it deliberately omits, and how to alert on it."
---

`GET /metrics` returns Prometheus text and uses the **same bearer
authentication** as `/v1`. Scrape it with the key, not anonymously.

## What it exposes

| Group | Signal |
| --- | --- |
| HTTP | route/status latency histograms, using bounded route templates |
| Creates | outcomes including resource refusals, and create latency |
| Executions | start latency, time to first output |
| Cleanup | failures and pending removals |
| State | live sandbox records and reserved resources |
| Prewarm | `execlet_prewarm_ready` instances available for handoff |

Counters reset on daemon restart. That is expected for a single-daemon design;
do not alert on a counter reset as if it were data loss.

## What it deliberately omits

No command text, image names, sandbox IDs, paths, environment variables, or
credentials appear as labels. Labels are bounded route templates so cardinality
cannot grow with usage.

<Note>
The corollary worth knowing: you cannot break down latency **by image** or **by
sandbox** from metrics alone. That omission is the privacy and cardinality
guarantee. Correlate with your own request IDs (`X-Request-ID` is on every
response) instead.
</Note>

## Alerts that correspond to real failure modes

| Alert on | Because |
| --- | --- |
| `429` rate on creates | Budgets are too small for the workload, or a caller is leaking sandboxes |
| Pending removals > 0 for minutes | Docker removal is failing; admission slots stay held |
| `502 backend_error` rate | The container runtime is unhealthy, not your code |
| `/readyz` down with `/healthz` up | Docker is unreachable. This is the condition worth paging on |
| Prewarm ready stuck at 0 | Provisioning is failing; cold starts will be slow |

## Verifying the numbers

```sh
python3 scripts/e2e-operations.py --server bin/execletd --image "$IMAGE" \
  --out artifacts/operations --samples 5
```

This records `report.json`, Docker/proxy/server logs, `cleanup.json`,
Prometheus samples, and `baseline.json`. Faults cover resource races, removal
failures, uncertain pid handshakes, crash and graceful restart, corrupt
journals, and storage outages.

For latency specifically:

```sh
python3 scripts/e2e-operations.py --server bin/execletd --image "$IMAGE" \
  --benchmark-only --samples 60 --out artifacts/latency-60
```

Each round creates a sandbox, runs `/bin/true` and `/bin/echo ok`, and deletes
it, discarding one warmup round. Retained rows record create-to-ready, command
time to terminal result, `echo` to first stdout callback, delete, and the whole
round.

<Warning>
These are **client-observed** times, not pure Docker exec time, and they do not
measure cold image pulls. A single warm run is not a benchmark. Compare
repeated runs on the same hardware, with the same image, on an idle host —
coverage-instrumented CI builds are a semantics check, not a latency threshold.
</Warning>

## Keeping evidence honest

Store `environment.json` (image ID, source HEAD, server checksum, Docker and
Python versions) next to `policy.json`, the raw samples, and `cleanup.json`. A
latency number without its environment is not reproducible, and an
irreproducible number is not evidence.
