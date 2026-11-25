---
title: "Execlet"
description: "Run agent-generated code in Docker and get a result you can act on."
---

Start a sandbox, run a command, and stream its output. If the connection drops,
reconnect by execution ID and replay retained output instead of running the
command again. When a command fails, use its typed termination reason to decide
what to do next.

<Columns cols={2}>
  <Card title="Quickstart" icon="rocket" href="/quickstart">
    Build the CLI and daemon, then run a Python program in Docker.
  </Card>
  <Card title="Execution and streaming" icon="wave-square" href="/concepts/execution">
    Resume a stream, set a deadline, or cancel a running command.
  </Card>
  <Card title="API reference" icon="file-code" href="/api-reference/sandboxes/create-a-sandbox">
    Start with the sandbox creation endpoint in the generated reference.
  </Card>
  <Card title="Self-hosting" icon="server" href="/operations/self-hosting">
    Configure resource limits, networking, and recovery on your Docker host.
  </Card>
</Columns>

## The shape of the API

```text
agent (SDK or CLI) ──► execletd ──► Docker Engine
                         │
                         └─ sandbox
                             └─ execution
```

A sandbox owns the container lifecycle. An execution is one command inside
that sandbox; closing an event stream does not stop it. State is in memory
by default. Opt-in [recovery](/operations/recovery) preserves eligible idle
containers and retained records, not live commands.

## Deployment boundary

Execlet is not a security boundary between mutually hostile tenants. Keep
the daemon private and read [Isolation](/operations/isolation) before running
code you do not trust. Use [source builds](/installation) for the renamed
daemon, CLI, and Go, Python, and TypeScript SDKs.
