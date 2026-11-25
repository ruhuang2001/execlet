---
title: "Templates and prewarming"
description: "Save a configured sandbox as an image, then keep containers ready so the first command is fast."
---

A template names an immutable image snapshot of a configured sandbox. A
matching prewarmed container lets a create request skip the image pull and
container startup. If none is available, the daemon creates a fresh container.

Templates keep **files**. They do not keep processes, memory, or execution
records. A sandbox created from a template starts fresh.

## Save a template

Start with a daemon using an [admission policy](/operations/policy). Prepare a
sandbox with the dependencies and files you want, then let writes finish. Set
`SANDBOX_ID` to its ID, `EXECLET_OWNER` to the daemon's `-owner` value, and
`POLICY_PATH` to its policy file. From the checkout root, with the Docker CLI
pointed at the same engine as the daemon:

```sh
python3 scripts/template-admin.py save \
  --policy "$POLICY_PATH" \
  --owner "$EXECLET_OWNER" \
  --sandbox "$SANDBOX_ID" \
  --name test-ready
```

`save` requires a new name. It imports a local Docker image and atomically
adds its ID to `templates` in the policy file. It does not push to a registry.
The daemon reads the updated mapping without restarting. With `SANDBOX_DOMAIN`
pointing at that daemon, try an image that provides `echo`:

```sh
./bin/execletctl run --image template:test-ready --rm -- echo template-ok
```

Use `template-admin.py delete --policy "$POLICY_PATH" --name test-ready` to
remove the alias. The tool may also remove its local snapshot tag when the
image is no longer approved. Existing sandboxes are separate from the alias.

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

A hard link that points at an excluded path fails the save, rather than
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

After saving `test-ready`, add this `prewarm` field to the policy file and
restart the daemon. The targets must fit within the policy budgets. Use the
same image, resource settings, network and runtime in the create request:

```json
{
  "prewarm": [
    {
      "image": "template:test-ready",
      "count": 1,
      "memory_mb": 256,
      "pids_limit": 128,
      "nano_cpus": 500000000,
      "network": "none"
    }
  ]
}
```

How the pool behaves:

- Standby containers count against the budgets **from the start of provisioning**, not from handoff.
- A request can claim an instance matching image digest, memory, CPU, PID limit, network, and runtime.
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

From the built checkout, run the local prewarm test with Docker available:

```sh
python3 scripts/e2e-prewarm.py --samples 10
```

This compares, on one engine, with a private loopback registry serving a unique
single-layer image: a cold pull, a local image, template resolution, and a
confirmed pool hit. Samples, P50, P95, saturation, and recovery results go to
`artifacts/prewarm-e2e/acceptance.json`.

Compare P50 and P95 from repeated runs on the same host. Keep cold image pulls
separate from runs that use a local image.
