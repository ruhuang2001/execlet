# Python SDK

Status: normative. Contract for `sandbox-python`, the `tiny-sandbox` Python
distribution imported as `tiny_sandbox`. The SDK speaks the [HTTP contract](api.md).

## Installation and quick start

Python 3.10 or newer is required. The synchronous client has no runtime
dependencies outside the Python standard library. There is no async client.
Create a new client in each child process after `fork`. Using or closing an
inherited client raises `forked_client`; its sockets belong to the parent.
Install from a repository checkout:

```sh
python3 -m pip install ./sandbox-python
```

Start `sandboxd` with access to Docker, then run:

```python
from tiny_sandbox import Client, CreateSandboxRequest, Error

with Client() as client:
    with client.sandbox(CreateSandboxRequest(image="alpine:3.20")) as sandbox:
        try:
            execution = client.exec_stream(
                sandbox.id, ["sh", "-c", "echo hello; sleep 1; echo done"],
                on_stdout=lambda chunk: print(chunk, end="", flush=True),
                on_stderr=lambda chunk: print(chunk, end="", flush=True),
            )
            assert execution.exit_code == 0
        except Error as error:
            print(error.code)
            if error.execution is not None:
                print(error.execution.stderr)
            raise
```

`client.sandbox(request)` creates a sandbox, yields its `Sandbox` response,
and kills it on exit, including exceptional exit, using a fresh five-second
cleanup budget. `with Client()` only closes client resources; it does not
remove sandboxes created through `create_sandbox`. Use `kill_sandbox` to
manage their lifetime explicitly.

## Configuration

```python
client = Client(domain="", api_key="", max_retries=3)
```

- `domain`: nonempty values override `SANDBOX_DOMAIN`; otherwise use that
  environment variable, then `http://localhost:8080`. A value without a scheme
  gets `http://`.
- `api_key`: nonempty values override `SANDBOX_API_KEY`; empty values use it.
- `max_retries`: additional attempts after the first request, and the total
  reconnect budget for one stream. `None` uses the default of 3; explicit `0`
  disables retries. This differs from Go, where zero selects the default.

There is no global request timeout that would cut off a long-lived stream.
Use an optional `CancellationToken` for a local deadline or explicit caller
cancellation. Authentication uses the HTTP contract's Bearer token.

## Requests and responses

Requests are typed dataclasses. `CreateSandboxRequest` accepts `image`,
`memory_mb`, `max_concurrent_executions`, `pids_limit`, `nano_cpus`, `network`, `timeout_seconds`,
`timeout_action`, `idle_timeout_seconds`, `idle_timeout_action`, and `env`.
An optional value of `None` is omitted from JSON. Explicit zero is sent to the
server, preserving the difference between omission and disabling a timeout.
The [HTTP resource and request tables](api.md#resources) define units, defaults,
and accepted values; the client does not invent different defaults.
Create-time `env` belongs to the sandbox; execution `env` overrides it for one
invocation. Neither is echoed in resource metadata.

`UpdateSandboxRequest(timeout_seconds=..., extend_seconds=...)` sets exactly
one lifetime operation: replace the deadline or extend without shortening it.
`update_sandbox(id, request)` sends one PATCH, without automatic replay after an
ambiguous response. See the [lifecycle contract](api.md) for state constraints.

```python
from tiny_sandbox import CreateSandboxRequest, ExecuteRequest

sandbox_request = CreateSandboxRequest(
    image="alpine:3.20", memory_mb=256, pids_limit=512,
    nano_cpus=1_000_000_000, network="none", timeout_seconds=0,
)
execute_request = ExecuteRequest(
    command=["sh", "-c", "sleep 30"],
    idempotency_key="my-operation-1",
    timeout_seconds=5,
)
```

`ExecuteRequest(command=..., idempotency_key="", timeout_seconds=None, env=None, cwd=None, stdin=None)` describes
a start. An empty key causes the SDK to generate 128 random bits encoded as
hex, reused for every retry or recovery of that start. Keys are scoped to a
sandbox and only remembered while the execution is retained. Replaying a
retained key with the same command, `env`, `cwd`, and `stdin` returns that execution;
changing the timeout does not update its deadline. A different command, `env`,
`cwd`, or `stdin` produces `idempotency_conflict`.

`env` is a `dict[str, str]` added to the image's environment for this execution
only, and `cwd` is an absolute working directory; `None` omits either. The
[shared SDK contract](sdk.md) lists their limits and behavior, including exit
code 126 for a `cwd` that is not a usable directory. The server never echoes
`env`. `Execution.cwd` repeats the requested directory and is `None` when the
request set none. Older servers can silently ignore these fields on raw HTTP
requests, so the client requires `execution.env-cwd.v1` before sending them.

```python
started = client.start_execution(sandbox.id, ExecuteRequest(
    command=["sh", "-c", 'echo "$GREETING from $(pwd)"'],
    env={"GREETING": "hello"}, cwd="/tmp",
))
done = client.stream_execution(sandbox.id, started.id)
```

The `exec` conveniences take only a command, as in the Go SDK. To set `env` or
`cwd`, put them on the request to `start_execution`, then pass the returned ID
to `stream_execution`.

`Sandbox`, `Execution`, and `Event` are keyword-only response dataclasses.
Resource fields use Python snake_case names matching the JSON contract.
Unknown resource response fields are retained in `extra` and do not fail decoding.
Unknown state, reason, network, or action strings are preserved rather than
rejected by a closed enum. Missing additive resource fields from an older
server are `None`, not assumed server defaults. Do not use missing resource
limits as evidence that the server enforced them. The package includes
`py.typed` for type-aware callers.

## Methods

All methods are synchronous. Every request or stream method accepts optional
keyword-only `cancel=token`; omitting it means no caller deadline.

| Area | Methods |
|---|---|
| Sandboxes | `create_sandbox(request)`, `get_sandbox(sandbox_id)`, `update_sandbox(sandbox_id, request)`, `list_sandboxes()`, `pause_sandbox(sandbox_id)`, `resume_sandbox(sandbox_id)`, `kill_sandbox(sandbox_id)` |
| Executions | `start_execution(sandbox_id, request)`, `get_execution(sandbox_id, execution_id)`, `list_executions(sandbox_id)`, `cancel_execution(sandbox_id, execution_id)` |
| Streaming | `stream_execution(sandbox_id, execution_id, *, on_event=None, on_stdout=None, on_stderr=None)` |
| Conveniences | `exec(sandbox_id, command)`, `exec_stream(sandbox_id, command, *, on_event=None, on_stdout=None, on_stderr=None)` |
| Files | `read_file(sandbox_id, path) -> bytes`, `write_file(sandbox_id, path, data) -> Optional[FileEntry]`, `list_files(sandbox_id, path, *, limit=100, cursor="", cancel=None) -> FileList`, `read_archive(sandbox_id, path) -> bytes`, `write_archive(sandbox_id, path, data)` |

Files preserve raw bytes; paths must be absolute. Sandbox and execution lists return lists of
typed resources; directory listing returns a `FileList` page. Execution lists
omit retained output at the HTTP layer; get
or stream an execution to retrieve its output.

`exec` and `exec_stream` start an execution and wait for completion. They return
normally only for `termination_reason == "exited"` and `exit_code == 0`.
`stream_execution` returns the final `Execution` for all server outcomes,
including nonzero exit, timeout, cancellation, OOM, or eviction. Transport,
callback, and caller-cancellation failures still raise `Error`.

### Server deadlines versus caller cancellation

The current API supports a per-execution `ExecuteRequest.timeout_seconds`.
Call `start_execution` with that request, then `stream_execution` when you need
that deadline. The server applies it and reports `termination_reason="timeout"`;
the sandbox normally remains available. An omitted or zero timeout leaves the
execution without its own deadline. The sandbox lifetime policy still applies.

```python
from tiny_sandbox import CancellationToken, ExecuteRequest

execution = client.start_execution(
    sandbox.id, ExecuteRequest(command=["sh", "-c", "sleep 30"], timeout_seconds=5)
)
execution = client.stream_execution(sandbox.id, execution.id)
assert execution.termination_reason == "timeout"

token = CancellationToken(timeout=2)
client.exec(sandbox.id, ["sh", "-c", "sleep 30"], cancel=token)
```

`CancellationToken(timeout=None)` has no deadline. Call `token.cancel()` from
another thread to request cancellation. A token deadline is also caller
cancellation, so its error code is `canceled` or `cancel_unconfirmed`, never an
invented `timeout`. A token already canceled before a start or ordinary request sends no request.
Passing an already canceled token to stream_execution requests cancellation
of that existing execution without opening its stream.

If cancellation interrupts a start, the SDK repeats the start with its existing
key on an independent five-second budget, then cancels the recovered execution.
The same total budget covers start replay, cancellation, and confirmation; each
step does not receive a new five seconds. During streaming, caller cancellation
uses a fresh five-second total budget to cancel and confirm the execution.

For explicit wait-only behavior, `start_execution` and `stream_execution`
accept `cancel_remote=False`. An interrupted in-flight wait raises `wait_timeout`
with the start key or execution identity, without remote cancellation. Pass it
to both calls when composing a start and stream. Pre-canceled starts still send
nothing. The default remains `True`; `exec`/`exec_stream` keep remote cancellation.
The optional [E2B facade](../examples/e2b/README.md), imported as `tiny_sandbox.e2b`,
uses this wait-only path, Bash command strings and compatible error families.
It does not change the native argv API or require an E2B dependency.

Confirmation requires a terminal execution snapshot. If the cancel response
is still running, the SDK polls with `get_execution` within the remaining
budget; it does not send another cancel or kill. Both IDs must match on every
snapshot. A terminal reason of `unknown` (or `exited` without an exit code)
does not confirm termination. If confirmation
fails, it raises `cancel_unconfirmed` carrying the execution IDs when known and
the start's idempotency key when applicable. Server cleanup may still continue;
retain those identifiers to inspect or clean up later. A confirmed terminal
snapshot is attached to the `canceled` error; inspect it because natural
completion can race cancellation. Plain `cancel_execution` returns the server's
latest snapshot, which may still be running; it does not add this confirmation
loop.

### One-shot stdin

`ExecuteRequest.stdin` sends UTF-8 text once, followed by EOF; omitted or empty input
means EOF. The limit is 1 MiB of decoded bytes, including NULs. Execution-start JSON has
a separate 8 MiB limit; command metadata remains limited to 1 MiB. Set input on the
request to `start_execution`, then stream the returned execution. This is not
interactive input. The server does not return stdin in execution metadata, but a command
can print it to output. See the [input contract](api.md#one-shot-stdin) for image
requirements and cleanup behavior. Use compatible client and server revisions; older
servers can ignore unsupported request fields.

### Concurrent executions

Set `CreateSandboxRequest.max_concurrent_executions` when creating a sandbox: the
default is 1 and the accepted range is 1–8. Start each execution separately and stream
its ID. A full sandbox returns `busy`. Cancellation normally affects one execution;
pause/resume applies to all work, and container-removal fallback can terminate siblings.
With a configured limit above one, an unexplained exit 137 is `unknown`: `exec` raises
`Error(code="unknown")`. See the [concurrency policy](api.md#concurrent-execution-policy).

## Streaming and callbacks

`on_event(event)` receives typed events; `on_stdout(chunk)` and `on_stderr(chunk)`
receive strings. Callbacks run synchronously in the reading thread, so keep them short.
`on_event` includes `stdout`, `stderr`, `truncated`, and `complete`. If using both
general and output callbacks, avoid rendering the same chunk twice.

On disconnect, the SDK reconnects with `Last-Event-ID` set to the last fully delivered
event ID and ignores replayed IDs already delivered. Partial frames do not advance that
cursor. Reconnects share one `max_retries` budget for the entire stream; receiving
output does not replenish it. SSE reconnects use backoff but do not apply `Retry-After`.
A successful `complete` event supplies the final execution.

An exception from any callback becomes `Error(code="callback")`, chained to that
exception, even if the callback raised another SDK `Error`. The stream closes without
retrying or canceling the remote execution. Explicitly call `cancel_execution` if remote
work should stop. Other stream failures also leave remote work running; only caller
cancellation invokes automatic cancellation.

Replay covers retained output, not unlimited output. A `truncated` event or
`execution.output_truncated` indicates data was discarded; reconnecting cannot recover
it. Output follows the server's UTF-8 text contract, including U+FFFD replacement of
invalid bytes. Encode binary stdout before sending it. SSE lines and complete event
frames are bounded to 16 MiB, covering the largest valid server event. Go bounds
individual lines to the same size.

## Files

Listing requires `files.list.v1`; both archive methods require `files.archive.v1`.
Ordinary `read_file` and `write_file` skip capability discovery.

`client.list_files(sandbox_id, path, limit=100, cursor="")` returns a `FileList` with
`entries` and `next_cursor`. Pass `next_cursor` as the next call’s `cursor`. An optional
`cancel=token` follows the normal request cancellation contract. Entries describe
immediate children with name, size, mode, modification time, and directory status.
`type`, `uid` and `gid` are required; `owner`, `group` and `symlink_target` may
be absent. Malformed known fields raise `decode`. Future fields are retained
in the model's `extra` dictionary, and type strings are open to new values.
Follow the returned cursor until it is empty or absent. Pages are name-ordered, not a
filesystem snapshot. The default page size is 100, with a maximum of 1000; bounded scans
can fail with HTTP 413. See [directory listing](api.md#directory-listing) for cursor and
metadata semantics.

`read_archive(sandbox_id, path)` returns tar `bytes`; `write_archive(sandbox_id, path, data)` uploads them. Both accept keyword-only `cancel=token`. Downloads are rooted at
the selected path's basename; uploads extract beneath an existing directory. Archive
downloads retry normally. Uploads make one attempt because extraction can be partial:
stop in-container filesystem writers and inspect remote state after a failed or
ambiguous upload. Only relative link targets without `..` are accepted; hard links must
target an earlier regular member. See the [tar contract](api.md#tar-transfer) for
validation and transfer limits.

## Errors

Operational SDK failures use `Error`. Its stable fields are `code`, `status_code`
(0 for client-side failures), `execution`, `idempotency_key`, and `cause`.
Python exception chaining preserves the underlying exception where available.
Prefer checking `error.code` for exact classification. `error.matches(code)`
also supports status and exit-code matching: status 404 matches `not_found`,
status 401 matches `unauthorized`, and a retained nonzero exit code matches
`nonzero_exit` even if the termination reason is different. There is no
exception subclass per API code. The package exports the string constants
`ERR_NONZERO_EXIT`, `ERR_TIMEOUT`, `ERR_CANCELED`, `ERR_OOM`, `ERR_EVICTED`,
`ERR_CANCEL_UNCONFIRMED`, `ERR_NOT_FOUND`, `ERR_UNAUTHORIZED`, `ERR_BUSY`,
and `ERR_INVALID_STATE` for use with `matches` or direct code comparison.

```python
from tiny_sandbox import Error

try:
    result = client.exec(sandbox.id, ["sh", "-c", "echo useful-output; exit 7"])
except Error as error:
    if error.code == "nonzero_exit" and error.execution is not None:
        print(error.execution.exit_code, error.execution.stdout)
    else:
        raise
```

Outcome codes include `nonzero_exit`, `timeout`, `canceled`, `oom`,
`evicted`, and `unknown`. `cancel_unconfirmed` means stopping remote work was not confirmed;
`callback` means application callback code failed. API failures retain codes
such as `not_found`, `unauthorized`, `busy`, and `invalid_state`, along with
HTTP status. Unknown future termination reasons remain observable as errors in
`exec` and `exec_stream`, never silent success. Exit code 137 alone does not
mean OOM. When `execution` exists, it carries retained output and truncation
metadata; start or transport failures may have none.

## Retries

Ordinary calls reuse fully consumed keep-alive connections (up to eight idle
per client). A stale socket failure counts as a request attempt, not a free
retry. Streams do not borrow pooled sockets. `close()` closes idle sockets.

The policy matches Go except for configuring zero retries:

- Retry transport failures before a complete response, truncated bodies, and
  statuses 429, 500, 502, 503, and 504.
- Repeat only safe operations: GET, DELETE, ordinary file PUT (archive PUT excluded), pause, resume, cancel,
  and execution starts carrying an idempotency key. Sandbox creation is
  attempted once, including on a lost response.
- For retry index `n` starting at zero, the cap is
  `min(2 seconds, 200 milliseconds * 2**n)`; uniformly sample a delay from
  half that cap to the cap.
- A positive integer `Retry-After` in seconds overrides ordinary request
  backoff. Zero, invalid values, and HTTP-date values use calculated backoff.
- Caller cancellation interrupts requests and backoff. Recovery uses the
  independent cleanup budget described above.

## Compatibility and differences from Go

The wire API, idempotency rules, safe retry set, retry status set, jitter,
constant reconnect budget, retained-output replay, and file operations
match the Go contract. Additive response fields and unknown string values are
accepted. The current client calls authenticated `get_capabilities()` before
nonempty env/cwd/stdin, explicit concurrency and managed `timeout_ms` requests;
missing support raises `incompatible_server` before mutation. Discovery is read
afresh with the current endpoint/credentials. Basic legacy calls skip it; raw
HTTP can still have unknown fields ignored by old servers. This is not runtime
image validation or support for mixed-version load balancers.

Native `get_command` returns the execution and atomic event watermark;
`kill_execution` sends one immediate managed kill, without retrying uncertain
acknowledgements. The optional `tiny_sandbox.e2b.CommandHandle` supplies
background run/connect/wait/disconnect/kill; see the [package examples](../sandbox-python/README.md)
and [normative contract](e2b-background-commands.md). Native graceful cancellation
and seconds-based deadlines remain unchanged.

Important differences:

1. Python's `max_retries=0` disables retries; Go's zero uses its default.
2. Both SDKs poll a matching `running` cancellation response within the same
   cleanup budget and require a confirmed terminal outcome.
3. Both SDKs wrap callback failures as `callback`, even SDK-typed failures, and
   stop observation without retrying or canceling the remote execution.
4. Python's `matches("nonzero_exit")` also matches retained nonzero exit codes
   from timeout, OOM, or other reasons. Go requires an `exited` reason for this
   fallback. Check Python's exact `error.code` to distinguish these outcomes.

Python uses synchronous callbacks, dataclasses, one exception type, and context
managers instead of Go contexts, pointer options, and `errors.Is`.

## End-to-end verification

The end-to-end suite uses a real `sandboxd` and Docker, with an HTTP fault proxy
for dropped connections and replay. The central scenario runs a long command,
drops its stream, verifies reconnect with `Last-Event-ID` and output
deduplication, then cancels and requires a terminal snapshot. It also exercises
unconfirmed cancellation instead of treating an accepted request as success.

```sh
python3 -m pip install build
python3 -m build --outdir artifacts/python-dist sandbox-python
python3 -m pip install --force-reinstall artifacts/python-dist/*.whl
PYTHON=python3 SDK_INSTALLED=1 bash scripts/e2e-python.sh artifacts/python-e2e
```

The suite retains a JSON report, HTTP proxy trace, daemon logs, source identity,
and cleanup results under `artifacts/python-e2e`. Without `SDK_INSTALLED=1`,
the launcher creates a virtual environment and installs a wheel itself. See the
[failure models](testing/README.md). CI runs
the installed package against real Docker on Python 3.10 and 3.15 and uploads
evidence even on failure. The wheel and source distribution are verification
artifacts, not publication to a package index.


### Terminal, metadata and purge operations

`stat_file(sandbox_id, path)` returns one FileEntry and checks `files.stat.v1`. A legacy 204 write returns `None`.
`open_terminal(sandbox_id, *, rows=24, cols=80, command=None, cwd=None)` returns a terminal ID. Use `terminal_input`, `resize_terminal`, `stream_terminal` and `close_terminal` with that ID. `stream_terminal` yields bytes and supports an initial byte offset. These calls check `terminals.v1`; all accept a cancellation token. `destroy_sandbox(sandbox_id)` checks `sandbox.purge.v1` and removes retained records.
