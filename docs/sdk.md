# Go SDK

Status: normative. Contract for `sandbox-go` (package `sandbox`). The SDK speaks the contract in [api.md](api.md).

For the synchronous Python client, see [sdk-python.md](sdk-python.md), including
its differences in zero-retry configuration and error matching.
Both SDKs reject unconfirmed automatic cancellation and
stop observation on callback errors without canceling remote work.

The [TypeScript SDK](sdk-typescript.md) uses the same HTTP contract with async
iteration, AbortSignal and discriminated outcomes. Its additions do not change
the Go interfaces or outcome/error behavior documented here.

## Installation

```sh
go get github.com/ruhuang2001/tiny-sandbox/sandbox-go@latest
```

`@latest` selects a published module version. For unreleased server features,
use the matching commit instead of `latest`. This guide describes the current
checkout; see [versioning](versioning.md) for compatibility.

## Configuration

```go
type Config struct {
    Domain     string
    APIKey     string
    HTTPClient *http.Client
    MaxRetries int
}

func NewClient(cfg Config) (*Client, error)
```

An empty `Config.Domain` uses `SANDBOX_DOMAIN`, then `http://localhost:8080`. A domain without a scheme gets `http://`. An empty `Config.APIKey` uses `SANDBOX_API_KEY`. Nonempty configuration values take precedence.

A nil `Config.HTTPClient` selects a client without a global timeout so streams can stay open. `Config.MaxRetries == 0` selects three retries after the initial attempt. A positive value sets the retry limit and a negative value disables retries.

## Methods

All methods take a `context.Context` first and return typed results.

```go
CreateSandbox(ctx, CreateSandboxRequest) (*Sandbox, error)
GetSandbox(ctx, sandboxID) (*Sandbox, error)
ListSandboxes(ctx) ([]*Sandbox, error)
PauseSandbox(ctx, sandboxID) (*Sandbox, error)
ResumeSandbox(ctx, sandboxID) (*Sandbox, error)
KillSandbox(ctx, sandboxID) (*Sandbox, error)

StartExecution(ctx, sandboxID, ExecuteRequest) (*Execution, error)
StreamExecution(ctx, sandboxID, executionID, onEvent func(Event) error) (*Execution, error)
GetExecution(ctx, sandboxID, executionID) (*Execution, error)
ListExecutions(ctx, sandboxID) ([]*Execution, error)
CancelExecution(ctx, sandboxID, executionID) (*Execution, error)
Capabilities(ctx) ([]string, error)
RequireCapability(ctx, capability string) error
RequireCapabilities(ctx, capabilities ...string) error
AttachCommand(ctx, sandboxID string, pid int) (*CommandAttachment, error)
KillExecution(ctx, sandboxID, executionID string) (*CommandKill, error)

Exec(ctx, sandboxID, command []string) (*Execution, error)
ExecStream(ctx, sandboxID, command []string, onEvent func(Event) error) (*Execution, error)

ListFiles(ctx, sandboxID, path string, options ListFilesOptions) (*FileList, error)
ReadArchive(ctx, sandboxID, path string) ([]byte, error)
WriteArchive(ctx, sandboxID, path string, data []byte) error
ReadFile(ctx, sandboxID, path string) ([]byte, error)
WriteFile(ctx, sandboxID, path string, data []byte) error
```

Optional request fields use pointers (`*int`, `*int64`, `*string`, or `*Action`)
so that "unset" and an explicit zero value are distinct.

`CreateSandboxRequest` includes these resource controls:

| Field | Type | Default and accepted values |
|---|---|---|
| `MaxConcurrentExecutions` | `*int` | 1; range 1–8 |
| `MemoryMB` | `*int` | 512 MB; minimum 16 |
| `PidsLimit` | `*int` | 1024 tasks, including threads; minimum 64 |
| `NanoCPUs` | `*int64` | `0` means unlimited; `1000000000` is one CPU |
| `Network` | `*string` | `NetworkBridge` (outbound access) or `NetworkNone`; default bridge |

These values are fixed at creation and returned on `Sandbox`. See the
[HTTP contract](api.md#sandbox) for the remaining fields and timeout defaults.

## Execution requests

`ExecuteRequest` also carries the execution's environment, working directory,
and one-shot input:

| Field | Type | Accepted values |
|---|---|---|
| `Env` | `map[string]string` | at most 64 entries; 64-byte names, 8192-byte values, 32768 bytes total including one `=` byte per entry; names must be non-empty and contain no `=`, NUL or newline; values contain no NUL |
| `Cwd` | `string` | empty for the image's working directory, else an absolute path of at most 4096 bytes |
| `Stdin` | `string` | UTF-8 text, at most 1 MiB; empty means EOF |
| `TimeoutMS` | `*int64` | managed remote deadline, 0–9223372036854 milliseconds; exclusive with `TimeoutSeconds` and nonempty stdin; zero disables this command deadline |

The native client checks capabilities before optional env/cwd/stdin, explicit
concurrency and managed deadlines; missing support returns `incompatible_server`
before mutation. Basic legacy calls skip discovery. Concurrent calls to
`RequireCapabilities` reuse one successful advertisement per client. Explicit
`Capabilities` refreshes it using the configured credentials. Failed discovery
is not cached; authentication and transient errors remain distinguishable from
a missing feature. This is not mixed-version load-balancer support.
`AttachCommand` returns an execution and atomic `LastEventID` watermark;
`KillExecution` sends one immediate managed kill and returns the confirmed
`Signaled` result. It never retries a lost signal acknowledgement. Native
seconds-based deadlines and graceful `CancelExecution` are unchanged; the
[background contract](e2b-background-commands.md) defines the managed boundary.

`Env` is additive. A name here overrides the same name from the image, every
other image variable stays visible, and a later execution does not inherit it.
Accepted names reach the process unchanged, including names a shell would
reject such as `MY-VAR`, so the image must provide `env` at `/usr/bin/env` or
`/bin/env`; without one the execution ends with `ExitCode` 126 and a marker on
stderr. Executables whose names contain `=` receive explicit `IFS`, `PWD`,
and `SHLVL` values. The Docker backend supports all accepted environment names
on its BusyBox and coreutils images. Custom minimal images have additional launcher requirements. The server
keeps environment values out of its responses because they may carry credentials.
[Image requirements](api.md#post-v1sandboxessandbox_idexecutions)
`Execution.Cwd` is nullable and repeats the directory only when the request set
one.

A `Cwd` that does not exist, or is not a directory, does not fail the start: the
execution completes with `ExitCode` 126 and
`sandbox: cwd is not a usable directory: <path>` on stderr, which stays
distinguishable from a missing command (`127`). The server rejects a relative
`Cwd` with `400 invalid_request`.

Idempotency covers the whole invocation, not just the command: replaying a key
with a different `Command`, `Cwd`, `Env`, or `Stdin` is `409 idempotency_conflict`.

`ExecuteRequest.TimeoutSeconds` bounds one execution. `Exec` and `ExecStream`
take no timeout argument, so a caller that needs one calls `StartExecution`
directly and then `StreamExecution`. Inspect the returned
`Execution.TerminationReason` for `ReasonTimeout`: `StreamExecution` does not
convert execution outcomes into errors. `Exec` and `ExecStream` do perform that
conversion, so their timeout errors match `errors.Is(err, ErrTimeout)`.
An execution deadline normally leaves the sandbox running, but the server can
remove it if it cannot confirm process termination. A sandbox lifetime timeout
with action `kill` removes the sandbox instead.

`Exec` and `ExecStream` are `StartExecution` followed by `StreamExecution`. When execution completes, they return its Execution together with an error: `nil` only when `termination_reason` is `exited` and `exit_code` is 0.

### One-shot stdin

`ExecuteRequest.Stdin` sends UTF-8 text once, followed by EOF; omitted or empty input
means EOF. The limit is 1 MiB of decoded bytes, including NULs. Execution-start JSON has
a separate 8 MiB limit; command metadata remains limited to 1 MiB. Set input on the
request to `StartExecution`, then stream the returned execution. This is not interactive
input. The server does not return stdin in execution metadata, but a command can print
it to output. See the [input contract](api.md#one-shot-stdin) for image requirements and
cleanup behavior. Older servers can silently ignore `Stdin` on raw HTTP
requests, so the client requires `execution.stdin.v1` before sending input.

### Concurrent executions

Set `CreateSandboxRequest.MaxConcurrentExecutions` when creating a sandbox: the default
is 1 and the accepted range is 1–8. Start each execution separately and stream its ID. A
full sandbox returns `busy`. Cancellation normally affects one execution; pause/resume
applies to all work, and container-removal fallback can terminate siblings. With a
configured limit above one, an unexplained exit 137 is `unknown`: `Exec` returns
`ErrUnknown`. See the [concurrency policy](api.md#concurrent-execution-policy).

## Files

Listing requires `files.list.v1`; archive download and upload require
`files.archive.v1`. Ordinary byte reads and writes do not require discovery.

`ListFiles(ctx, sandboxID, path, ListFilesOptions{Limit: 100})` returns a `FileList`
page. Pass `NextCursor` as the next call's `Cursor`; a zero `Limit` uses the server
default. Entries describe immediate children with name, size, mode, modification time,
and directory status, entry type, and numeric UID/GID. Owner/group names and
symlink targets can be absent. Follow the returned cursor until it is empty or absent. Pages are
name-ordered, not a filesystem snapshot. The default page size is 100, with a maximum of
1000; bounded scans can fail with HTTP 413. See [directory listing](api.md#directory-listing) for cursor and metadata semantics.

`ReadArchive` returns tar `[]byte`; `WriteArchive` uploads tar `[]byte`. Downloads are
rooted at the selected path's basename; uploads extract beneath an existing directory.
Archive downloads retry normally. Uploads make one attempt because extraction can be
partial: stop in-container filesystem writers and inspect remote state after a failed or
ambiguous upload. Only relative link targets without `..` are accepted; hard links must
target an earlier regular member. See the [tar contract](api.md#tar-transfer) for
validation and transfer limits.

## Errors

```go
type Error struct {
    StatusCode     int
    Code           string
    Message        string
    Execution      *Execution
    IdempotencyKey string
    Cause          error
}
func (e *Error) Error() string
func (e *Error) Unwrap() error
func (e *Error) Is(target error) bool
func (e *Error) Retryable() bool

var (
    ErrNonZeroExit
    ErrTimeout
    ErrCanceled
    ErrUnknown
    ErrOOM
    ErrEvicted
    ErrCancelUnconfirmed
    ErrNotFound, ErrUnauthorized, ErrBusy, ErrInvalidState
)
```

`Error.StatusCode` is zero when there is no HTTP response. For an HTTP API error, `Error.Code` contains the server’s code, such as `not_found`, `busy` or `invalid_state`. SDK-generated codes identify what happened locally or to the command.

| `Error.Code` | Meaning |
| --- | --- |
| `nonzero_exit` | An ordinary exit reported a nonzero status or lacked an exit code. |
| `timeout`, `oom`, `evicted`, `unknown` | The execution’s recorded termination reason. A future reason is preserved as its wire string. |
| `canceled` | The command was canceled, the request was canceled before dispatch, or automatic cleanup confirmed remote termination after local cancellation. |
| `cancel_unconfirmed` | Cleanup could not confirm that the execution stopped. During start recovery, `Error.IdempotencyKey` identifies the request for follow-up. |
| `callback`, `transport`, `stream` | An event handler failed, an HTTP exchange failed, or the event stream could not be consumed. |

The sentinels are mutually exclusive. An execution matches the sentinel for its termination reason and nothing else; `ErrNonZeroExit` therefore means the command exited on its own with a non-zero status, never an OOM kill, timeout, cancellation, or eviction that also happened to report a non-zero code. `ErrTimeout` does not distinguish the two deadlines that produce it: an execution timeout normally leaves the sandbox running, while a lifetime timeout with action `kill` removes it, so inspect the `Sandbox` when that difference matters.

`errors.Is(err, sandbox.ErrOOM)` must work on every error the SDK returns for that outcome. When a final execution is available, `Error.Execution` holds its retained `stdout`, `stderr`, and `exit_code`. Check `output_truncated` before treating output as complete. Transport or start failures may return no execution; an unconfirmed cancellation may carry only IDs.

## Retries

- Retry candidates: transport errors before a complete response, truncated response bodies, and status 429, 500, 502, 503, 504.
- Only requests that are safe to repeat are retried: `GET`, `DELETE`, `PUT` (ordinary files, excluding archives), pause, resume, cancel, and execution starts carrying an idempotency key. `POST /v1/sandboxes` is attempted exactly once.
- Backoff cap: `min(2s, 200ms * 2^attempt)`. The delay is sampled uniformly from half the cap to the cap. For ordinary requests, a positive integer `Retry-After` value in seconds overrides the delay; zero, HTTP-date values, and invalid values use the calculated backoff.
- Retries stop as soon as the context is done.

## Starting an execution

1. If `ExecuteRequest.IdempotencyKey` is empty, generate 128 random bits, hex-encoded.
2. If the context is already done, return its error without sending anything.
3. Send the start request, retrying as above with the same key.
4. If the context ends while the start is in flight, the SDK cannot know whether the server accepted it. Using a fresh context with a 5-second deadline, it repeats the start with the same key (the server returns the existing execution, or creates one if the original request was never accepted), validates the returned sandbox identity, then cancels that execution. Only a matching, confirmed terminal response yields `ErrCanceled` with the Execution. A failed or unconfirmed recovery returns `ErrCancelUnconfirmed` with `IdempotencyKey` and the original identity when known. It never switches keys.

Reusing a retained key with the same command, environment, directory, and stdin
returns the original execution.
Changing `TimeoutSeconds` on that replay does not update its deadline; use a
new key to start a new execution with a different timeout.

## Streaming

- `StreamExecution` connects to the event endpoint and calls `onEvent` for `stdout`, `stderr`, `truncated`, and `complete`, in order, each exactly once.
- On disconnect it reconnects with `Last-Event-ID` set to the last delivered ID and drops any event with an ID not greater than it.
- Reconnect attempts use the retry backoff. The current implementation allows up to `MaxRetries` reconnects during one `StreamExecution` call; receiving output does not reset this budget.
- It returns the Execution from the `complete` event. `StreamExecution` does not turn a nonzero exit or termination reason into an outcome error; `Exec` and `ExecStream` do that.
- If `onEvent` returns any error, including a typed or wrapped `*sandbox.Error`, the SDK closes the stream and returns `Error{Code: "callback"}` wrapping it. `errors.Is` can still match the original error. It never reconnects or cancels remote work, even when the callback also ends the local context. Call `CancelExecution` explicitly when the command itself should stop.
- If the caller's context ends without a callback failure, the SDK sends one cancel under a fresh 5-second deadline. As in start recovery, a matching `running` acknowledgement triggers only `GetExecution` polling within the remaining budget, never another cancel or kill. Both IDs must match on every snapshot. Only `completed` with a recognized reason other than `unknown` (`exited` also requires an exit code) yields `ErrCanceled`; otherwise the result is `ErrCancelUnconfirmed` carrying the requested IDs.
- Ending a stream for any other reason never cancels the execution.

Automatic cleanup reports local cancellation even when the command won the
race and already exited; inspect the retained Execution for its actual reason.
The raw `CancelExecution` method still returns the server's latest snapshot,
which may be nonterminal. `ExecStream` preserves uncertainty rather than
relabeling it as confirmed cancellation.

### Current streaming limitations

These are implementation limits, not additional guarantees:

- Reconnects count against one budget per call, even when events arrive between
  disconnections. The counter does not reset on progress.
- SSE reconnects do not apply the response's `Retry-After` header.
- Replay covers retained output only. A `truncated` event means further output
  was discarded; reconnecting cannot recover it.
- Output is treated as UTF-8 text. Bytes that are not valid UTF-8 arrive as
  U+FFFD, because output events carry JSON strings.
- One SSE line is capped at 16 MiB. Command metadata and retained output are
  each bounded at 1 MiB; stdin is excluded from execution snapshots. The frame
  budget accommodates JSON escaping. A larger line ends with a `stream` error.

## Dependencies

Standard library only. Minimum Go version is the `go` directive in `go.mod`.

## Implementation references

- [Client configuration](../sandbox-go/client.go) and [HTTP retries](../sandbox-go/retry.go).
- [Execution start and recovery](../sandbox-go/exec.go).
- [SSE reconnects and context cancellation](../sandbox-go/stream.go).
- [Error matching and outcomes](../sandbox-go/errors.go).
