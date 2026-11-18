# Deployment policy, recovery and metrics

These features describe the current source, not the published v0.0.2 binaries.
They do not turn Docker into a hardened multi-tenant isolation boundary.
Use a dedicated runtime host, protect its Docker socket, and keep the listener
private or behind an authenticated TLS endpoint. An API key authorizes the
whole daemon, not one tenant.

## Administrator policy

`sandboxd -policy /absolute/path/policy.json` enables strict aggregate admission.
All fields are required; unknown fields, nonpositive budgets and unsupported
network modes prevent startup. For example, replace the image placeholder
with an installed, approved image digest:

```json
{
  "max_sandboxes": 4,
  "total_memory_mb": 2048,
  "total_nano_cpus": 4000000000,
  "total_pids": 4096,
  "allowed_images": ["sha256:REPLACE_WITH_APPROVED_IMAGE_ID"],
  "allowed_networks": ["none"]
}
```

Reservations are the sum of configured limits, not measured usage. A paused
sandbox still reserves capacity. Concurrent creates reserve atomically before
Docker mutation; an exhausted budget returns `429 rate_limited`. A forbidden
image/network or explicit unlimited CPU returns `400 invalid_request`. Omitted
network selects the first allowed mode; omitted CPU reserves the smaller of
one CPU and the total CPU budget. Memory/PID defaults remain 512 MiB/1024, so
set smaller values explicitly when the total budget requires them. Native
live-sandbox limits also apply.

The allowlist compares exact image strings. Prefer immutable digests: allowing
a mutable tag does not pin its content. Before admitting a container, the
backend checks Docker HostConfig and reads actual cgroup-v2 memory, swap, CPU
and PID limits inside the guest. Unsupported controllers, unreadable probes
or mismatched settings fail closed and trigger removal. Images must supply
`sh`, `cat`, `ls` and readable cgroup-v2 files for these probes.

A failed create or delete does not free its reservation until removal is
confirmed. A record may already say `killed` while its container still exists;
the reaper retries removal and `/metrics` exposes the backlog. Changing the
policy requires a restart and recovery rechecks adopted containers.

### Network guarantees

- `none` requires only the guest loopback interface. No external interface
  means no outbound internet, host/private-network or metadata route.
- `bridge` is deliberately unrestricted; it can reach private/host services
  according to Docker routing. Including it in the allowlist opts out of the
  no-egress guarantee for those sandboxes.
- No destination/domain allowlist, filtered internet, per-tenant ACL or
  custom bridge mode is implemented. Unsupported names are rejected.

The acceptance suite includes a reachable private-peer control under bridge
and denied private/metadata attempts under none, plus interface evidence. It
does not exercise every host-service configuration or prove resistance to a
Docker/kernel escape.

## Conservative restart recovery

Use a stable, unique `-owner` and an absolute `-state-dir` on private local
storage. Do not share an owner between daemons, even with different state
directories. The directory must have private permissions (0700); records and
lock are private regular files (0600). A kernel file lock prevents a second
writer; wrong owner, unsupported format or corrupt/missing linked records
reject startup before Docker reconciliation.

The journal uses atomic replacement with file and directory fsync. It stores
sandbox metadata, start intents, bounded terminal events, idempotency keys and
the invocation fingerprint key. It is not encrypted: sandbox environment,
argv and retained output can contain secrets. Its ancestors and host account
must be trusted; do not mount it into a guest. Network filesystems, shared
writers, tamper resistance and storage-version migration are unsupported.

On restart, before accepting traffic:

1. Validate the complete record graph and terminal outcome/event agreement.
2. Reconcile exact owned container IDs. Eligible idle containers retain files
   and completed execution records. Policy mode revalidates limits and budget.
3. Mark interrupted executions `completed` with reason `unknown`, null exit
   code and `output_truncated=true`; remove their whole sandbox, including
   siblings. Never re-execute or infer success from a surviving process.
4. Remove owned orphans and unsafe containers. Failure to establish a safe
   state refuses admission rather than silently ignoring resources.

Completed IDs, outcomes, retained output and idempotency keys survive subject
to normal retention. Live output is not journaled. A nonzero SSE cursor into
lost live output returns `409 invalid_state`; reading the snapshot or starting
from zero exposes the unknown/truncated result, not a complete transcript.
Policy mode removes paused containers because it cannot run its enforcement
probe without resuming them. Without policy, an otherwise eligible paused
container can be preserved.

Graceful shutdown retains idle containers but terminates active work within
the cleanup budget. A crash before terminal persistence leaves uncertainty,
even if the command actually finished. These are **control-record recovery
and idle-container adoption**, not live-process takeover, memory checkpoints,
durable full logs, or E2B persistence parity. Fresh-client background attachment
is supported while the owning daemon remains alive.

A journal write failure closes `/readyz` and new sandbox/execution admission
and never acknowledges an unpersisted successful completion. Repair storage,
then restart with the same owner/directory; do not clear the journal to force
readiness. Inspect unknown outcomes before deciding whether business work can
be retried. Keep consistent backups while the daemon is stopped; rolling back
binaries is not a state-format rollback.

The reaper checkpoints sandbox activity rather than fsyncing every read, so
post-crash idle expiry can use the last checkpoint. Checkpoint fsync cost and
the existing bounded history limit throughput; this is not an HA database.
Without `-state-dir`, history remains in-memory and startup removes owned
containers; incomplete removal now prevents startup.

## Metrics and repeatable evidence

`GET /metrics` uses the same bearer authentication as `/v1`. Prometheus text
includes HTTP route/status latency, create outcomes (including resource
refusals), execution-start and first-output latency, cleanup failures/pending
removals, live records and reserved resources. Labels are bounded templates,
not IDs, images, command text, environment or credentials. Counters reset on
restart. HTTP cancellation/kill routes are observable; these are not a separate
per-process signal-confirmation trace.

Run the reproducible Docker/HTTP policy, failure and recovery suite with a
compatible image already installed, using a fresh short output path:

```sh
go build -o bin/sandboxd ./cmd/sandboxd
IMAGE=$(docker image inspect tiny-sandbox-e2b:local --format '{{.Id}}')
python3 scripts/e2e-operations.py --server bin/sandboxd --image "$IMAGE" \
  --out artifacts/operations --samples 5
```

Evidence includes `report.json`, Docker/proxy/server logs, `cleanup.json`,
Prometheus samples and `baseline.json`. Faults cover resource races, removal
failures, uncertain PID handshake, crash/graceful restart, corrupt journals
and storage outages. Startup faults first observe real user code, then drop or
stall its Docker response, with durable state and managed commands on and off.
They verify single execution, bounded return, unknown/truncated results, sibling
loss and visible cleanup failure; a normal stream exceeds the handshake budget
without losing output. See the [reliability failure model](testing/reliability-failures.md).
Cleanup is scoped to the run's owner, not all Docker.

The baseline reports sample counts, medians, empirical p95 and raw samples for fresh containers
using an already-local image, commands on reused containers and time to first
stdout. It does not measure image pulls, cross-host capacity, sustained load,
p99 SLOs or a speedup over the previous release. Compare repeated runs on the
same hardware/image before making performance claims.
