---
title: "Run repository tests from an MCP client"
description: "Sync a workspace into an offline sandbox and let an agent run pytest or go test against it."
---

`execletctl mcp` serves sandbox tools over stdin/stdout. An MCP client can
sync a workspace, run tests in an offline sandbox, and read the results. The
workspace is sent to the configured daemon; keep it local if the files must
stay on your host. Tool results go back to your MCP client, which may send them
to a model.

## Prepare the pytest demo

Build and start the daemon from the [quickstart](/quickstart#start-the-daemon).
From the checkout root, build the test image and check the example workspace:

```sh
docker build -t execlet-mcp:local examples/mcp
./bin/execletctl doctor mcp --workspace examples/mcp/demo --collect
```

On success, `doctor mcp` prints only MCP client configuration JSON, including
absolute paths and `SANDBOX_DOMAIN`. Merge its `execlet` entry into your client's
configuration (for example, `.cursor/mcp.json`). If authentication is enabled,
provide `SANDBOX_API_KEY` in the client's environment; the generated JSON does
not copy it. A failed check exits nonzero and prints diagnostics to stderr,
not a partial configuration to stdout.

<Warning>
`--collect` uploads the workspace and imports test modules **inside the
temporary sandbox**. This executes project code. Use it only for a project you
trust. Without `--collect`, the daemon only probes pytest in the image.
</Warning>

## What the agent can do

Use these tools to run a test and inspect the result:

| Tool | Effect |
| --- | --- |
| `create_sandbox` | Start an offline sandbox from an allowed image |
| `sync_workspace` | Upload the host workspace; large trees upload in bounded batches |
| `run_command` | Start an argv command and return its execution ID and available result; defaults to the synced workspace |
| `get_execution` / `cancel_execution` | Inspect retained output and status, or request cancellation |
| `read_file` / `write_file` | Read or write a UTF-8 file inside the sandbox |
| `destroy_sandbox` | Remove the sandbox and discard its local sync manifest |

Calls that address a sandbox renew its lifetime (except destruction). The
default is **1,800 seconds**. Set `--sandbox-ttl` to change it. Without calls to
renew it, the sandbox expires even if the client is still connected.

## The demo: a failing test, then a fix

The packaged pytest demo starts with one failing test, which makes it a real
exercise rather than a smoke test. The agent request that works:

> Create an offline sandbox, sync this workspace, and run `python3 -m pytest -q`.
> It should fail with `assert 21 == 14`. Fix `calculator.py` in my local
> workspace, sync again without reset, run the same test, and destroy the
> sandbox. Show both test results and the number of files in the second upload.

Two things to watch for in the transcript:

1. The second `sync_workspace` uploads **only the changed file**, not the tree.
2. `run_command` defaults to the synced directory after the first sync, so the agent
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

Reconnect also needs the CLI's local ownership receipt. `sandboxctl` kept it
under `~/.cache/tiny-sandbox/mcp`; `execletctl` uses `~/.cache/execlet/mcp`
and a different ownership scope. Copying the cache or reusing the ID does not
migrate a session. Save needed files and destroy old sandboxes with the old CLI,
then regenerate your client entry with `doctor mcp` and create new sessions.

## Your own project

Point `--workspace` at it, install locked dependencies when you build the image,
and pass `--image`. Rebuild the image when the lockfile changes — the sandbox is
offline, so it cannot install anything you forgot.

```sh
docker build -t my-tests:local -f my.Dockerfile .
"$EXECLET_SRC/bin/execletctl" doctor mcp --workspace . --image my-tests:local
```

Run this in your project root with `EXECLET_SRC` pointing at the
[built checkout](/installation#use-the-sdks-from-a-checkout). The printed
configuration starts `execletctl mcp` with the requested workspace and image.

For a Go project, `--profile go --check-deps` lists package and test
dependencies using the image's own toolchain and module cache with
`GOPROXY=off`; it does not run the tests. A missing offline dependency fails
the check rather than producing client config.

<Note>
The MCP server creates **offline** sandboxes by default and allows only the
images you pass. Add `--allow-network filtered` or `--allow-network bridge` only
if your daemon is configured for it and you actually need the network.
</Note>
