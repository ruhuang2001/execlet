---
title: "Migrate from E2B"
description: "What maps cleanly, what changes, and the differences the adapter keeps explicit."
---

Execlet provides a synchronous Python facade (`execlet.e2b`) and a TypeScript
facade (`@execlet/sdk/e2b`) for part of the E2B Sandbox API. They adapt
application code to Execlet, not the E2B HTTP protocol. No E2B Cloud account
or key is needed.

## Run against a local image

Start a [daemon built from source](/quickstart#start-the-daemon) and install the
[Python](/sdk/python#install-from-source) or
[TypeScript](/sdk/typescript#install-a-local-build) SDK from the same checkout.
From the checkout root, build the compatibility image:

```sh
docker build -t execlet-e2b:local examples/e2b
export SANDBOX_E2B_IMAGE="$(docker image inspect execlet-e2b:local --format '{{.Id}}')"
export SANDBOX_DOMAIN="${SANDBOX_DOMAIN:-http://127.0.0.1:8080}"
```

`Sandbox.create()` accepts an omitted template or `base`. Set the `image`
option or `SANDBOX_E2B_IMAGE` to the immutable image ID or digest, not a tag.
The compatibility image includes Bash, Python, and a non-root user with
`/home/user`. It is not E2B's hosted base image.

```python
from execlet.e2b import Sandbox

with Sandbox.create() as sb:
    command = sb.commands.run("printf 'hello\\n'", background=True, timeout=10)
    result = command.wait()
    print(result.stdout, end="")
```

The TypeScript equivalents are `await Sandbox.create()`,
`await sb.commands.run(command, { background: true, timeoutMs: 10_000 })`, and
`await handle.wait()`. Use `await sb.kill()` in `finally` for cleanup.
`endpoint` and `api_key` (Python) or `apiKey` (TypeScript) override
`SANDBOX_DOMAIN` and `SANDBOX_API_KEY`; the facades do not read `E2B_*`.

## What to change in your application

| Operation | Execlet behavior |
| --- | --- |
| `Sandbox.connect(id)` | Reopens a running sandbox with its files and environment; can extend, but not shorten, its lifetime |
| `set_timeout` / `setTimeout` | Replaces the lifetime deadline and may shorten it |
| `commands.run` | Returns a result in the foreground or a `CommandHandle` in the background |
| `commands.connect(pid)` | Opens a new observer of a managed command from the current server watermark, not from the beginning of stdout |
| `handle.disconnect()` | Stops local observation only. `handle.kill()` targets the original execution and requests immediate SIGKILL |
| `files.read`, `files.write`, `files.list` | Single-file transfers up to 10 MiB and bounded, one-level listing. Relative paths resolve from the image user's home |

Keep the sandbox ID and Execlet's execution ID (`execution_id` / `executionId`)
for diagnostics. Guest PIDs can be reused; connecting by PID selects the
newest retained incarnation. A fresh command handle cannot resume a live
execution across a daemon restart. Interactive stdin, PTYs, Code Interpreter,
pause/resume, and volume management are outside this facade.

## Timeouts and failures

- `commands.run` sets a **remote command deadline** and a local observation
  deadline. The default is 60 seconds (`timeout` in Python, `timeoutMs` in
  TypeScript), including for a background start. `commands.connect` sets
  only a local observation deadline and does not reset the remote deadline.
- A local `TimeoutException` / `TimeoutError`, disconnect, or callback failure
  does not confirm that work stopped. Use available execution identity and
  partial output to reconcile the result before retrying. The remote command
  deadline and sandbox lifetime still apply.
- The combined retained output limit is 1 MiB, also bounded by event count.
  `AdapterError.code == 'output_truncated'` means output is incomplete, not
  an ordinary command exit. Sandbox cleanup failures also raise, including
  on a repeated kill call while removal remains unconfirmed.
- `run`, `connect`, and command kill require the `e2b.background.v1`
  capability. A server missing it fails with `incompatible_server`, not a
  fallback to the older timeout semantics.

For your own image and daemon revision, follow the [installed-package
acceptance instructions](https://github.com/ruhuang2001/execlet/tree/main/scripts/e2b-compatibility#setup).
Use the background profile with Python 3.11+, Node 22.19+, and the pinned
reference setup. The gate runs locally; it does not certify E2B Cloud parity.
