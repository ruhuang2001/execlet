---
title: "Templates and prewarming"
description: "Save a configured sandbox as an image, then keep containers ready so the first command is fast."
---

A template is a named, immutable image snapshot of a sandbox you configured.
Prewarming keeps containers started from that template alive, so a request pays
for neither image pull nor process startup.

Templates keep **files**. They do not keep processes, memory, or execution
records. A sandbox created from a template starts fresh.

## Save a template

Install dependencies first, then save — whatever you saved is what you get.

```sh
python3 scripts/template-admin.py save \
  --policy /etc/execlet/policy.json \
  --owner prod-host-1 \
  --sandbox "$SANDBOX_ID" \
  --name python-test
```

```sh
execletctl run --image template:python-test --rm -- python3 -m pytest
```

Save requires a **new** name. Then make it reachable:

```json
{
  "templates": { "python-test": "sha256:<the published digest>" }
}
```

The policy is re-read per admission, so a new template becomes available while
the daemon runs. Budgets and runtime settings still need a restart.

## What the snapshot contains — and what it drops

Excluded, because they are runtime state and would be wrong or unsafe to carry:

| Path | Why |
| --- | --- |
| `/tmp`, `/var/tmp`, `/run` | Session state |
| `/dev`, `/proc`, `/sys` | Kernel-backed |
| Each mounted volume | Not part of the container filesystem |
| `/.execlet`, `/.dockerenv` | Control and runtime markers |
| Docker-managed `/etc/hosts`, `/etc/hostname`, `/etc/resolv.conf` | Per-container |

<Warning>
Everything else is captured — **including project-local settings and any
credentials you wrote to disk**. A token left in `~/.config` is now in an image
that other sandboxes can start from. Clean the sandbox before saving it.
</Warning>

A hard link that points at an excluded path fails publication, rather than
silently producing a broken image.

## Runtime differences

**runc:** Docker briefly freezes the source during commit. The tool exports a
stopped copy, filters its tar, and imports a local image.

**runsc:** Run the same command on the Linux Docker host with the `runsc`
binary installed. Docker pauses writers while the tool captures the lower
filesystem and gVisor's upper layer via `runsc tar rootfs-upper`, then merges
them — applying deletions, directory replacements, permissions and hard links.
A running source resumes afterwards; a paused source stays paused. Pass
`--runsc-root DIR` if `runsc` keeps its state somewhere other than
`/var/run/docker/runtime-runc/moby`.

The template preserves the base image's architecture, user, working directory,
environment, entrypoint, command, and volume declarations. Processes and
execution records start fresh.

## Prewarming

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

How the pool behaves:

- Standby containers count against the budgets **from the start of provisioning**, not from handoff.
- A request claims an instance matching image digest, memory, CPU, pid limit, network, and runtime under one lock.
- A miss takes the ordinary creation path, subject to quota.
- The pool assigns the sandbox ID, Docker name, and ownership labels when the container is provisioned, and admission claims that identity atomically.
- The sandbox **lifetime starts at handoff**, not at provisioning. A standby that sits idle does not burn someone's lifetime.
- Each instance serves one caller and is discarded on deletion, never recycled.

Failed provisioning logs a warning and backs off per profile from 2 to 60
seconds. `execlet_prewarm_ready` reports instances ready for handoff.

<Note>
Changing a template's digest switches new requests to the new digest; the pool
then removes excess standbys of the old one. Deleting an alias leaves its pool
target dormant, including across restarts — registering the name again reactivates it.
</Note>

## Measuring whether it helped

```sh
python3 scripts/e2e-prewarm.py --samples 10
```

This compares, on one engine, with a private loopback registry serving a unique
single-layer image: a cold pull, a local image, template resolution, and a
confirmed pool hit. Samples, P50, P95, saturation, and recovery results go to
`artifacts/prewarm-e2e/acceptance.json`.

Report P50 and P95 from that file rather than from a single warm run — the first
number after a pull is not a latency measurement, it is a network measurement.
