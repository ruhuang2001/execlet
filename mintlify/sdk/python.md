---
title: "Python SDK"
description: "A synchronous, dependency-free client with typed dataclasses and context-managed sandboxes."
---

## Install from source

Set `EXECLET_SRC` to the absolute path of the [checkout](/installation#use-the-sdks-from-a-checkout)
that built your daemon. From your application directory, create a virtual
environment and install from that checkout:

```sh
uv venv --python python3 .venv
. .venv/bin/activate
uv pip install "$EXECLET_SRC/sandbox-python"
```

Without uv, create the venv with `python3 -m venv .venv`, activate it, and run
`python -m pip install "$EXECLET_SRC/sandbox-python"`.

The distribution and import are both named `execlet`. The SDK is synchronous,
requires Python 3.10+, and has no runtime dependencies outside the standard
library. Create a new client in each child process after `fork`; using or
closing an inherited client raises `forked_client`.

## Run a command and stream it

With the daemon running, save this as `demo.py` and run `python demo.py`:

```python
import sys

from execlet import Client, CreateSandboxRequest

with Client() as client:
    with client.sandbox(CreateSandboxRequest(image="python:3.12-alpine", network="none")) as sandbox:
        client.exec_stream(
            sandbox.id,
            ["python3", "-c", "print(2 ** 10)"],
            on_stdout=lambda chunk: print(chunk, end="", flush=True),
            on_stderr=lambda chunk: print(chunk, end="", flush=True, file=sys.stderr),
        )
```

It prints `1024`. `client.sandbox(request)` creates a sandbox, yields it, and
requests removal on exit, including exceptional exit, using a fresh five-second
cleanup budget.

<Warning>
`with Client()` only closes client resources. It does **not** remove sandboxes
created through `create_sandbox`. Only `client.sandbox(...)` owns their
lifetime. Mixing the two is how you accumulate containers.
</Warning>

## Configuration

```python
client = Client(domain="", api_key="", max_retries=3)
```

| Option | Behavior |
| --- | --- |
| `domain` | Nonempty overrides `SANDBOX_DOMAIN`, then `http://localhost:8080`. A missing scheme gets `http://` |
| `api_key` | Nonempty overrides `SANDBOX_API_KEY` |
| `max_retries` | Additional attempts after the first, and the total reconnect budget per stream |

<Note>
`max_retries=None` means the default of 3, and explicit `0` disables retries.
This is **opposite** to Go and TypeScript, where `0` selects three.
</Note>

There is no global request timeout — one would cut off long-lived streams. Use
a `CancellationToken` for a local deadline or explicit caller cancellation.

## Requests are typed, and omission is meaningful

`CreateSandboxRequest` accepts `image`, `memory_mb`, `max_concurrent_executions`,
`pids_limit`, `nano_cpus`, `network`, `runtime`, `timeout_seconds`, `timeout_action`,
`idle_timeout_seconds`, `idle_timeout_action`, `env`, and `mounts`.

`None` omits a field. Explicit zero is sent, which is how you distinguish "use
the server default" from "disable this timeout". The client never invents a
default the server does not have.

<Columns cols={2}>
  <Card title="Create-time env" icon="box">
    Belongs to the sandbox.
  </Card>
  <Card title="Execution env" icon="terminal">
    Overrides the sandbox value for one invocation only.
  </Card>
</Columns>

Neither is echoed back in resource metadata.

## Volumes

When [persistent volumes](/operations/volumes) are enabled, pass `VolumeMount`
in `mounts` to attach a volume. This example uses a disposable volume and an
image allowed by your policy:

```python
from execlet import Client, CreateSandboxRequest, CreateVolumeRequest, VolumeMount

with Client() as client:
    volume = client.create_volume(CreateVolumeRequest(size_bytes=16 * 1024 * 1024))
    request = CreateSandboxRequest(
        image="alpine:3.20",
        network="none",
        mounts=[VolumeMount(volume_id=volume.id, path="/mnt/work")],
    )
    with client.sandbox(request) as sandbox:
        print(sandbox.id)
    client.delete_volume(volume.id)
```

Exiting `client.sandbox` removes the sandbox, not the volume. Omit the final
delete and keep `volume.id` to reuse the data.

Each volume method checks the `volumes.v1` capability first. `create_volume`
is attempted once. `get_volume` and `list_volumes` only return ready volumes.
`delete_volume` retries only an explicit HTTP 503. If its response is lost, it
raises `Error` with code `transport` without replaying. Repeat the delete to
reconcile: `204` confirms cleanup and `404` means the ID is absent.

## Changing a lifetime

```python
from execlet import UpdateSandboxRequest

client.update_sandbox(sandbox_id, UpdateSandboxRequest(extend_seconds=300))
```

`timeout_seconds` sets a new deadline relative to now. `extend_seconds` keeps
the later of the current deadline and now plus the supplied duration; it does
not add to the existing deadline. Supply exactly one field. A `PATCH` is **not**
automatically replayed after an ambiguous response — extending twice is not the
same as extending once.

## Errors

```python
from execlet import Error

try:
    client.exec(sandbox_id, ["false"])
except Error as error:
    print(error.code)
    if error.execution is not None:
        print(error.execution.termination_reason)
```

`error.code` is the stable field to branch on; the message text is not.

`exec` and `exec_stream` raise `Error` for failed commands, including
`nonzero_exit` for an ordinary nonzero exit. The exception can carry the final
`execution`. To handle the terminal snapshot yourself, call `start_execution`
with an `ExecuteRequest`, then `stream_execution` with the returned ID.
`stream_execution` returns the snapshot regardless of the command's outcome;
transport, API, and observation errors can still raise.

## Streaming behavior

`exec_stream` reconnects with `Last-Event-ID` and suppresses duplicates. The
retry budget is cumulative per call and does not reset when output arrives.

A callback that raises closes the stream and surfaces `Error` with code
`callback`; it does not cancel the command. Canceling an active observation via
`CancellationToken` does request remote cancellation by default. Pass
`cancel_remote=False` to `stream_execution` to stop waiting without canceling,
and check for `cancel_unconfirmed` before assuming cleanup succeeded.
