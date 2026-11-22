# tiny-sandbox Python SDK

Synchronous, standard-library-only client for tiny-sandbox. Requires Python
3.10 or newer and a running `sandboxd` with Docker. Use client and server
builds from compatible revisions.

Install from the repository root:

```sh
python3 -m pip install ./sandbox-python
```

```python
from tiny_sandbox import Client, CreateSandboxRequest

with Client() as client:
    with client.sandbox(CreateSandboxRequest(image="alpine:3.20")) as sandbox:
        execution = client.exec(sandbox.id, ["echo", "hello"])
        print(execution.stdout, end="")
```

Set `SANDBOX_DOMAIN` and, when required, `SANDBOX_API_KEY` for your server.
The sandbox context manager requests cleanup on exit. `exec` raises `Error`
for nonzero exits and other unsuccessful outcomes; inspect `error.code` and
`error.execution` for details.

The client supports execution-local environment and working directory, one-shot
stdin, paginated directory listing, and tar transfers. See the
[SDK guide](../docs/sdk-python.md) for methods, cancellation, retries, and
repeatable end-to-end verification.

### Native managed-command access

These additions require a compatible server; existing native argv, timeout
defaults and graceful `cancel_execution` semantics are unchanged:

- `client.get_capabilities()` returns `Capabilities(capabilities=[...])` from
  authenticated `GET /v1/capabilities`. Results are not cached across calls.
- `ExecuteRequest(..., timeout_ms=1250)` selects a managed command with a
  precise remote deadline. Explicit `timeout_ms=0` selects the same supervisor
  without a command deadline. This field is mutually exclusive with
  `timeout_seconds`; omitting both preserves the native default.
- `Execution.pid` is an optional positive guest PID, never a fabricated ID.
- `client.get_command(sandbox_id, pid)` returns `CommandAttachment` with
  `execution` and an atomic `last_event_id` boundary. It does not start, resume,
  or extend the sandbox. PID lookup selects the newest retained incarnation.
- `client.stream_execution(..., last_event_id=boundary)` starts after that
  boundary; the default remains zero. Native terminal snapshots still contain
  full retained output. A completed attachment needs no new stream request.
- `client.kill_execution(sandbox_id, execution_id)` returns `ExecutionKillResult`
  with `execution` and `signaled`. It requests immediate group SIGKILL, not
  graceful cancel. A true result requires confirmed termination; already
  completed commands return false. A lost acknowledgement is an error carrying
  the execution identity and is not automatically retried. Native missing
  resources remain `Error(code="not_found")`, not a fabricated result.

The native client checks `e2b.background.v1` before a managed start, attach,
or kill. A missing capability fails before the command request is sent.

## Optional E2B facade

`from tiny_sandbox.e2b import Sandbox, CommandHandle, CommandExitException` exposes the
E2B 2.64.0 basic Sandbox subset: lifecycle, Bash commands with callbacks and
compatible errors, text/binary files and complete bounded directory listings.
It has no E2B dependency or cloud-key requirement. Set `SANDBOX_E2B_IMAGE` to
the digest of the local compatibility image and use a server from the same
checkout. Cleanup is explicit or via `with Sandbox.create()`.

### Background commands and corrected timeouts

This checkout adds the source-derived [background command contract](../docs/e2b-background-commands.md).
It is not a claim of E2B Cloud parity or registry publication. **Behavior change
from v0.0.2:** both foreground and background `commands.run(timeout=...)` now
set a remote command deadline, rather than only stopping the caller's wait.
The facade requires the authenticated `e2b.background.v1` capability before
any command run, connect, or kill; an old server or missing flag raises
`AdapterError(code="incompatible_server")` without starting a command. Auth
failures keep their own family. The check uses the same endpoint and API key
as the command, with no shared cache or fallback. Mixed-version server pools
and downgrade during a session are unsupported.

```python
from tiny_sandbox.e2b import Sandbox

sandbox = Sandbox.create(timeout=300)
handle = sandbox.commands.run(
    "sleep 2; printf done > /home/user/result.txt",
    background=True, timeout=60,
)
saved_sandbox_id, saved_pid = sandbox.sandbox_id, handle.pid
handle.disconnect()

connected = Sandbox.connect(saved_sandbox_id)
observer = connected.commands.connect(saved_pid, timeout=30)
result = observer.wait(on_stdout=lambda chunk: print(chunk, end=""))
print(connected.files.read("result.txt"))
connected.kill()
```

- `commands.run(..., background=True)` returns a `CommandHandle` after a
  confirmed guest PID, even on fast exit. Omitted/false background returns
  `CommandResult`. Literal overloads and a dynamic-boolean union are typed.
- `handle.pid`, `handle.sandbox_id`, and `handle.execution_id` are read-only.
  `execution_id` is a tiny-sandbox diagnostic/native-recovery extension.
  PID reuse may select a newer command on a fresh lookup, but an existing
  handle always observes or kills its original execution.
- `handle.wait(on_stdout=..., on_stderr=...)` is the synchronous output
  consumer; background run and connect do not accept callbacks. Ordinary exit
  zero returns a result; ordinary nonzero raises `CommandExitException`. A
  terminal result or failure is cached, without new HTTP or callback dispatch.
- Fresh `commands.connect(pid)` observes only output after its atomic attach
  boundary, not retained history. A completed attach returns the real exit
  outcome with empty output. Same-handle transport recovery resumes after the
  last delivered event without repeating callbacks. Use native `get_execution`
  for full bounded logs and files for durable artifacts.
- `handle.disconnect()` is local and idempotent. A disconnected or failed
  handle cannot resume; create another with `commands.connect`. Observed
  terminal results remain readable after disconnect. Original callback
  exceptions and local timeouts are re-raised on subsequent waits.
- `handle.kill()` addresses the immutable execution ID; `commands.kill(pid)`
  resolves once in this sandbox, then kills that execution. Both return true
  only for a confirmed signal and termination, false for an already completed
  or absent command. Missing sandboxes remain sandbox-not-found errors. Kill
  still works on a disconnected or locally failed handle. Lost acknowledgements,
  uncertain termination, and backend containment fallback are errors, never
  ordinary success. The server's diagnostic code/message is preserved.

There are three independent clocks: sandbox lifetime, remote command deadline,
and local observation deadline. Run defaults to 60 seconds for both command
and observation; connect defaults to a fresh 60-second observation only. The
observation clock begins at run/connect entry, not the first `wait`. Reconnect
does not reset the remote deadline or renew sandbox TTL. Zero disables the
corresponding clocks, not sandbox lifetime. Python durations must be finite,
nonnegative seconds (not booleans); positive values use
`max(1, round(seconds * 1000))`, with a maximum of 9,223,372,036,854 milliseconds.
Thus 1.25 seconds is sent as 1250 milliseconds, not rounded to seconds.

A local timeout, failed callback, disconnect, or interpreter exit does **not**
cancel the command; its original remote deadline still applies. Local timeout
is not proof of remote termination. Remote timeout, OOM, eviction, unknown
termination and confirmed cancellation keep their diagnostic outcomes and
actual exit evidence rather than becoming invented exit 0/137 results.
Truncated output (including truncation before attach) raises
`AdapterError(code="output_truncated")` with identity and available partial
evidence. Retention remains bounded; handles do not pin server records.

Each handle is confined to its creating thread. Simultaneous/reentrant wait
consumers and cross-thread operations fail explicitly. PTY/iterator consumption,
interactive stdin, process listing, AsyncSandbox, arbitrary process discovery,
per-command `request_timeout`, and new `wait` timeout options are unsupported.
Only `stdin=False` and `retain_output=True` are accepted. Kill uses the
configured connection request budget (default 30 seconds); run/connect use
their remaining observation budget. Native graceful cancel is unchanged.

See the [migration example](../examples/e2b/README.md) and
[supported subset](../docs/e2b-compatibility.md) for lifecycle/files and image
requirements. The unit suite uses public APIs and loopback HTTP faults:

```sh
PYTHONPATH=sandbox-python/src python3.15 -B -m unittest discover -s sandbox-python/tests -v
```

These tests do not prove runtime PID/process-group safety. The installed
Docker background acceptance profile is the release gate; cloud behavior and
package publication are separate.
