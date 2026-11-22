# E2B background commands: next-profile contract

The next compatibility profile lets an application start a command, leave the
client process, reconnect by sandbox ID and PID, and wait or kill without
starting the work again.

**Status: implemented in the current source; not part of published v0.0.2.**
Decided 2026-11-18 against tiny-sandbox
`28912c69c245df6903536f7b9fac165340f64f56` (v0.0.2). This does not rewrite
that release. Its 19-case baseline is preserved for historical verification;
current CI uses the 40-case `--profile background` contract in both languages.
The [failure model and cases](testing/e2b-background-failures.md) are the
implementation gate. No E2B Cloud account, key, or cloud run is required.

## 1. Reference and a correction to the previous profile

Keep the published Python and TypeScript SDK baseline at **E2B 2.64.0**, with
the wheel hash and npm integrity in
[`reference.json`](../scripts/e2b-compatibility/reference.json). Inspect
Python `sandbox_sync/commands/command.py`, `command_handle.py`, and
`envd/utils.py`; TypeScript `Commands` and `CommandHandle` in the published
bundle and declarations. SDK source alone does not establish server behavior.

For the server-dependent rules below, freeze the separately inspected E2B
runtime source at **`391b22443460fb30432da889b44aa413478d5717`**:

- [Start](https://github.com/e2b-dev/runtime/blob/391b22443460fb30432da889b44aa413478d5717/packages/envd/internal/services/process/start.go) reads `Connect-Timeout-Ms` and creates a process deadline independent of client disconnect.
- [Spawn](https://github.com/e2b-dev/runtime/blob/391b22443460fb30432da889b44aa413478d5717/packages/envd/internal/services/process/handler/spawn_unix.go) uses that context for the child command.
- [Connect](https://github.com/e2b-dev/runtime/blob/391b22443460fb30432da889b44aa413478d5717/packages/envd/internal/services/process/connect.go) subscribes to an existing process; its request deadline does not replace the process deadline. It can serve a retained terminal event.
- [Output fan-out](https://github.com/e2b-dev/runtime/blob/391b22443460fb30432da889b44aa413478d5717/packages/envd/internal/services/process/handler/multiplex.go) subscribes to live output rather than replaying a durable full log.
- [Signal](https://github.com/e2b-dev/runtime/blob/391b22443460fb30432da889b44aa413478d5717/packages/envd/internal/services/process/signal.go) and the SDK kill helpers specify SIGKILL.
- [Background command documentation](https://docs.e2b.dev/commands/background), inspected 2026-11-18, describes command timeouts and cross-client reconnect. It is explanatory, not a frozen runtime version.

These are a **source-derived target**, not evidence of the version deployed
by E2B Cloud or an upstream runtime tested locally. Record any later baseline
change explicitly; do not follow `main` or `latest` in an acceptance run.

**Correction:** v0.0.2 treats `commands.run(timeout)` as local waiting only.
Its A05/W03 and parts of A14 test that local behavior. The inspected upstream
Start implementation also enforces a remote command deadline, so those passes
do not prove E2B timeout parity. The next profile corrects both foreground and
background `run`; `connect(timeout)` remains local-only. This is a documented
behavior change requiring a new release, not a silent reinterpretation of the
published tests. A dropped connection, failed callback, or explicit disconnect
does not itself cancel a command. Its original remote deadline still applies.

## 2. Supported public surface

Keep the existing `tiny_sandbox.e2b` and `@tiny-sandbox/sdk/e2b` entry points.
The server continues to use execution resources; no independent job scheduler
or second execution store is introduced. The native SDKs keep their current
argv, cancellation and timeout defaults.

| Operation | Python synchronous API | TypeScript Node API | Contract |
|---|---|---|---|
| Start | `commands.run(cmd, background=True, ...)` | `commands.run(cmd, { background: true, ... })` | Return `CommandHandle` after accepted start and confirmed identity, without waiting for exit. A fast exit still returns a handle. |
| Reattach | `commands.connect(pid, timeout=60)` | `commands.connect(pid, { timeoutMs: 60000 })` | Resolve the command in this sandbox and return a new handle. No start, TTL renewal, or command-deadline change. |
| Wait | `handle.wait(on_stdout=..., on_stderr=...)` | `await handle.wait()` | Return `CommandResult` on ordinary exit 0; raise the existing command-exit family on ordinary nonzero exit. No new timeout parameter on `wait`. |
| Disconnect | `handle.disconnect()` | `await handle.disconnect()` | Close only this handle's observation. Idempotent; does not send a remote signal or release the running execution slot. |
| Kill handle | `handle.kill()` | `await handle.kill()` | Address the handle's immutable execution identity and use immediate SIGKILL, not the existing graceful cancel operation. |
| Kill by PID | `commands.kill(pid)` | `await commands.kill(pid)` | Resolve once in this sandbox, then kill that exact execution. Never retry a raw PID against a replacement process. |

`background` omitted/false still returns a result, not a handle. Python exports
`CommandHandle`; TS exports its type and overloads for literal true, literal
false/omitted, and dynamic boolean (a result-or-handle union).

Existing command `envs`, absolute `cwd`, default Bash login shell, image and
non-root user requirements apply. `stdin` omitted/false and `retain_output` /
`retainOutput` omitted/true remain the only accepted values.

Language differences are intentional:

- Python background `run` rejects non-None `on_stdout`/`on_stderr` before any
  start; attach callbacks to `wait`. Python `connect` does not accept callbacks.
  There is one synchronous consumer per handle; cross-thread use and iterator/
  PTY output consumption are outside this first extension. They must fail
  explicitly, not silently consume nothing.
- TS accepts `onStdout`/`onStderr` at background `run` or `connect` and consumes
  output without requiring `wait`. Await callbacks serially. Its handle exposes
  accumulated `stdout`/`stderr` and optional `exitCode`/`error`; an unobserved
  terminal result is not exit 0. Python need not fabricate these live properties.
- Repeated waits on an observed terminal result return the same result values
  or error family without new callbacks or HTTP operations. TS simultaneous
  waits share observation; Python simultaneous consumers are unsupported.
- Python callback exceptions keep their identity; TS callback rejections keep
  their value. They fail that handle, never restart or kill work. A TS handle
  records errors even if `wait` is called later; no unhandled-rejection leak.

No `commands.list`, arbitrary OS-process discovery, interactive stdin, PTY,
AsyncSandbox, port URLs, user switching, custom shell, scheduler, persistent
restart recovery, or unlimited logs are added. The saved PID is enough for the
chosen workflow; listing env-bearing process metadata is separate work.

## 3. Identity and start acceptance

- `handle.pid` is a positive integer **guest process ID** for the command
  leader, obtained from runtime start evidence. It is not an execution string,
  Docker exec ID, host PID, hash, or client-local counter. Validate with a
  command that records its own PID before continuing.
- Keep `(sandbox_id, execution_id)` as the canonical identity. Expose
  `execution_id` / `executionId` as a documented tiny-sandbox extension for
  diagnostics and native recovery; it is not an E2B field.
- The server owns the bounded mapping from `(sandbox ID, guest PID)` to an
  execution. A fresh Python or Node process must resolve it without a local
  cache. Resolve only managed commands, never arbitrary guest processes.
- A PID may be reused. `commands.connect(pid)` / `commands.kill(pid)` select
  the newest managed incarnation in that sandbox; a saved PID alone cannot
  guarantee historical identity. Previously returned handles stay bound to
  their execution IDs and must never target the replacement. Retired mappings
  are removed with retention; never fall back to an older incarnation.
  An execution ID alone does not make `kill(-pid)` safe: the backend must also
  bind signaling to the original runtime process/group lifetime and reject
  stale or unprovable identity. Test the exit/reuse race at signal dispatch,
  not only at PID lookup. A stale guest PID file is not sufficient evidence.
- A known completed execution can still return a handle while retained; wait
  uses its real terminal evidence. If identity has expired, return the
  command-not-found family (`NotFoundException` / `NotFoundError`), not an
  empty successful result. Missing sandbox is the existing sandbox-not-found
  family; authentication failures remain distinct.
- Starting must neither wait for user output nor leak PID framing into stdout.
  Capture identity even for `true`/`exit 37`. A failed PID handshake cannot
  return a successful fake handle; surface available execution ID/key and
  start uncertainty, then keep the command bounded by its original deadline.
- Reuse one idempotency key and exact invocation across accepted-response
  loss. A retry must neither create a second process nor reset its deadline.
  Once retention has forgotten an invocation, no at-most-once promise remains;
  do not silently retry uncertain old work with a new key.

## 4. Three independent clocks

| Clock | Meaning | Disconnect / reconnect effect |
|---|---|---|
| Sandbox lifetime | Existing `Sandbox.create`, `set_timeout` and `Sandbox.connect` rules | `Sandbox.connect` can extend TTL; command-handle operations cannot. Expiry ends all work. |
| Command deadline | `run(timeout)` / `run(timeoutMs)`, default 60 seconds | Enforced remotely after accepted start, even with zero observers. Reconnect never resets it. Zero disables this clock only. |
| Observation deadline | Same `run` connection budget, or new `commands.connect(timeout)` budget | Stops this handle's observation; `connect` cannot kill or extend the command. Zero disables only this observation budget. |

The observation clock begins at entry to run/connect, not at the first wait.
A transport retry uses the remaining budget. The remote deadline is fixed on
initial accepted start; time spent connecting can make local timeout occur
first. A local TimeoutException/TimeoutError therefore is **not confirmation
of remote termination**. An independently reconnected observer must see the
real terminal state, or an explicit uncertain outcome.

Python durations are nonnegative finite seconds, normalized by the pinned
`timeout_to_ms` rule: zero disables; positive values use
`max(1, round(seconds * 1000))`. TS accepts nonnegative safe-integer
milliseconds in this bounded profile. Reject boolean Python durations,
negative/non-finite values and unsupported fractional TS milliseconds before
side effects. In particular, 1.25 seconds is 1250 ms, not 1 or 2 seconds.
Both normalized durations must fit a nonnegative signed 64-bit nanosecond
interval; the maximum milliseconds is 9223372036854.

Do not map millisecond command deadlines to the existing integer-seconds
native field. The required wire extension is specified below. The deadline
applies to foreground as well as background; background is not unlimited by
default. A disconnected `run(timeout=0)` is still bounded by sandbox lifetime.

Per-call `request_timeout`/`requestTimeoutMs` and TS AbortSignal are deferred
on these new operations; reject rather than ignore them. Existing adapter
connection configuration remains available. The start/connect observation
clock governs their command transport; kill uses the configured request budget
(default 30 seconds). These subset exclusions avoid claiming the two upstream
SDKs have identical request-timeout behavior.

## 5. Observation, reconnect and retention

A handle is an observation session, not the process. Terminal observation,
local timeout, callback failure and disconnect close that observation. They do
not free a slot or cancel the execution. A disconnected or failed handle cannot
be revived by `wait`; reconnect creates a new one. An already observed result
remains readable after disconnect.

- A newly started handle observes from execution event 0.
- A **new `commands.connect(pid)` observes from an atomic server attach
  boundary**, not from the beginning of stdout. Its callbacks and returned
  output include only events after that boundary. A command already complete
  at attach returns its exit outcome with empty observation output. This
  matches the source-derived live-subscription profile; it does not promise
  the exact scheduling boundary of E2B's RPC.
- An **automatic transport reconnect within that same handle** resumes from
  its last delivered event ID. Do not replay delivered callbacks or skip
  unconsumed events. A fresh handle has a new boundary, not a persisted cursor.
  Automatic retained replay is a tiny-sandbox reliability extension.
- The attach boundary is the resolve response actually accepted by the client.
  If that response is lost before identity is known, another resolve may
  establish a later boundary; output before it is not promised. Once identity
  and boundary are accepted, retry SSE by execution ID and cursor only: never
  resolve the PID again or advance the boundary to hide a gap.
- A terminal native snapshot carries full retained output, but the adapter
  must not append that full prefix to a newly attached handle's output. It
  uses the snapshot for outcome evidence only. Use native get-execution for
  bounded full-log retrieval and files for durable artifacts.
- Retain the existing 1 MiB combined-output, 4096-event, 200-record and global
  retention rules. A handle does not pin a completed record indefinitely.
  Truncation, including truncation predating attach, produces
  `AdapterError(code=output_truncated)` with available identity and partial
  evidence. No successful complete-log claim is allowed after truncation.
- Ordinary nonzero exit is the command-exit family. Deadline, OOM, eviction,
  unknown termination and unconfirmed cancellation keep diagnostic codes and
  real evidence; never manufacture 137, 0, or an upstream error message.

TS disconnect promptly aborts its stream and stops new callback dispatch even
when the command is silent or a callback is pending. It cannot undo a callback
already entered; it does not wait on a user callback that calls disconnect.
Python disconnect closes the idle/suspended synchronous observer; cross-thread
interrupt of an active wait is not promised. Repeating disconnect is harmless.
No handle cleanup, finalizer, or client exit is allowed to send remote cancel.
Waiting on a disconnected handle without a terminal result raises
`AdapterError(code=disconnected)`. A cached callback failure or timeout is
re-raised on a later wait, rather than replaced by success. A missing retained
execution is command-not-found; premature EOF without terminal evidence is a
stream failure after bounded recovery, never an empty result. Kill remains
available on a disconnected or locally failed handle while its execution is
retained; it uses its original identity, not the observer's state.

## 6. Kill and confirmed outcomes

E2B kill requests SIGKILL. The existing native `/cancel` starts with SIGTERM
and must not be reused unchanged under that name. Add an explicit immediate-
kill operation; preserve graceful native cancellation.

The local safety profile signals the managed **process group** and requires
confirmed termination before reporting true. This is stronger than the pinned
upstream signal-acknowledgement API and is not a claim of identical process-
tree semantics. Descendants that intentionally escape the group are outside
this profile; sandbox deletion is the final containment operation.

- Return true only if this request signaled the selected active execution and
  its termination was confirmed. Return false if it was already completed or
  absent before signaling. A racing natural exit must not become a claimed
  successful signal. Concurrent calls can differ; no exactly-one-true promise.
- A lost acknowledgement, failed signal, nonterminal response, or unconfirmed
  container removal is an error with identity, not false or confirmed success.
  Retry only by immutable execution identity; do not repeat PID resolution.
- Bounded escalation may require container removal if group termination cannot
  be confirmed. Report this explicitly, including sibling impact; never hide
  sandbox destruction behind ordinary command-kill success.
- `wait` after kill keeps a diagnostic canceled outcome and real exit evidence;
  this is a local extension, not a forced E2B `CommandExitError(137)`.

## 7. Minimal server prerequisites

These are **proposed additions**, not current endpoints or schemas. Update
OpenAPI, native SDK models and raw-wire tests together during implementation.

| Addition | Required behavior |
|---|---|
| Authenticated `GET /v1/capabilities` | Return `{"capabilities":["e2b.background.v1"]}` only when PID identity, attach boundary, precise remote deadlines and immediate kill are implemented together. Additional flags are allowed. Authentication follows existing API policy, including keyless loopback mode. This is a narrow feature gate, not general version negotiation. |
| Execution `pid` | Optional additive guest PID; absent means unsupported/unconfirmed, never a fabricated default. Preserve it in get/list/start/complete where known. |
| Start `timeout_ms` | Nonnegative integer, mutually exclusive with `timeout_seconds`; 0 means no command deadline. Part of the idempotency fingerprint. Its deadline uses immediate group kill on expiry; existing seconds-based native graceful timeouts remain unchanged. |
| `GET /v1/sandboxes/{sandbox_id}/commands/{pid}` | Resolve latest retained managed incarnation; return `{execution, last_event_id}` from one consistent snapshot. No process creation, admission slot, resume or TTL renewal. Running sandbox required. |
| `POST /v1/sandboxes/{sandbox_id}/executions/{execution_id}/kill` | Immediate group SIGKILL with bounded confirmation; return `{execution, signaled}` only for a confirmed outcome. Signal/removal uncertainty is an explicit error. Never reinterpret `/cancel`. |

When resolving a completed command, consume its terminal snapshot directly;
do not open an SSE request after its final event and hang forever. For a running
snapshot, SSE starts after `last_event_id`; a concurrent completion is replayed
normally. Identity, snapshot and event watermark must refer to the same
execution and generation. Do not build PID lookup by scanning unbounded client
history or running a separate `ps` execution.

The next adapter checks `e2b.background.v1` before dispatching **any command run
using the corrected timeout semantics**, and before connect/kill. An old server
or missing capability raises `incompatible_server` before a command starts.
Do not send `timeout_ms` to an old server that silently ignores unknown fields.
Do not fall back to shell `&`, `nohup`, parsing stdout PIDs or graceful `/cancel`.

Keep checks scoped to the configured endpoint and credential. A cached
capability cannot protect a connection to a different build behind a load
balancer; mixed-version rolling deployments and downgrade during a session
remain unsupported. No new endpoint calls E2B Cloud.

Default concurrency remains one. A background execution occupies that slot
until remotely terminal; a second start gets `busy`, not a hidden queue.
Connect, wait and kill consume no slot. Do not silently increase concurrency
or broaden the existing adapter's create options. Existing native multi-slot
tests still check sibling safety. Default in-memory mode loses handles and
history on restart. The separate opt-in [recovery mode](operations.md) preserves
completed native records but does not reconnect live background work across
server restart; interrupted commands become `unknown`. This profile promises
fresh-client reconnect while the same daemon owns the command, not E2B
persistence or tenant isolation.

## 8. Delivery gate

Implement the [B01–B20 and W04 scenarios](testing/e2b-background-failures.md)
before claiming support. Keep source-reviewed, locally exercised and
unimplemented status separate. Update A05/W03 for remote start deadlines, A14
for timeout-versus-response-loss, A12 for the newly allowed background option,
and A16 for the real missing-capability rejection. Other assertions stay.

Use installed Python wheels and npm tarballs against private real Docker/HTTP,
without the E2B runtime dependency. Pin SDK references and the local image
digest; retain wire faults, guest PID/side effects, clocks, result objects and
independent owned-container cleanup. Run both SDK languages and a cross-language
W04 handoff. Missing or unimplemented cases cannot count as passed.

The profile was written before production support and is expected to fail
against v0.0.2, including its installed declarations. The current Docker
backend and both adapters implement it; passing a selfcheck alone is not
behavioral evidence. See the [runner instructions](../scripts/e2b-compatibility/README.md#background-command-profile).

Managed commands use the native guest supervisor when `agent_mode` is `guest`.
It runs workloads as the image user and keeps its control socket private for
non-root images. Root images and the legacy Python supervisor allow same-UID
access. Legacy mode requires Linux Python 3 and uses a per-invocation control
socket and token. PID
handshake failure after accepted start retains the execution ID and original
deadline without killing siblings or starting again. An earlier ambiguous
Docker start failure without a process handle can still require whole-container
removal. Neither case may be reported as successful execution.
