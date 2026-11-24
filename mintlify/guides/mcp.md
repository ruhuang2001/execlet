---
title: "Run repository tests from an MCP client"
description: "Sync a workspace into an offline sandbox and let an agent run pytest or go test against it."
---

`execletctl mcp` serves sandbox tools over stdin/stdout, so any MCP client
(Cursor, Claude Desktop, an editor agent) can sync a workspace into an offline
sandbox, run commands, and read results — without network access from the
sandbox and without your project files ever leaving the host.

```sh
docker build -t execlet-mcp:local examples/mcp
execletctl doctor mcp --workspace examples/mcp/demo --collect
```

`doctor mcp` proves the loop works **before** you wire it into a client. On
success it prints only the client configuration JSON, with absolute paths:

```json
{ "mcpServers": { "execlet": { "command": "...execletctl", "args": ["mcp", "..."] } } }
```

Merge that into `.cursor/mcp.json`. A failed check exits nonzero, prints a
diagnostic to stderr, and prints **nothing** to stdout — so you can pipe the
output directly into a config file.

<Warning>
`--collect` syncs the workspace and imports test modules, which **executes
project code** on the host side of the check. Use it only for a project you
trust.
</Warning>

## What the agent can do

The server exposes eight tools. The useful ones:

| Tool | Effect |
| --- | --- |
| `create_sandbox` | Start an offline sandbox from an allowed image |
| `sync_workspace` | Upload the host workspace; large trees upload in bounded batches |
| `run` | Execute in the sandbox, defaulting to the synced workspace directory |
| `destroy_sandbox` | Release it |

A tool call renews the sandbox's lifetime, which defaults to **1,800 seconds**.
Set `--sandbox-ttl` to change it. Without a client attached, the daemon reclaims
the sandbox when the lifetime expires.

## The demo: a failing test, then a fix

The packaged pytest demo starts with one failing test, which makes it a real
exercise rather than a smoke test. The agent request that works:

> Create an offline sandbox, sync this workspace, and run `python3 -m pytest -q`.
> It should fail with `assert 21 == 14`. Fix `calculator.py` in my local
> workspace, sync again without reset, run the same test, and destroy the
> sandbox. Show both test results and the number of files in the second upload.

Two things to watch for in the transcript:

1. The second `sync_workspace` uploads **only the changed file**, not the tree.
2. `run` defaults to the synced directory after the first sync, so the agent
   does not need to `cd`.

## How sync actually behaves

| Behavior | Detail |
| --- | --- |
| Upload target | `/tmp/execlet-workspace` |
| Default `cwd` | That directory after the first sync; the image's working directory before it |
| Incremental sync | Hashes host files; an unchanged tree skips guest work entirely |
| `reset: true` | Replaces guest contents with a fresh host copy — use it when files were edited **inside** the sandbox |
| Per-file limit | 16 MiB |
| Reconnect | Use the existing sandbox ID with the same workspace, daemon, credentials, and allowed images and networks |

## Your own project

Point `--workspace` at it, install locked dependencies when you build the image,
and pass `--image`. Rebuild the image when the lockfile changes — the sandbox is
offline, so it cannot install anything you forgot.

```sh
docker build -t my-tests:local -f my.Dockerfile .
execletctl mcp --workspace . --image my-tests:local
```

For a Go project, `--profile go --check-deps` lists package and test
dependencies using the image's own toolchain and module cache with
`GOPROXY=off`. It does not run tests, so it is safe to run on a cold checkout.

<Note>
The MCP server creates **offline** sandboxes by default and allows only the
images you pass. Add `--allow-network filtered` or `--allow-network bridge` only
if your daemon is configured for it and you actually need the network.
</Note>
