---
title: "Self-hosting"
description: "One daemon, one host, one trust boundary. What that means before you expose it."
---

execlet is designed to run as a single daemon on a host you control. That shape
is not a limitation to work around; it is the deployment model, and the
security properties follow from it.

```text
your app / agent host  ──►  execletd (127.0.0.1)  ──►  Docker Engine (unix socket)
```

## The trust boundary is the API key

One API key authorizes the **whole daemon**, not one tenant. Every caller with
that key can see and kill every sandbox, read every file, and run any allowed
image.

| Deployment | Reasonable? |
| --- | --- |
| Loopback on a laptop for one developer | Yes |
| A build host for one team, key held by their CI | Yes, with a policy file |
| Public internet endpoint | No |
| Two customers sharing one daemon | No |

<Warning>
"Do not use execlet as a security boundary for untrusted multi-tenant
workloads" is not boilerplate. Docker container isolation is a usability
feature here, not a hostile-tenant boundary. If you need tenants, use one
daemon (or one host) per tenant.
</Warning>

## First production checklist

```text
1. Bind to loopback. Put TLS + auth in front if anything else must reach it.
2. Write a policy file. Budgets, images, and networks — all required.
3. Pin images by digest. A mutable tag is not a pin.
4. Set -state-dir on private local storage for restart recovery.
5. Set -owner to a unique value. Never share it between daemons.
6. Scrape /metrics. Alert on policy rejection, cleanup backlog, and OOM.
```

## Daemon flags that matter

| Flag | Why |
| --- | --- |
| `-docker` | Explicit socket path, when context discovery is wrong |
| `-policy` | Absolute path to the admission policy; absent means no aggregate limits |
| `-state-dir` | Private directory enabling conservative restart recovery |
| `-owner` | Stable unique owner for container adoption; must not be shared |
| `-api-key` | Enables bearer authentication |

<Note>
Without a policy file the daemon enforces only its built-in limits: 64
admission slots and per-sandbox resource limits. A policy file is what turns
"some limits" into "a host that cannot be exhausted by one caller".
</Note>

## Health and readiness

| Endpoint | Use |
| --- | --- |
| `GET /healthz` | The process is up. Use for a cheap liveness check |
| `GET /readyz` | Docker is reachable and the daemon can serve. Use for readiness |

Point a liveness probe at `/healthz` and a readiness probe at `/readyz`. A
liveness probe on `/readyz` will restart the daemon every time Docker hiccups,
which turns a Docker problem into a container-churn problem.

## Upgrading

```sh
# stop the daemon, keep the state directory intact
# verify the new binary reads it
execletd -version
execletd -state-dir /var/lib/execlet -owner prod-host-1
```

Durable state is a journal, not a database. An older binary may not understand a
newer journal, so **do not downgrade against a state directory you cannot back
up**. Back it up before upgrading, and never clear state to get past a
validation error — that is how you lose the record of containers you still own.

## What is not included

No clustering, no leader election, no canarying, no automated rollback, no
multi-tenant authorization, and no billing. Running two daemons against one
Docker socket is not supported: they will each try to adopt the other's
containers, and `-owner` exists precisely because that must never happen.
