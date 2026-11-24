---
title: "Python SDK"
description: "A synchronous, dependency-free client with typed dataclasses and context-managed sandboxes."
---

```sh
pip install execlet
```

Python 3.10+. No runtime dependencies outside the standard library. There is no
async client; create a new client in each child process after `fork` — using or
closing an inherited client raises `forked_client`.

## The shortest useful program

```python
from execlet import Client, CreateSandboxRequest, Error

with Client() as client:
    with client.sandbox(CreateSandboxRequest(image="alpine:3.20")) as sandbox:
        execution = client.exec_stream(
            sandbox.id,
            ["sh", "-c", "echo hello; sleep 1; echo done"],
            on_stdout=lambda chunk: print(chunk, end="", flush=True),
            on_stderr=lambda chunk: print(chunk, end="", flush=True),
        )
        print(execution.exit_code, execution.termination_reason)
```

`client.sandbox(request)` creates a sandbox, yields it, and kills it on exit —
including exceptional exit — using a fresh five-second cleanup budget.

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
`pids_limit`, `nano_cpus`, `network`, `timeout_seconds`, `timeout_action`,
`idle_timeout_seconds`, `idle_timeout_action`, and `env`.

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

## Changing a lifetime

```python
client.update_sandbox(sandbox_id, UpdateSandboxRequest(extend_seconds=300))
```

`timeout_seconds` replaces the deadline; `extend_seconds` adds without ever
shortening it. Exactly one operation per request. A `PATCH` is **not**
automatically replayed after an ambiguous response — extending twice is not the
same as extending once.

## Errors

```python
from execlet import Error

try:
    client.exec_stream(sandbox_id, ["false"])
except Error as error:
    print(error.code)
    if error.execution is not None:
        print(error.execution.termination_reason)
```

`error.code` is the stable field to branch on; the message text is not.

Completed commands do not raise, even with a nonzero exit code. Only transport,
protocol, and API failures raise `Error`.

## Streaming behavior

`exec_stream` reconnects with `Last-Event-ID` and suppresses duplicates. The
retry budget is cumulative per call and does not reset when output arrives.

Returning an error from a callback closes the stream and raises — it does not
cancel the remote execution. That separation is intentional: a bug in your
printer should not kill a ten-minute build.
