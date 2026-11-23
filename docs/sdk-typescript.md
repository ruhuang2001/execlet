# TypeScript SDK

Status: normative for the ESM package in [sandbox-ts](../sandbox-ts). It speaks
the [HTTP contract](api.md).
There are no runtime dependencies or Node built-in imports. Node 22+ and modern
browsers must supply fetch, ReadableStream, TextDecoder, AbortController and
Web Crypto. Declarations support TypeScript 5.9+ with DOM types.

## Install from this repository

Build a local package from the repository,
then install its actual local path in your application:

~~~sh
cd sandbox-ts
npm ci
npm run check
npm pack
~~~

From your application directory, run

~~~sh
npm install /path/to/tiny-sandbox/sandbox-ts/tiny-sandbox-sdk-0.0.2.tgz
~~~

~~~ts
import { Client, SandboxError } from '@tiny-sandbox/sdk';

const client = new Client({ domain: 'http://localhost:8080' });
const sandbox = await client.createSandbox({ timeout_seconds: 300 });
try {
  const result = await client.exec(sandbox.id, ['sh', '-c', 'echo hello; exit 42']);
  switch (result.reason) {
    case 'exited': console.log(result.execution.exit_code, result.execution.stdout); break;
    case 'timeout': case 'canceled': case 'oom': case 'evicted':
      console.log(result.reason, result.execution.stdout); break;
    case 'unknown': console.log(result.terminationReason); break;
    default: { const exhaustive: never = result; throw exhaustive; }
  }
} catch (error) {
  if (error instanceof SandboxError) console.error(error.code, error.execution);
  else throw error;
} finally {
  await client.killSandbox(sandbox.id);
}
~~~

## Configuration and methods

The constructor accepts an optional ClientConfig:

| Option | Meaning |
| --- | --- |
| domain | Nonempty option, then Node's SANDBOX_DOMAIN, then http://localhost:8080. Missing scheme becomes http://. Base paths are preserved; credentials, query, fragment and non-HTTP(S) schemes are rejected. |
| apiKey | Nonempty option, then Node's SANDBOX_API_KEY; sent as a bearer header, including streams. Environment access is optional and requires no Node imports. |
| maxRetries | Omitted or zero means three; negative disables retries, matching Go. Positive integers count additional attempts. Noninteger/nonfinite values are rejected. |
| fetch | Fetch-compatible injectable transport; defaults to global fetch. Must honor AbortSignal and streaming bodies. |

No global timeout is imposed. Redirects are rejected instead of changing
request semantics. Methods use camelCase, request/response fields use the HTTP
snake_case names. Dates remain RFC3339 strings, binary files are Uint8Array.
Optional fields use undefined; explicit zero is transmitted unchanged.
`RequestOptions` contains an optional `signal`; options are the final argument.
`ListFilesOptions` also accepts `limit` and `cursor`.

~~~ts
createSandbox(request?: CreateSandboxRequest, options?: RequestOptions): Promise<Sandbox>
getSandbox(sandboxID, options?): Promise<Sandbox>
updateSandbox(sandboxID, request: UpdateSandboxRequest, options?): Promise<Sandbox>
listSandboxes(options?): Promise<Sandbox[]>
pauseSandbox(sandboxID, options?): Promise<Sandbox>
resumeSandbox(sandboxID, options?): Promise<Sandbox>
killSandbox(sandboxID, options?): Promise<Sandbox>

startExecution(sandboxID, request: ExecuteRequest, options?): Promise<Execution>
getExecution(sandboxID, executionID, options?): Promise<Execution>
listExecutions(sandboxID, options?): Promise<Execution[]>
cancelExecution(sandboxID, executionID, options?): Promise<Execution>

streamExecution(sandboxID, executionID, options?): AsyncIterableIterator<Event>
streamExecution(sandboxID, executionID, onEvent: EventHandler, options?): Promise<Execution>
exec(sandboxID, command: readonly string[], options?): Promise<ExecutionOutcome>
execStream(sandboxID, command: readonly string[], onEvent: EventHandler, options?): Promise<ExecutionOutcome>

listFiles(sandboxID, path, options?: ListFilesOptions): Promise<FileList>
readArchive(sandboxID, path, options?): Promise<Uint8Array>
writeArchive(sandboxID, path, bytes: Uint8Array, options?): Promise<void>
readFile(sandboxID, path, options?): Promise<Uint8Array>
writeFile(sandboxID, path, bytes: Uint8Array, options?): Promise<FileEntry | undefined>
~~~

EventHandler returns void or Promise<void>; callbacks are awaited in order.
Execution stdout/stderr are optional because lists omit them. All request and
response interfaces, Event, ExecutionOutcome, SandboxError and executionOutcome
are exported from the package root. Internal deep imports are not supported.
The explicit `@tiny-sandbox/sdk/e2b` subpath provides the optional
[E2B application-source subset](e2b-compatibility.md) for Node, without an E2B
runtime dependency. It does not change native argv or browser behavior.

`CreateSandboxRequest` accepts optional `image`, `memory_mb`,
`max_concurrent_executions`, `pids_limit`, `nano_cpus`, `network`,
`timeout_seconds`, `timeout_action`, `idle_timeout_seconds`,
`idle_timeout_action`, and `env`. Create-time env persists with the sandbox;
execution env overrides it for that invocation. Neither is echoed in metadata.
`UpdateSandboxRequest` accepts exactly one of `timeout_seconds` (replace) and
`extend_seconds` (extend without shortening). `updateSandbox` sends one PATCH,
without replaying a relative deadline after uncertainty.
See the [resource contract](api.md#sandbox) for defaults
and accepted values. `ExecuteRequest` requires `command` and optionally takes
`idempotency_key`, `timeout_seconds`, `env`, `cwd`, and `stdin`. Omitted or zero
execution timeout means no separate execution deadline; sandbox lifetime
policies still apply.

`env` adds variables to the image's environment for this execution only;
`cwd` is an absolute working directory.
The [shared SDK contract](sdk.md) lists their limits and behavior, including
exit code 126 for a cwd that is not a usable directory. Idempotency compares
command, env, cwd and stdin, and the server never echoes env. Execution.cwd repeats
the requested directory and is null when the request set none. Older servers
can silently ignore env/cwd on raw HTTP requests, so the client requires
`execution.env-cwd.v1` before sending them. exec and execStream take
only a command, as in Go. To set env or cwd, put them on the request to
startExecution, then stream the returned execution:

~~~ts
const started = await client.startExecution(sandbox.id, {
  command: ['sh', '-c', 'echo "$GREETING from $(pwd)"'], env: { GREETING: 'hello' }, cwd: '/tmp',
});
const done = await client.streamExecution(sandbox.id, started.id, () => {});
~~~

### One-shot stdin

`ExecuteRequest.stdin` sends UTF-8 text once, followed by EOF; omitted or empty input
means EOF. The limit is 1 MiB of decoded bytes, including NULs. Execution-start JSON has
a separate 8 MiB limit; command metadata remains limited to 1 MiB. Set input on the
request to `startExecution`, then stream the returned execution. This is not interactive
input. The server does not return stdin in execution metadata, but a command can print
it to output. See the [input contract](api.md#one-shot-stdin) for image requirements and
cleanup behavior. Use compatible client and server revisions; older servers can ignore
unsupported request fields.

### Concurrent executions

Set `CreateSandboxRequest.max_concurrent_executions` when creating a sandbox: the
default is 1 and the accepted range is 1–8. Start each execution separately and stream
its ID. A full sandbox returns `busy`. Cancellation normally affects one execution;
pause/resume applies to all work, and container-removal fallback can terminate siblings.
With a configured limit above one, an unexplained exit 137 is `unknown`: `exec` returns
`reason: "unknown"` and `terminationReason: "unknown"`. See the [concurrency policy](api.md#concurrent-execution-policy).

## Files

Listing requires `files.list.v1`; both archive methods require `files.archive.v1`.
Ordinary `readFile` and `writeFile` skip capability discovery.

`client.listFiles(sandboxID, path, { limit: 100, cursor, signal })` returns a `FileList`
page with `entries` and optional `next_cursor`. Pass `next_cursor` as the next call’s
`cursor`. Entries describe immediate children with name, size, mode, modification time,
and directory status. `type`, `uid` and `gid` are required; `owner`, `group` and
`symlink_target` may be absent. Malformed known fields reject with `decode`;
future fields and type strings are retained. Follow the returned cursor until it is empty or absent. Pages are
name-ordered, not a filesystem snapshot. The default page size is 100, with a maximum of
1000; bounded scans can fail with HTTP 413. See [directory listing](api.md#directory-listing) for cursor and metadata semantics.

`readArchive(sandboxID, path, { signal })` returns tar `Uint8Array` bytes;
`writeArchive(sandboxID, destination, bytes, { signal })` uploads them. Downloads are
rooted at the selected path's basename; uploads extract beneath an existing directory.
Archive downloads retry normally. Uploads make one attempt because extraction can be
partial: stop in-container filesystem writers and inspect remote state after a failed or
ambiguous upload. Only relative link targets without `..` are accepted; hard links must
target an earlier regular member. See the [tar contract](api.md#tar-transfer) for
validation and transfer limits.

## Outcomes and errors

Completed commands resolve values, including nonzero exits. ExecutionOutcome
is a discriminated union with reason exited, canceled, timeout, oom, evicted or
unknown, plus execution. The unknown variant carries the original
terminationReason. Only exited with exit_code zero is command success.
executionOutcome(snapshot) converts a terminal snapshot, rejecting nonterminal
ones with invalid_state. An exhaustive strict switch is shown above.

SandboxError covers API, transport, protocol, callback and cancellation failures.
Inspect code, statusCode (zero for client failures), cause, execution (a full
snapshot or just IDs), idempotencyKey and the retryable boolean getter. Retryable
classifies a failure; it does not authorize retrying unsafe operations like
creation. API codes, including future ones, are preserved. Plain-text HTTP
errors use http_error.

Client codes: invalid_config, invalid_request, decode (invalid JSON), transport,
stream (invalid SSE/protocol), callback, canceled, cancel_unconfirmed and
invalid_state. Protocol/callback failures never retry. Completed timeout/nonzero
outcomes do not throw; an aborted caller signal does, even after cleanup succeeds.

## Streaming and retries

The SDK uses **fetch plus one SSE parser everywhere**. EventSource cannot send
Authorization or enforce the SDK retry budget/backoff. An unauthenticated
connection uses the same implementation and guarantees.

- On reconnect, Last-Event-ID is the last delivered ID. Events at or below it
  are suppressed, so delivery within a call is ordered and duplicate-free.
- The cumulative maxRetries budget never resets on output. EOF before complete
  is retryable; complete ends iteration without reconnecting.
- Output events have id, type stdout/stderr and data. A truncated event has
  limit_bytes. A complete event has execution. Unknown event types become type
  unknown, with original event_type and raw data; their IDs advance the cursor.
- Parsing handles comments, multiline data, CR/LF/CRLF, fragmented UTF-8 and a
  final unterminated frame. IDs must be positive safe integers (the bounded
  server event count fits). Lines/frames are capped at 16 Mi UTF-16 code units;
  the valid worst-case complete event fits. Incoming data is scanned once.
- Breaking for-await or calling iterator.return closes the connection, even
  with next pending. Neither cancels the execution. Abandoning an iterator
  without closing it is not a cleanup mechanism.
- Callback failures always become callback with the original cause, even when
  application code throws SandboxError. They close the stream without retry or
  remote cancellation. Independently aborting the caller signal still cancels.
- Retention is a server limit. Truncated events/output_truncated mean output was
  discarded and cannot be recovered through replay. Backpressure does not
  increase retention.

Repeatable ordinary requests retry transport failures (including interrupted
response bodies) and HTTP 429/500/502/503/504. Repeatable operations are GET,
DELETE, ordinary file PUT (archive PUT excluded), pause/resume/cancel and keyed starts. Creation is attempted
once, as are relative lifetime PATCH requests. A complete malformed JSON body
is a decode error, not a transport retry.

Delay is sampled uniformly between half and all of the cap
min(2000ms, 200ms * 2^attempt). Positive integer Retry-After seconds override
ordinary delays; zero, dates, fractions and invalid values use backoff.
Like Go, SSE reconnects do not apply Retry-After. Waits and in-flight fetches
stop when their signal aborts.

## Cancellation and start recovery

startExecution copies the request, including its command array and env
record, and, absent a supplied key, generates 128 random bits as a 32-digit
hex idempotency key. Retries reuse the key and the complete invocation, including stdin.
A signal already aborted before starting sends no request.

`startExecution` and both `streamExecution` forms accept `ExecutionOptions`,
which adds `cancelRemote` to `RequestOptions`. Its default is true. Explicit
false stops local I/O on abort and reports `wait_timeout` with the start key or
execution identity, without canceling the remote process. Pass false to both
start and stream when composing them. The E2B facade uses this opt-in; native
`exec` and `execStream` keep their existing cancellation behavior.

An aborted in-flight start triggers a fresh five-second deadline: repeat the
start with the same key, verify the sandbox and execution ID, then cancel it. Unconfirmed
recovery returns cancel_unconfirmed with idempotencyKey and any known snapshot
or IDs, allowing later reconciliation. Keys expire with server retention; do
not replay an ancient key as if it were permanent.

Aborting an active stream starts cancellation with a fresh five-second deadline,
even while an iterator is suspended after an event or an async callback waits.
The next consumer observation reports cleanup. After one cancel, a matching
`running` acknowledgement triggers only `getExecution` polling under the same
deadline, never another cancel or kill. The sandbox and execution IDs must match
on every snapshot. Only `completed` with a known terminal reason other than
`unknown` (and an integer exit code for `exited`) confirms cancellation.
The attached execution may have exited naturally before cleanup; inspect its
actual reason. HTTP 200 alone never confirms termination.

cancelExecution itself returns the raw snapshot, which may still be running
during removal fallback; it does not use the recovery wrapper. Aborting ordinary
get/list/file requests only aborts those requests.

## Compatibility and Go parity

Use client and server builds from compatible revisions. `getCapabilities()`
queries authenticated flags. Optional env/cwd/stdin, explicit concurrency and
managed `timeout_ms` are checked before mutation; missing support raises
`incompatible_server`. Flags are cached per client with its immutable endpoint
and credentials. Basic legacy calls skip discovery. Downgrade during a session
and mixed-version load balancers remain unsupported. Future wire termination
reasons map to the explicit `unknown` outcome so switches remain exhaustive.

Native `resolveCommand` returns the execution and atomic event watermark;
`killExecution` sends one immediate managed kill without retrying uncertain
acknowledgements. `@tiny-sandbox/sdk/e2b` exports `CommandHandle` for background
run/connect/wait/disconnect/kill; see the [package examples](../sandbox-ts/README.md)
and [contract](e2b-background-commands.md). Native graceful cancellation and
seconds-based deadlines are unchanged.

Unknown response fields are retained and response enum strings remain open.
Additive resource fields can be absent; the client does not substitute defaults
as evidence that unsupported controls were enforced.

| Guarantee | Go and TypeScript |
| --- | --- |
| Auth, lifecycle, files, keyed starts | Same HTTP behavior |
| Replay cursor, de-duplication, total budget, jitter | Same guarantees; TypeScript uses fetch for Node and browsers |
| Callback failures | TypeScript always wraps and never retries; Go has a documented typed-error caveat |
| Completed outcomes | Go Exec returns outcome errors; TypeScript resolves a discriminated union |
| Abort/context cleanup | Both use fresh five-second recovery; TypeScript also requires a terminal snapshot |
| Stream consumption | Go callback; TypeScript async iterator plus async callback overload |

Browser-safe packaging does not enable cross-origin access on sandboxd. Serve
through the same origin or use a trusted CORS/preflight proxy allowing
Authorization, Content-Type, Last-Event-ID and the used methods. Expose
Retry-After for browser retries. Never ship privileged bearer keys to untrusted
browser users. The SDK does not change daemon CORS behavior.

## End-to-end verification

~~~sh
bash scripts/e2e-typescript.sh artifacts/typescript-e2e
~~~

The entry gate runs one long-lived numbered-output command in real Docker
behind an authenticated proxy. After its first output event the proxy drops
the stream, then inserts that event into the reconnect. The SDK sends
Authorization and Last-Event-ID, delivers an exact duplicate-free four-line
prefix, then aborts the caller signal. Both recovery and a fresh GET must show
a canceled terminal snapshot retaining every delivered byte. Additional abort
scenarios cover ambiguous starts and nonterminal cancel responses.

The runner records requests, IDs, event content/hashes, results, build/daemon
logs and scoped container cleanup. The [failure matrix](../sandbox-ts/e2e/FAILURE-MATRIX.md)
precedes implementation. Faults wrap real server responses; there is no fake-only
unit suite. Package validation installs the tarball, compiles a strict consumer
and exercises a browser bundle without Node globals against the real daemon.

### Terminal, metadata and purge operations

`statFile(sandboxID, path)` checks `files.stat.v1` and returns the observed entry. A legacy 204 write returns `undefined`.
`openTerminal(sandboxID, {rows, cols, command, cwd, signal})` returns the terminal ID. Use `terminalInput`, `resizeTerminal`, `streamTerminal` and `closeTerminal` with that ID. `streamTerminal` yields Uint8Array chunks and supports an initial byte offset. Terminal methods check `terminals.v1`. `destroySandbox(sandboxID)` checks `sandbox.purge.v1` and removes retained records.
