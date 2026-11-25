---
title: "Admission policy"
description: "Aggregate budgets, image and network allowlists, and fail-closed verification."
---

Use a policy to limit which images, networks and runtimes callers can choose,
and how much capacity they can reserve. Without one, Execlet still enforces
per-sandbox limits and a 64-slot admission cap, but not aggregate CPU, memory
or task budgets.

## Start with an offline policy

From the [built checkout](/quickstart#build-the-binaries), with the Docker Unix
socket [configured](/quickstart#start-the-daemon), pull a test image and use
its local image ID as the only approved image:

```sh
docker pull alpine:3.20
IMAGE=$(docker image inspect alpine:3.20 --format '{{.Id}}')
POLICY_DIR=$(mktemp -d)
cat > "$POLICY_DIR/policy.json" <<EOF
{
  "max_sandboxes": 4,
  "total_memory_mb": 2048,
  "total_nano_cpus": 4000000000,
  "total_pids": 4096,
  "allowed_images": ["$IMAGE"],
  "allowed_networks": ["none"]
}
EOF
./bin/execletd -addr 127.0.0.1:8081 -owner "policy-demo-${POLICY_DIR##*/}" \
  -policy "$POLICY_DIR/policy.json"
```

In a second terminal in the checkout, pass the same image ID, not its tag:

```sh
IMAGE=$(docker image inspect alpine:3.20 --format '{{.Id}}')
SANDBOX_DOMAIN=http://127.0.0.1:8081 ./bin/execletctl run --image "$IMAGE" --rm -- echo policy-ok
```

The command prints `policy-ok`. A strict policy rejects unknown fields,
nonpositive aggregate budgets, or unsupported modes at startup. Stop the demo
daemon with Ctrl+C and remove the temporary policy directory when done.

## Reservations are configured limits, not measured use

An admitted sandbox reserves its **configured** memory, CPU, and pid values, not
what it is currently using. A paused sandbox still reserves capacity. This is
the conservative choice: measured use would let four idle-looking sandboxes all
spike at once and OOM the host.

Consequences you will notice:

- Omitted network selects the **first** allowed mode, not a separate default.
- Omitted CPU reserves the smaller of one CPU and the total CPU budget.
- Memory and pid limits still default to 512 MiB / 1024, so a small total budget
  requires explicitly smaller per-sandbox values.

| Situation | Response |
| --- | --- |
| Budget exhausted | `429 rate_limited` |
| Forbidden image, network or runtime, or explicit unlimited CPU | `400 invalid_request` |
| Runtime could not confirm the limits | `502 backend_error` |

## Reservation happens before Docker mutates anything

Concurrent creates reserve atomically before the backend call. Two simultaneous
requests cannot both see the last slot. If a create fails after leaving a
container behind, or a delete fails, that slot stays held until the runtime
**confirms** removal. `/metrics` exposes the backlog; do not treat it as a leak
in the accounting.

## Docker settings are re-verified, not trusted

Before admitting a container, Execlet checks Docker's `HostConfig` and
probes cgroup v2 memory, swap, CPU, and PID limits plus guest interfaces.
For `runc` the guest needs `sh`, `cat`, `ls`, and readable cgroup v2 files.
For [`runsc`](/operations/isolation#running-under-gvisor), Execlet reads the
Linux host cgroup instead and runs `cat /proc/net/dev` in the guest.

An unreadable or mismatched probe rejects the create and triggers cleanup.
Verify the policy on the Docker host and base image you intend to use.

## Images

`allowed_images` compares **exact strings**. Prefer immutable digests or local
image IDs; a mutable tag can point to new content and remain allowed. A digest
registered as a [template](/operations/templates) is also approved.

`allowed_runtimes` additionally restricts which Docker runtime a caller may
request. The first entry is the policy default. If omitted, only Docker's
configured default is used and per-sandbox overrides are rejected.

## Networks

| Mode | Network configuration |
| --- | --- |
| `none` | Docker `none`; guest loopback only |
| `filtered` | Docker `none` plus a loopback proxy for CONNECT to approved domains on port 443 |
| `bridge` | Follows Docker routing; it may reach private and host services |

Network mode is fixed in Execlet at create time. These settings govern guest
traffic, not the Docker daemon's image pulls.

### Filtered egress

Allow `filtered` in `allowed_networks` and set `egress.domains` to exact
domain names (no wildcards). The embedded guest agent listens on
`127.0.0.1:8888`, and sandbox commands receive `HTTPS_PROXY`. The daemon relays
traffic over Docker exec, resolves the requested hostname on the host, rejects
private and reserved addresses, and dials an approved address directly. Each
new tunnel is checked; TLS payloads and HTTP paths inside it are not inspected.

Up to **64 tunnels** per sandbox; `egress.tunnel_idle_seconds` (default 120)
bounds inactivity, but a transfer with continuing traffic can outlive it.

<Warning>
Fake-IP DNS resolution returns addresses in `198.18.0.0/15`, which the policy
rejects. Configure real-address DNS for approved domains on the daemon host.
</Warning>

### Credential brokering

`egress.credential` binds one GET request to a fixed HTTPS URL and a list of
authorized images. The workload calls `GET http://127.0.0.1:8888/credential`;
the host identifies the sandbox from its own exec channel and sends a freshly
constructed request.

The daemon takes the bearer token from `token_env` and sends it upstream, not
to the guest. It does not follow redirects. Only a successful bounded
response body is returned to the workload; treat that body as data available
to any code running in an authorized sandbox.

## Editing a policy

Write the new document to a temporary file in the **same directory**, then rename
it over the original, so a reader never sees a half-written policy.

Template lookups read the current file, so template additions and deletions
take effect **while the daemon runs**. Other settings, including budgets,
egress, and the runtime allowlist, require a restart.

## Verifying a policy

Use the [operations acceptance suite](/operations/metrics#verifying-the-numbers)
to exercise resource races, failed removals, journal corruption, and storage
outages. Its fixture needs more tooling than the minimal policy example above.
