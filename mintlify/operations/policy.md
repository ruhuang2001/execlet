---
title: "Admission policy"
description: "Aggregate budgets, image and network allowlists, and fail-closed verification."
---

Without `-policy`, execlet enforces per-sandbox limits and a 64-slot admission
cap. A policy file turns that into a host that one caller cannot exhaust.

```sh
execletd -policy /etc/execlet/policy.json
```

The file is **strict**: unknown fields, nonpositive budgets, and unsupported
network modes prevent startup. A typo that silently disables a budget is worse
than a daemon that refuses to start.

```json
{
  "max_sandboxes": 4,
  "total_memory_mb": 2048,
  "total_nano_cpus": 4000000000,
  "total_pids": 4096,
  "allowed_images": ["sha256:REPLACE_WITH_APPROVED_IMAGE_ID"],
  "allowed_networks": ["none"],
  "templates": {
    "python-test": "sha256:REPLACE_WITH_APPROVED_IMAGE_ID"
  },
  "egress": { "domains": ["pypi.org", "files.pythonhosted.org"] }
}
```

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
| Forbidden image or network, explicit unlimited CPU | `400 invalid_request` |

## Reservation happens before Docker mutates anything

Concurrent creates reserve atomically before the backend call. Two simultaneous
requests cannot both see the last slot. If a create fails after leaving a
container behind, or a delete fails, that slot stays held until the runtime
**confirms** removal. `/metrics` exposes the backlog; do not treat it as a leak
in the accounting.

## Docker settings are re-verified, not trusted

Before admitting a container, the backend checks the Docker `HostConfig` **and**
reads the actual memory, swap, CPU, and pid limits from cgroup v2 inside the
guest. A mismatch, an unsupported controller, or an unreadable probe fails
closed and removes the container.

This is why images must supply `sh`, `cat`, `ls`, and readable cgroup v2 files.
A minimal image without them cannot be admitted, which is a deliberate trade:
admitting without verification would mean trusting a flag.

## Images

The allowlist compares **exact strings**. Prefer immutable digests: allowing a
mutable tag does not pin its content, so the tag can move to a different image
tomorrow and still be "allowed".

`allowed_runtimes` additionally restricts which Docker runtime a caller may
request, which is how you make gVisor available without making it universal.

## Networks

| Mode | Guarantee |
| --- | --- |
| `none` | Guest loopback only. No outbound internet, host, private network, or metadata route |
| `filtered` | Docker `none` plus a guest loopback HTTPS proxy; only configured domains over CONNECT :443 |
| `bridge` | **Deliberately unrestricted**; follows Docker routing, including private and host services |

Including `bridge` in the allowlist opts those sandboxes out of any no-egress
guarantee. That is a policy decision you are making, not a default.

### Filtered egress

The proxy listens at `127.0.0.1:8888` inside the guest. The host opens each
upstream connection through Docker exec, resolves the hostname, checks the
returned addresses, then dials the selected address directly. Every new tunnel
is re-checked, including after redirects. TLS payloads stay opaque.

Up to **64 tunnels** per sandbox; `egress.tunnel_idle_seconds` (default 120)
bounds inactivity, but a transfer with continuing traffic can outlive it.

<Warning>
Fake-IP DNS resolution returns addresses in `198.18.0.0/15`, which the policy
rejects. Configure real-address DNS for approved domains on the daemon host, or
every request will look like an attack.
</Warning>

### Credential brokering

`egress.credential` binds one GET request to a fixed HTTPS URL and a list of
authorized images. The workload calls `GET http://127.0.0.1:8888/credential`;
the host identifies the sandbox from its own exec channel and sends a freshly
constructed request.

The guest cannot choose the upstream path, headers, redirect handling, or
method — that is the entire point. The token comes from `token_env` on the
daemon's environment and never enters the sandbox.

## Editing a policy

Write the new document to a temporary file in the **same directory**, then rename
it over the original, so a reader never sees a half-written policy.

`templates` are re-read on each admission, so template additions and deletions
take effect **while the daemon runs**. Budgets and runtime settings require a
restart.

## Verifying a policy

```sh
python3 scripts/e2e-operations.py --server bin/execletd --image "$IMAGE" \
  --out artifacts/operations --samples 5
```

That suite exercises resource races, removal failures, journal corruption, and
storage outages — the cases where a policy is most likely to be quietly wrong.
