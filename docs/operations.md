# Deployment policy, recovery and metrics

These features describe the current source, not the published v0.0.2 binaries.
They do not turn Docker into a hardened multi-tenant isolation boundary.
Use a dedicated runtime host, protect its Docker socket, and keep the listener
private or behind an authenticated TLS endpoint. An API key authorizes the
whole daemon, not one tenant.

## Administrator policy

`sandboxd -policy /absolute/path/policy.json` enables strict aggregate admission.
The aggregate budgets and image and network allowlists are required. Unknown fields, nonpositive budgets and unsupported
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
sandbox admission limits also apply.

The allowlist compares exact image strings. Prefer immutable digests: allowing
a mutable tag does not pin its content. Before admitting a container, the
backend checks Docker HostConfig and reads actual cgroup-v2 memory, swap, CPU
and PID limits inside the guest. Unsupported controllers, unreadable probes
or mismatched settings fail closed and trigger removal. Images must supply
`sh`, `cat`, `ls` and readable cgroup-v2 files for these probes.

The default 64-slot admission limit includes every creation in progress, live
sandbox and container awaiting confirmed cleanup. This limit applies with or
without a deployment policy. A failed create that left a container, or a failed
delete, holds its slot until the runtime confirms removal. A record may already say `killed` while its container still exists;
the reaper retries removal and `/metrics` exposes the backlog. Changing the
budgets or runtime policy requires a restart. Recovery rechecks adopted containers.

### Named images and filesystem templates

The optional `templates` object maps names to local Docker image IDs
(`sha256:<64 lowercase hex digits>`) or digest-pinned repository references.
Each registered digest is an approved image. Requests can use the digest or
`{"image":"template:python-test"}`. The sandbox records the resolved digest.
Each creation receives its own writable filesystem. New admissions read the
template mapping from the policy file, so additions and deletions take effect
while the server runs. A missing or invalid catalog prevents alias resolution.
Use an atomic rename when editing the policy.

An administrator with local Docker access can save a configured sandbox and
publish its digest to the same policy. Run the tool against the Docker context
used by sandboxd and pass the daemon's ownership label.

```sh
python3 scripts/template-admin.py save --policy /etc/tiny-sandbox/policy.json \
  --owner sandboxd-prod --sandbox "$SANDBOX_ID" --name python-test

sandboxctl run --image template:python-test --rm -- python3 -m pytest

python3 scripts/template-admin.py delete --policy /etc/tiny-sandbox/policy.json \
  --name python-test
```

Save requires a new name. For runc, Docker briefly freezes the source during its commit. The administrator tool exports a stopped copy, filters its tar, and imports a local image.

For runsc, execute the same command on the Linux Docker host with the installed
`runsc` binary. Docker pauses writers while the tool captures the lower filesystem
and gVisor's upper layer using `runsc tar rootfs-upper`. The merge applies file
deletions, directory replacements, permissions and hard links, excludes the
runtime's backing file and imports the flattened filesystem. A running source
resumes after capture. A paused source stays paused. The default runtime state
directory is `/var/run/docker/runtime-runc/moby`. Supply `--runsc-root DIR` for
another installation path. An unsupported overlay format or failed capture
aborts publication.
[Upstream runsc tar command](https://github.com/google/gvisor/blob/release-20261116.0/runsc/cmd/tar.go)
 Finish dependency installation before saving to
capture an application-consistent project. Contents under `/tmp`, `/var/tmp`,
`/run`, `/dev`, `/proc`, `/sys`, and each mounted volume are excluded.
The tool also omits `/.tiny-sandbox`, `/.dockerenv` and Docker-managed
`/etc/hosts`, `/etc/hostname` and `/etc/resolv.conf`. Other files, including
project-local settings and credentials written to disk, are part of the
snapshot. A retained hard link to an excluded path fails publication.
The template preserves the base image's architecture, user, working
directory, environment, entrypoint, command and volume declarations. Processes
and execution records start fresh. The tool needs temporary host disk space
for the flattened filesystem and uses a lock and atomic rename to publish the
catalog after the image import succeeds.

Delete revokes the name for future creates. If no other name or explicit
image allowlist entry references the digest, it also removes this tool's
Docker tag. Docker retains layers needed by existing containers. Registry
images and independently managed tags stay under administrator control.

Run `python3 scripts/e2e-templates.py --server bin/sandboxd` to exercise
the save, restart, isolation and delete flow and write the acceptance report
to `artifacts/templates/report.json`.

### Network guarantees

- `none` requires only the guest loopback interface. No external interface
  means no outbound internet, host/private-network or metadata route.
- `bridge` is deliberately unrestricted; it can reach private/host services
  according to Docker routing. Including it in the allowlist opts out of the
  no-egress guarantee for those sandboxes.
- `filtered` uses a Docker `none` network and a guest loopback HTTPS proxy at
  `127.0.0.1:8888`. The host opens each upstream connection through Docker exec,
  resolves the requested hostname, checks the returned addresses and dials the
  selected address directly. The proxy accepts CONNECT on port 443 for exact
  configured domain names. Each new tunnel undergoes the same checks, including
  new connections after redirects. TLS payloads are opaque to the proxy.
  HTTPS clients use `HTTPS_PROXY` or `https_proxy`. Tools ignoring these
  variables only have the guest loopback interface. Filtered sandboxes require
  a running guest agent and are retired during daemon restart.

The relay accepts up to 64 active tunnels per sandbox and starts a relay for each incoming connection. Set `egress.tunnel_idle_seconds` to control the inactivity timeout (default 120 seconds). A transfer with continuing traffic can exceed that duration. Inspect sandboxd's `egress request denied` logs for the requested host and reason. DNS proxies using fake-IP mode can return addresses in `198.18.0.0/15`, which the policy rejects. Configure real-address DNS resolution for approved domains on the sandboxd host.

For a deployment that uses the package index, add these policy fields to the
resource budgets and image allowlist above.

```json
{
  "allowed_networks": ["none", "filtered"],
  "egress": {"domains": ["pypi.org", "files.pythonhosted.org"]}
}
```

The optional `egress.credential` object binds one GET request to a fixed
HTTPS URL and a list of authorized image identities. Set `url`, `images`
and `token_env` in that object. Supply the bearer token in the named
environment variable of sandboxd. A workload in an authorized filtered
sandbox can call `GET http://127.0.0.1:8888/credential`. The host identifies
the sandbox from its Docker exec channel, sends a newly constructed request,
and returns up to 1 MiB from a successful response. The broker exposes
no guest-selectable upstream path, headers, redirect handling or method.

Run `python3 scripts/egress-e2e/run.py` after `make agent` to verify an
authenticated request with temporary credentials, allowlist enforcement
and connectivity after proxy shutdown. The suite uses an internal Docker
network and writes its report to `artifacts/egress/acceptance.json`.

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
make build
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

`baseline.json` includes a Python command with an intentional 50 ms sleep and
uses a Docker fault proxy. For short-command measurements use the separate
`short-latency.json` workload, which runs in normal CI or on its own:

```sh
python3 scripts/e2e-operations.py --server bin/sandboxd --image "$IMAGE" \
  --benchmark-only --samples 60 --out artifacts/latency-60
```

The Python SDK connects directly to a fresh local daemon with policy enabled,
no journal and the real Docker socket. Each serial round creates a sandbox,
runs `/bin/true` and `/bin/echo ok` (alternating order), then deletes it. It
discards one warmup round. Each retained row records milliseconds for create
to ready, each command to terminal result, `echo` to first stdout callback,
delete, and the whole round through delete. Per-column summaries use the median
and nearest-rank P95; aggregate the paired whole-round samples rather than
adding percentiles. These are client-observed times, not pure Docker exec time.

Keep `environment.json` (image ID, source HEAD, server checksum, Docker and
Python versions), `policy.json`, the raw rounds and `cleanup.json` together.
Use the same build flags and an idle host for comparisons: CI uses coverage
instrumentation and its small sample is a semantics/cleanup check, not a
latency threshold. Neither workload measures cold pulls or pool hits.

## Prewarmed templates

A deployment policy can reserve fresh running containers for repeated requests. Set `templates` to an immutable image reference and add a `prewarm` target to the same policy. For example, the following fields belong alongside the existing aggregate budgets and allowlists.

```json
{
  "templates": {
    "python-test": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  },
  "prewarm": [
    {
      "image": "template:python-test",
      "count": 1,
      "memory_mb": 256,
      "pids_limit": 128,
      "nano_cpus": 500000000,
      "network": "none",
      "runtime": "runc"
    }
  ]
}
```

Replace the example digest with the installed image ID or repository digest. Include `runc` in `allowed_runtimes` when setting the runtime explicitly. Omitting resource, network or runtime fields applies the same defaults as sandbox creation. Restart the daemon to load a new pool configuration. With no `prewarm` entries the daemon creates containers on demand.

The daemon counts each standby container against every deployment budget from the beginning of provisioning. It validates resource limits before making an instance available. An incoming request claims a matching image digest, memory size, CPU limit, PID limit, network and runtime under one lock. A miss takes the usual creation path subject to available quota. The pool assigns the sandbox ID, Docker name and ownership labels at provisioning. Admission claims that identity atomically. The sandbox lifetime starts at handoff. Each instance goes to one user and is discarded on deletion. The background worker replenishes the configured target while capacity is available. Failed provisioning records a warning and backs off per profile from 2 to 60 seconds. The `tiny_sandbox_prewarm_ready` metric counts instances available for handoff.

On a journaled restart the daemon reconciles each delivered sandbox against its durable record. It removes unclaimed owner-labelled containers, then starts filling the pool. Graceful shutdown also removes idle pool containers. Changing a named template switches new requests to its current digest. On its next reconciliation, the pool removes excess standby containers for the old digest and fills the new target. Deleting an alias leaves its pool target dormant, including across restarts. Registering that name again reactivates it.

Run `python3 scripts/e2e-prewarm.py --samples 10` with Docker available to measure the interval from a create request to its first completed command. A private loopback registry serves a unique single-layer image for cold pulls. The script compares cold pulls, a local image, template resolution and confirmed pool hits on one engine. The output includes all samples, P50, P95, saturation and recovery results in `artifacts/prewarm-e2e/acceptance.json`.

### Runtime verification

Clients specifying a runtime first require the `sandbox.runtime.v1` capability so older servers that ignore unknown fields cannot silently select their default.

Set `allowed_runtimes` in the deployment policy to the Docker runtime names the administrator permits, for example `["runc", "runsc"]`. The first entry supplies the default. Set `runtime` on the create request to choose one of the allowed runtimes.

Docker inspection selects the filesystem transfer path at creation and journal recovery, including when the Docker Engine chooses its default runtime.

Runsc exposes a virtualized cgroup hierarchy to the guest. Policy verification therefore requires sandboxd to run on the Linux Docker host with access to its process table and cgroup tree. Admission fails if the matching scope or a limit cannot be checked. Runsc's private overlay and `/tmp` can diverge from Docker's archive view of the container. The backend uses a guest transfer helper for live filesystem operations.
