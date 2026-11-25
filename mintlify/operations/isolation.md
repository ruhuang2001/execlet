---
title: "Isolation and Docker runtimes"
description: "Choose a Docker runtime and verify the limits it enforces."
---

Execlet uses Docker with resource limits and container hardening. Use it within
one trust domain. Neither the default Docker runtime nor gVisor turns a shared
Execlet daemon into a security boundary between mutually hostile tenants.

## Container settings

| Setting | Why it is there |
| --- | --- |
| `pids_limit` (default 1024) | Caps tasks in the Docker cgroup; the meaning differs under gVisor |
| Docker init as PID 1 | Adopts orphans and reaps them when they exit |
| `NET_RAW` and `MKNOD` dropped | Removes raw sockets and device creation from the default capabilities |
| `no-new-privileges` | Prevents gaining privileges through `exec`, for example via a setuid binary |

With no policy, CPU is unlimited unless you set `nano_cpus`, and networking
defaults to `bridge`. Use an [admission policy](/operations/policy) to cap
aggregate CPU, memory and tasks, and select `none` or `filtered` to restrict
[outbound network access](/operations/policy#networks). Host networking is not
offered through the API.

## Running under gVisor

Register `runsc` with Docker on the Linux host. Make it available alongside
`runc` by adding this field to a valid policy file:

```json
{ "allowed_runtimes": ["runc", "runsc"] }
```

The first entry becomes the policy default. Without `allowed_runtimes`, Execlet
uses Docker's configured default and does not accept a per-sandbox override.
Before sending `runtime: "runsc"` in a sandbox create request, check
`GET /v1/capabilities` for `sandbox.runtime.v1`. This checks the API feature;
it does not confirm that `runsc` is installed. Use `make build` so the daemon
embeds the guest agent required for runsc filesystem access.

## What differs under runsc

| Area | What to verify on your host |
| --- | --- |
| Admission | With a policy, `execletd` needs the Linux Docker host's `/proc` and cgroup v2 tree. It checks memory, swap, CPU and PID limits in the matching host scope. If it cannot verify them, creation fails. |
| Task limits and reaping | Host `pids.max` caps host tasks in that scope; it is not a cap of the same size on virtual guest PIDs. Reaching the host limit can terminate the whole sandbox. Check guest reaping and task pressure separately. |
| Pause and resume | Verify that the workload stops making progress and resumes. Do not infer that from a Docker `Paused` flag or assume the host cgroup freezer behaves identically under both runtimes. |
| Files and archives | Live transfers use the guest helper to read the actual runsc overlay. Verify the content and metadata you rely on, including after idle-container recovery. Ownership in downloaded archives is normalized, so it does not reliably represent guest user IDs. |
| OOM | Execlet reads `memory.events` in the matching host cgroup. Under either runtime, an OOM can take the whole sandbox down before a command reports an exit code. A scoped OOM kill does not identify which guest process was killed. Treat `oom`, `evicted`, and uncertain outcomes separately; exit 137 alone is not evidence. |

Use a dedicated local Linux Docker host with cgroup v2, `runc` and `runsc`
registered, and loop devices and ext4 tooling available. Build the binaries
and fixture as the normal build user. Run only the probes as root: the volume
test creates, mounts and cleans up loop-backed ext4 filesystems on the host.
From the checkout root:

```sh
make build runtime-image
sudo env "PATH=$PATH" python3 scripts/e2e-runtime.py
sudo env "PATH=$PATH" TMPDIR=/var/tmp python3 scripts/e2e-volumes.py
sudo env "PATH=$PATH" python3 scripts/e2e-templates.py --runtime runsc --out artifacts/runsc-template
```

Inspect `artifacts/runtime/report.json` and the reports under `artifacts/volumes/`
and `artifacts/runsc-template/` for checks and cleanup. The suite covers admission, pause/resume, archives, PID pressure,
orphan reaping and OOM evidence. It does not establish security against a
container escape or justify identical behavior across hosts. See
[Self-hosting](/operations/self-hosting) for deployment boundaries.
