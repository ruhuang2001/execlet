---
title: "execlet"
description: "Run the code your agent writes — predictably, locally, in Docker."
---

execlet runs untrusted code in local Docker containers and gives you an honest
account of what happened. It is one daemon, one HTTP API, a CLI, and SDKs for
Go, Python, and TypeScript.

Running code is easy. The hard part is everything after it:

- The stream dropped. Did the command stop, or is it still running?
- It exited 137. Was that out of memory, a timeout, a cancellation, or the host?
- The caller cancelled. Are the grandchildren gone too?
- Which requests are safe to retry, and how does a retry avoid running the
  command twice?

execlet answers those with working code instead of prose: executions are
resources with IDs, output reconnects losslessly, failures are typed, and
cancellation is verified rather than assumed.

<Columns cols={2}>
  <Card title="Quickstart" icon="rocket" href="/quickstart">
    Zero to running Python in about five minutes. Needs only Docker.
  </Card>
  <Card title="Execution semantics" icon="activity" href="/concepts/execution">
    Reconnect, replay, and cancellation. The reason this project exists.
  </Card>
  <Card title="API reference" icon="braces" href="/api-reference/sandboxes/create">
    Every endpoint, generated from the OpenAPI contract.
  </Card>
  <Card title="Self-hosting" icon="server" href="/operations/self-hosting">
    Policy, templates, recovery, and metrics for a long-lived daemon.
  </Card>
</Columns>

## What it is not

**execlet is not a security boundary for untrusted multi-tenant workloads.** It
uses Docker container isolation, which keeps a sandbox from damaging your
host's files but does not separate mutually hostile tenants. Use a dedicated
runtime host, keep the listener private, and read [Isolation](/operations/isolation)
before promising anyone anything.

State is in memory by default. Opt-in recovery restores sandbox records and
filesystems, not running processes.

## The shape of the API

```text
your agent  ──►  execletd  ──►  Docker Engine
   (SDK or CLI)      │
                     └─ sandbox = container + lifecycle record
                        execution = one command, with an ID and an output stream
```

A sandbox is created, runs commands, can be paused and resumed, and is finally
killed. Commands inside it are executions, and an execution outlives the
connection that watched it.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/ruhuang2001/tiny-sandbox/main/scripts/install.sh | sh
```

That is the whole install: a `execletd` daemon and a `execletctl` CLI, for macOS
and Linux on amd64 and arm64. No Go, no Python runtime, no model API key.

<Card title="Build from source instead" icon="hammer" href="/installation">
  For unreleased features, a pinned commit, or contributing. Requires Go 1.27+.
</Card>
