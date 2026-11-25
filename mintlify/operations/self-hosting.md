---
title: "Self-hosting"
description: "Run a private Execlet daemon, configure authentication, and check readiness."
---

Run `execletd` on a host you control with access to a Docker Unix socket.
Keep the daemon private and use it for one trust domain.

```text
your app / agent host  ──►  execletd (127.0.0.1)  ──►  Docker Engine (unix socket)
```

## Authentication and access

Set `SANDBOX_API_KEY` in the daemon's environment to require a matching
`Authorization: Bearer` header on API and metrics requests. The daemon
refuses to bind a non-loopback address without a key, but that check does
not enable TLS. Put a TLS-authenticated private proxy in front of routes
that must be reached remotely.

One key authorizes the **whole daemon**, not one tenant. Anyone with access
can inspect and kill any sandbox, read its files, and run any approved image.

| Deployment | Reasonable? |
| --- | --- |
| Loopback on a laptop for one developer | Yes |
| A private build host for one team, key held by their CI | Yes, with a policy file |
| Public internet endpoint | No |
| Two customers sharing one daemon | No |

<Warning>
Do not use one daemon as a security boundary between untrusted tenants.
An owner label separates cleanup, not security. Use separate hosts or an
independently isolated deployment for separate trust domains.
</Warning>

## First production checklist

```text
1. Bind to loopback and set SANDBOX_API_KEY for shared access.
2. Write a policy file. Budgets, images, and networks — all required.
3. Pin images by digest. A mutable tag is not a pin.
4. Set -state-dir on private local storage for restart recovery.
5. Set -owner to a unique value. Never share it between daemons.
6. Scrape /metrics for policy rejections and cleanup backlog. Track OOM outcomes in execution logs.
```

## Daemon flags that matter

| Flag | Why |
| --- | --- |
| `-addr` | Listen address; defaults to `127.0.0.1:8080` |
| `-docker` | Unix socket path; defaults to the `unix://` path in `DOCKER_HOST`, or `/var/run/docker.sock` |
| `-policy` | Absolute path to the admission policy; absent means no aggregate limits |
| `-state-dir` | Private directory enabling conservative restart recovery |
| `-owner` | Stable label for container cleanup and adoption; defaults to `-addr`, never share it between daemons on the same engine |
| `-guest-agent` | `auto` (default) or `off`; filtered egress and runsc require the guest agent |

<Note>
Without a policy file the daemon enforces only its built-in limits: 64
admission slots and per-sandbox resource limits. Use a
[policy](/operations/policy) to bound aggregate reservations. These budgets do
not, by themselves, cover all host resources such as the Docker image store.
</Note>

## Health and readiness

| Endpoint | Use |
| --- | --- |
| `GET /healthz` | The process is up. Use for a cheap liveness check |
| `GET /readyz` | Checks Docker connectivity and journal health. Returns 503 when either check fails |

These two endpoints do not require a bearer token. Use `/healthz` for
liveness and `/readyz` for readiness; Docker or journal errors do not imply
that the process needs to be restarted by its watchdog.

## Upgrading

When switching from tiny-sandbox, stop new admissions, save needed files,
and retire existing sandboxes using the old daemon. Stop it and
take a consistent backup of its state directory. A durable shutdown alone
can preserve idle containers. On the same Docker engine, set `OWNER` to the
old daemon's exact owner (its listen address if `-owner` was omitted) and run:

```sh
docker ps -aq --filter "label=tiny-sandbox.owner=${OWNER:?set the old owner}"
```

The command must succeed with no output. Even a stopped container with that
label prevents the renamed daemon from starting; it will not adopt it. If IDs
remain, reconcile them with the old deployment. Do not relabel containers,
pick a new owner, or delete the journal to get past the check.

For upgrades between Execlet versions, stop the daemon and back up the state
directory. In either case, check the new binary's version and reuse **your
existing** owner, state directory, policy, and other settings. These paths and
owner are examples:

```sh
./bin/execletd -version
./bin/execletd -state-dir /var/lib/execlet -owner prod-host-1 -policy /etc/execlet/policy.json
```

Use `/readyz` before restoring traffic. An older binary may not understand a
newer journal. Do not clear it to bypass a validation failure; consult
[Restart recovery](/operations/recovery).

## What is not included

Execlet does not provide clustering, failover or per-tenant authorization.
Each daemon must have its own owner and state directory. Two daemons with the
same owner can remove each other's containers, even with separate journals.
Policies on the same Docker host do not coordinate resource budgets.
