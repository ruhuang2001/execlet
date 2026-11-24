---
title: "Isolation"
description: "What the container hardening actually buys, and how to run under gVisor."
---

execlet applies hardening and resource limits that stop common sandbox escapes
from becoming host problems. It does not claim a hostile-tenant boundary.

Read this page before telling anyone something is "isolated".

## The fixed settings

Each one exists because of a specific failure that reached past the sandbox.

| Setting | Why it is there |
| --- | --- |
| **Pids limit, default 1024** | A fork bomb costs task slots, not memory. Memory limits alone do not stop it; without a pid limit it runs until the host PID table is empty and every other sandbox stalls |
| **`sleep infinity` never calls `wait`** → init as PID 1 | A pids limit plus an init that never reaps is a slow leak: every orphaned child stays a zombie holding a slot, so a sandbox that forked once eventually cannot fork at all. Docker's init reaps them |
| **`NET_RAW` and `MKNOD` dropped** | No raw packet crafting, no device nodes |
| **`no-new-privileges`** | A setuid binary in the image cannot raise privileges inside the container |

The rest of Docker's default capability set is kept because `apk`, `pip`, and
`apt` were verified to install under it. The e2e suite forks a thousand
children and then runs a fresh multi-process command, which fails if the slots
were not released.

## CPU is unlimited unless you say otherwise

Defaulting CPU to a limit would make legitimate work mysteriously slow, so it is
a create-time option (`nano_cpus`), matching Docker's own default. The
consequence is real: without one, a busy loop competes with the daemon and with
every other sandbox on the host. In a shared deployment, set it in the policy.

## Network

| Mode | Guarantee |
| --- | --- |
| `none` | Loopback only |
| `filtered` | Loopback + HTTPS proxy to allowlisted domains, CONNECT :443 |
| `bridge` | Unrestricted, follows Docker routing |

The mode is fixed at creation because Docker cannot change a container's network
afterwards. Host networking is not offered. See
[Admission policy](/operations/policy#networks).

## Running under gVisor

gVisor (`runsc`) is a Docker runtime, so you select it per sandbox rather than
choosing a different backend:

```json
{ "allowed_runtimes": ["runc", "runsc"] }
```

The first entry is the default. A client sets `runtime: "runsc"` on create, and
must check the `sandbox.runtime.v1` capability first — an older server would
ignore the unknown field and silently pick its default.

What is verified rather than assumed:

- **Policy verification requires the daemon to run on the Linux Docker host**, with access to its process table and cgroup tree. runsc exposes a virtualized cgroup hierarchy, so admission fails if the matching scope or a limit cannot be checked.
- **Filesystem transfer uses a guest helper** for live operations, because runsc's private overlay and `/tmp` can diverge from Docker's archive view of the container.
- **The backend selects the transfer path at creation**, including when Docker chooses its default runtime, and again during journal recovery.

```sh
make runtime-e2e    # real runc and runsc admission, limits and policy
```

## Where the guarantee stops

| Claim | True? |
| --- | --- |
| A sandbox cannot write to your host's filesystem | Yes |
| A sandbox cannot read another sandbox's files | Yes |
| A fork bomb cannot stall unrelated sandboxes | Yes, with a pid limit |
| A sandbox cannot reach private network services | Only with `none` or `filtered` |
| A kernel exploit stays inside the sandbox | **No** under runc; part of gVisor's design goal under runsc |
| Two mutually hostile tenants can share one daemon | **No** |

<Warning>
The acceptance suites verify policy admission, allowlist enforcement, and
resource limits against a reachable private peer and denied metadata attempts.
They do **not** prove resistance to a Docker or kernel escape. If your threat
model includes that, run gVisor, keep one trust domain per host, and say so
plainly to whoever relies on it.
</Warning>
