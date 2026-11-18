# HTTP API

Status: normative. [OpenAPI specification](../api/openapi.yaml) must describe exactly this contract; when they disagree, fix one of them in the same change.

## Conventions

- Base URL: `SANDBOX_DOMAIN`, default `http://localhost:8080`. All resource paths start with `/v1`.
- Bodies are UTF-8 JSON unless stated otherwise. Unknown request fields are ignored, so a newer client can talk to an older server; see [versioning.md](versioning.md). Malformed JSON and invalid values are still `400 invalid_request`.
- Clients should tolerate unknown response fields and preserve unknown string
  values. A missing additive field on an older server is unknown, not evidence
  that a newer default or capability took effect. See the [Python SDK](sdk-python.md).
- Timestamps are RFC 3339 in UTC. Durations use the unit in their name: integer seconds (`_seconds`) or managed-command milliseconds (`timeout_ms`).
- IDs are opaque strings. Clients must not parse them.
- Authentication: if the server was started with `SANDBOX_API_KEY` set, every `/v1` request and `/metrics` must carry `Authorization: Bearer <key>`, otherwise `401 unauthorized`. `/healthz` and `/readyz` never require authentication. The key is compared in constant time.

## Errors

Errors emitted by the API handlers use this body, except /readyz, whose 503
response uses the readiness object documented below. Router-level errors for
unknown paths or unsupported methods may use plain text:

```json
{"code": "busy", "message": "sandbox sbx_123 has reached its execution limit"}
```

| Status | `code` | When | Retryable |
|---|---|---|---|
| 400 | `invalid_request` | malformed JSON, invalid value, or an invalid `Last-Event-ID` | no |
| 401 | `unauthorized` | missing or wrong API key | no |
| 404 | `not_found` | sandbox, execution, or file does not exist | no |
| 409 | `invalid_state` | operation not allowed in the sandbox's current state (see transition table) | no |
| 409 | `busy` | the sandbox execution slots are full | no |
| 409 | `idempotency_conflict` | idempotency key reused with a different `command`, `cwd`, `env`, or `stdin` | no |
| 413 | `payload_too_large` | JSON request body, file upload, directory scan, or tar transfer exceeds a limit | no |
| 429 | `rate_limited` | a create would exceed the live-sandbox limit; see [limits](#limits) | yes |
| 500 | `internal` | unexpected server error | yes |
| 502 | `backend_error` | the container runtime failed | yes |
| 503 | `unavailable` | server shutting down or not ready | yes |

Clients may honor a `Retry-After` header (seconds) on 429 and 503.

Go and TypeScript SDKs retry the documented repeatable requests on 429, 500,
502, 503 and 504 and honor a positive integer Retry-After on ordinary requests.
Creation is never automatically retried; see the respective SDK contracts.

## Resources

### Sandbox

| Field | Type | Notes |
|---|---|---|
| `id` | string | |
| `state` | string | `pending`, `running`, `paused`, `killed` |
| `image` | string | default `alpine:3.20` |
| `memory_mb` | int | memory limit, default 512, minimum 16 |
| `max_concurrent_executions` | int | immutable execution slot limit, 1–8, default 1 |
| `pids_limit` | int | task limit, default 1024, minimum 64 |
| `nano_cpus` | int | CPU limit in units of 1e-9 CPUs, default `0` (unlimited) |
| `network` | string | `bridge` (default, outbound access) or `none` |
| `timeout_seconds` | int | lifetime timeout, default 300, `0` disables |
| `timeout_action` | string | `kill` (default) or `pause` |
| `idle_timeout_seconds` | int | default `0` (disabled) |
| `idle_timeout_action` | string | `kill` (default) or `pause` |
| `created_at` | string | |
| `expires_at` | string or null | current lifetime deadline; null when disabled, pending, killed, or paused with timeout action `pause` |
| `kill_reason` | string or null | set when `killed`: `requested`, `timeout`, `idle_timeout`, `evicted` |

### Execution

| Field | Type | Notes |
|---|---|---|
| `id` | string | |
| `sandbox_id` | string | |
| `command` | string[] | argv, executed without a shell |
| `cwd` | string or null | the directory the execution was started with; null when the request set none. Never the environment, which may carry credentials |
| `state` | string | `running` or `completed` |
| `exit_code` | int or null | null while running, or if the runtime could not report one |
| `termination_reason` | string or null | null while running; see below |
| `stdout`, `stderr` | string | retained output; omitted in list responses |
| `output_truncated` | bool | true when output exceeded retention or live output was lost during recovery |
| `pid` | int, optional | confirmed positive guest PID for managed commands; absent on native executions or unconfirmed start |
| `started_at` | string | |
| `finished_at` | string or null | |

Termination reasons used by the execution classifier. The first recorded
termination intent wins; otherwise the classifier checks OOM evidence,
container liveness, and finally the exit code:

| Reason | Condition |
|---|---|
| `canceled` | the server terminated it because of a cancel request or sandbox deletion |
| `timeout` | the sandbox lifetime with action `kill`, native `timeout_seconds`, or managed `timeout_ms` expired |
| `oom` | positive cgroup OOM evidence during an execution in a sandbox configured for one slot |
| `evicted` | the container stopped or disappeared, or sibling work was lost to conservative uncertain-start cleanup |
| `exited` | the process exited without a higher-priority reason; unexplained 137 is included only with one configured slot |
| `unknown` | unexplained exit 137 with multiple configured slots, unconfirmed runtime start without a process observer, or interrupted execution after durable restart recovery; a non-success SDK outcome |

`canceled` and `timeout` are intents the server records before it acts; a later intent does not replace the first. The periodic eviction sweep can also finalize an execution directly as `evicted` when the container is gone. Exit code 137 alone never implies `oom`.

### `GET /v1/capabilities`

Authenticated feature discovery: `200 {"capabilities":["execution.env-cwd.v1","files.archive.v1","files.list.v1","execution.stdin.v1","execution.concurrent.v1","e2b.background.v1"]}`.
The background flag is present only for a supporting backend. Missing endpoint
or flag means unsupported, not permission to send fields an old server ignores.
Flags describe the configured endpoint; mixed-version rolling servers are unsupported.
Current native SDKs preflight nonempty env/cwd/stdin, explicit concurrency and
managed `timeout_ms` before create/start. Basic legacy calls need no discovery.
File methods use dedicated endpoints; they do not silently downgrade to shell.

### `GET /metrics`

Uses the same bearer authentication as `/v1`. Returns Prometheus text containing
bounded route/status HTTP latency histograms, create and execution-start latency,
time to first output, cleanup failures/pending removals, live sandboxes and
policy resource reservations. Counters reset on daemon restart. No command,
image, sandbox ID, path, environment or credentials are metric labels.

### `GET /v1/sandboxes/{sandbox_id}/commands/{pid}`

Resolve the newest retained managed incarnation of a positive guest PID.
Returns `200 {execution, last_event_id}` with an atomic outcome/output watermark.
Requires a running sandbox; does not start work, resume it, or renew its TTL.
Unknown PID is `404 not_found`. A fresh observer starts **after** the watermark;
an already completed command uses the returned terminal outcome, not an SSE
request after the last event. The snapshot's retained full stdout is not the
new observer's output. See [background command semantics](e2b-background-commands.md).

### `POST /v1/sandboxes/{sandbox_id}/executions/{execution_id}/kill`

Immediate, lifetime-bound SIGKILL for a managed command, distinct from graceful
`/cancel`. Returns `200 {execution, signaled}` only with confirmed termination.
Already completed returns false; lost acknowledgement or unconfirmed termination
is `502 backend_error`, never fabricated success. Container-removal fallback
reports the collateral termination of siblings explicitly. No automatic retry
of this signal acknowledgement is safe. Handles keep their immutable execution ID.

Managed execution is selected by `timeout_ms` on the existing execution-start
endpoint. It must be an integer from 0 through 9223372036854, exclusive with
`timeout_seconds` and nonempty `stdin`; zero disables only the command deadline.
It is part of the idempotency fingerprint. Deadlines are enforced remotely with
immediate group kill; disconnect does not stop or extend them. Managed results
include additive `pid` in start/get/list/complete when confirmed. Native seconds
deadlines and graceful cancel are unchanged. Linux Python 3 is required in images
used for managed commands; missing runtime support must not silently fall back.

## Sandbox lifecycle

```text
pending ──► running ◄──► paused
   │           │            │
   └───────────┴────────────┴──► killed (terminal)
```

Creation is synchronous: `POST /v1/sandboxes` returns once the container is running, so `pending` is only observable by concurrent readers.

Operation results by current state:

| Operation | `pending` | `running` | `paused` | `killed` |
|---|---|---|---|---|
| pause | 409 `invalid_state` | becomes `paused` | 200, no change | 409 `invalid_state` |
| resume | 409 `invalid_state` | 200, no change | becomes `running` | 409 `invalid_state` |
| delete | becomes `killed` | becomes `killed` | becomes `killed` | 200, no change |
| start execution | 409 `invalid_state` | allowed | resumes, then allowed | 409 `invalid_state` |
| read/write file | 409 `invalid_state` | allowed | resumes, then allowed | 409 `invalid_state` |
| cancel execution | n/a | allowed | resumes, then allowed | returns the stored record |
| get, list | allowed | allowed | allowed | allowed |

Pausing freezes every process in the sandbox, including a running execution, which stays `running` and continues when resumed. Deleting a sandbox with a running execution terminates it with reason `canceled`.

### Lifetime timeout

- With `timeout_action: kill`, the deadline is `created_at + timeout_seconds` and keeps counting while paused. At the deadline the sandbox is killed with `kill_reason: timeout`, and a running execution ends with reason `timeout`.
- With `timeout_action: pause`, the deadline only counts while `running`. At the deadline the sandbox is paused. Every transition to `running` (explicit or automatic resume) sets a fresh deadline `now + timeout_seconds`. Without this, an automatic resume would immediately expire again.
- `expires_at` always shows the current deadline.

### Execution timeout

An execution may carry its own `timeout_seconds` on the start request. It is
independent of the sandbox lifetime policy. A lifetime timeout with action
`kill` ends the sandbox and its command; action `pause` freezes the command
until the sandbox resumes.

- Omitted or `0` means the execution has no deadline of its own; it runs until
  it finishes, is canceled, or the sandbox lifetime ends it.
- The deadline is fixed when the execution starts. It counts wall time,
  including time the sandbox spends paused, so a command cannot extend it by
  staying busy.
- At the deadline the server terminates the command's process group: SIGTERM,
  then SIGKILL after the grace period. The execution ends with
  `termination_reason: timeout` and the sandbox keeps running with its
  filesystem intact.
- If the process tree cannot be confirmed gone, the server removes the
  container as it does for a cancel, and the sandbox ends `killed` with
  `kill_reason: timeout`.
- A pause that begins before the deadline does not stop it. The server applies
  the timeout on the first reaper beat after the sandbox is running again.

The reaper has a nominal one-second interval. Pauses, backend calls, signal
grace periods, and lock contention can delay termination beyond the deadline;
the interval is not a hard upper bound.

### Idle timeout

- A sandbox is idle while it is `running`, has no running execution, and has no open event stream.
- The intended activity model includes authenticated requests addressing the sandbox, execution completion, and stream closure. Currently activity is refreshed by sandbox creation, `GET /v1/sandboxes/{sandbox_id}`, successful execution start, execution completion, and event-stream open/close. Other requests do not consistently refresh it; polling execution or file endpoints does not keep an otherwise idle sandbox alive.
- When idle time reaches `idle_timeout_seconds`, the configured action runs; `kill` sets `kill_reason: idle_timeout`.

### Eviction

If the container stops or disappears without the server asking, the sandbox becomes `killed` with `kill_reason: evicted` and a running execution ends with reason `evicted`. The periodic sweep checks live containers on a nominal one-second interval. Requests that encounter a missing container can detect eviction sooner, but ordinary get/list calls do not probe Docker. Five seconds is the intended detection budget, not a hard bound when backend calls block or sandbox locks remain busy.

Timeout transitions are applied by a periodic reaper configured to tick at most one second apart; backend calls and lock contention can delay observation. Ordinary reads do not apply timeouts. File reads can resume a paused sandbox, and requests may detect external eviction.

### Concurrent execution policy

Create accepts max_concurrent_executions (integer 1–8, default 1); Sandbox views
return it. Pause freezes every execution and resume resumes every execution.
Cancel and execution deadlines normally affect only their selected process group.
If escalation must remove the container, siblings also end as canceled or timeout
respectively, unless an earlier recorded intent applies. Delete cancels every
active execution; external eviction completes every active execution as evicted unless an earlier explicit intent applies.
All active records are protected from retention pruning. Idle starts only after
all executions and streams finish.

When configured above one slot, container-wide OOM counters cannot attribute an
individual process: no execution is labeled oom from those counters. After explicit
intent and container-loss checks, exit 137 is unknown; other exits are exited.
Unknown is a non-success SDK outcome. The default one-slot policy is unchanged.

## Endpoints

### Request correlation

Every handled response includes X-Request-ID. A single incoming value of
1–128 ASCII letters, digits, dots, underscores, or hyphens is preserved; missing,
invalid, duplicate, or longer values are replaced with a UUID. The response ID
correlates JSON access/error records and execution lifecycle records. Logs use
route templates and safe error codes, without commands, environment values,
authorization headers, query values, or raw runtime error messages.

### `GET /readyz`

Public Docker readiness probe, bounded to two seconds. It calls Docker's
unversioned /_ping and requires HTTP 200 with body OK. Success returns 200
with {"status":"ok","version":"<build version>","docker":"ok"}. Failure returns 503
with {"status":"unavailable","version":"<build version>","docker":"unreachable"};
version is the daemon build version. A backend without readiness support is
unavailable. Readiness checks connectivity, not image availability or capacity.

### `GET /healthz`

`200 {"status": "ok", "version": "<build version>"}`. No authentication.

### `POST /v1/sandboxes`

Request (all fields optional):

```json
{"image": "python:3.12-alpine", "memory_mb": 256, "pids_limit": 512, "nano_cpus": 1000000000, "network": "none", "timeout_seconds": 600, "timeout_action": "pause", "idle_timeout_seconds": 60, "idle_timeout_action": "pause"}
```

- `pids_limit` defaults to 1024 and must be at least 64. It bounds tasks, not
  processes: threads count. It is what stops a fork bomb at the container
  boundary instead of at the host's PID table.
- `max_concurrent_executions` defaults to 1 and accepts 1–8. It is immutable;
  see the [concurrency policy](#concurrent-execution-policy).
- `nano_cpus` is a CPU limit in units of 1e-9 CPUs; `0`, the default, means no
  limit. `1000000000` is one CPU.
- `network` is `bridge` (default) or `none`. `bridge` gives the sandbox its own
  network namespace with outbound access, so package installs and HTTP calls
  work. `none` removes networking entirely. Host networking is not offered: it
  would expose every host socket to code the caller does not control.
- `env` is the sandbox's own environment. Every execution sees it, and it
  survives a client reconnecting and creating a new Sandbox object. A
  per-execution `env` entry overrides it for that execution only. The same
  limits as execution `env` apply, and exceeding one is `400 invalid_request`.
  Values are never echoed in a response or written to a log. An execution
  record still retains only an entry count and a fingerprint of the per-command
  environment, so a replay comparison stays meaningful without storing secrets.

`201` with the Sandbox. Not idempotent; clients must not retry it automatically.

### `GET /v1/sandboxes`

`200 {"sandboxes": [Sandbox, ...]}`, newest first. Killed sandboxes stay in the list as records; the server keeps at most 64 of them, oldest dropped first (see [limits](#limits)).

### `GET /v1/sandboxes/{sandbox_id}`

`200` Sandbox.

### `PATCH /v1/sandboxes/{sandbox_id}`

Lifetime is the only mutable property: image, memory, CPU, pids and network
describe the container itself and are fixed at creation. The body sets exactly
one field, and sending both is `400 invalid_request`.

- `{"timeout_seconds": n}` replaces the deadline with now plus `n`, so it may
  shorten or lengthen it. `0` removes the deadline and means "run until killed",
  matching the create-time meaning. A later resume re-arms the new value.
- `{"extend_seconds": n}` keeps the later of the current deadline and now plus
  `n`, so it can only ever extend. This is the rule a reconnecting client uses;
  `0` is a no-op rather than an unbounded lifetime.

`200` Sandbox. A pending or killed sandbox is `409 invalid_state`; a paused one
is updated in place without resuming. Negative values are `400 invalid_request`.
An update never changes an execution's own deadline, and it cannot revive an
expired sandbox.

### `POST /v1/sandboxes/{sandbox_id}/pause`, `POST /v1/sandboxes/{sandbox_id}/resume`

No body. `200` Sandbox. Idempotent per the transition table.

### `DELETE /v1/sandboxes/{sandbox_id}`

`200` Sandbox in state `killed`, once the container is confirmed gone. Repeating deletion is idempotent and returns the stored killed record.

A removal the runtime refuses is `502 backend_error` with `unconfirmed container removal: <container>`: the sandbox is killed and the removal is retried in the background, but removal is unconfirmed, so answering `200` would claim cleanup that has not happened. Repeating the delete continues to report the error until the background retry confirms removal.

### `POST /v1/sandboxes/{sandbox_id}/executions`

```json
{"command": ["python3", "-c", "print(1)"], "idempotency_key": "6f1c..."}
```

- `command` is required and non-empty, with at most 1 MiB of UTF-8 bytes plus
  one separator byte per argument. Larger commands return `413 payload_too_large`.
- `idempotency_key` is optional, at most 128 characters, scoped to the sandbox.
- `timeout_seconds` is optional and bounds this execution. Omitted or `0` means no deadline of its own. The sandbox lifetime deadline still applies, and whichever comes first wins. See [execution timeout](#execution-timeout).
- `env` is optional and sets environment variables for this execution only. It is additive: a name here overrides the container's value, and every other container variable stays visible. A later execution does not inherit it. Limits: 64 entries, 64 bytes per name, 8192 bytes per value, 32768 bytes in total, counting each name, one equals-sign byte, and its value. A name must be non-empty and must not contain `=`, a NUL byte, or a newline; a value must not contain a NUL byte. Exceeding a limit is `400 invalid_request`. Environment fields are never echoed in a response; commands may print their environment. Execution records retain only an environment entry count and a process-keyed HMAC-SHA256 fingerprint, not raw environment names or values.

  The environment is applied through the `env` utility, so accepted names such as `MY-VAR` and `CONFIG.KEY` are not filtered by the wrapper shell. An image must provide `env` at `/usr/bin/env` or `/bin/env`; without one the execution completes with `exit_code: 126` and `sandbox: this image has no env utility, so the requested environment cannot be applied` on stderr rather than silently dropping entries. Both paths exist in the images this project uses (Alpine, Debian, the official Python images) and in `postgres:16`.

  One case is exempt: when `command[0]` contains `=`, the command goes through an extra shell, because `env` would otherwise read that path as another variable assignment and run nothing. A shell rewrites `IFS`, `PWD`, and `SHLVL` on startup, so those three names carry the shell's values instead of the requested ones for that execution. Every other execution passes all names through unchanged.
- `cwd` is optional and sets the directory the command runs in. Omitted or empty uses the image's working directory. It must be absolute and at most 4096 bytes; otherwise `400 invalid_request`. A directory that does not exist or is not a directory does not fail the start: the execution completes with `exit_code: 126` and `sandbox: cwd is not a usable directory: <path>` on stderr, which keeps it distinguishable from a missing command (`127`).
- `201` with the new Execution (`state: running`).
- State validation precedes idempotency lookup: a killed or pending sandbox rejects the start, including a replay; a paused sandbox resumes first.
- Same key and the same invocation as an existing execution: `200` with that execution, in whatever state it is now. No new process starts; a different `timeout_seconds` on the replay does not change its deadline. The comparison covers `command`, `cwd`, `env`, and `stdin`, so a replay cannot silently return an execution that ran with a different environment.
- Same key with a different command, `cwd`, `env`, or `stdin`: `409 idempotency_conflict`.
- Execution slots are full: `409 busy`. The immutable create-time max_concurrent_executions accepts 1–8 and defaults to 1. Identical keyed replays do not consume slots.

#### Unconfirmed runtime startup

The Docker start handshake shares the five-second `BackendCallTimeout` with
exec creation and identity confirmation. After the handshake succeeds, output
observation outlives that budget and the initiating HTTP request. The preceding
OOM baseline read and any cleanup have separate bounded budgets; five seconds
is not an end-to-end HTTP latency guarantee. Docker start is never retried.

If Docker may have accepted user code but no process observer was obtained,
the server returns `502 backend_error` identifying the retained execution.
With or without durable state, the invocation/key remains recorded with
`state: completed`, `termination_reason: unknown`, null exit code and
`output_truncated: true`. The server retires the sandbox and requests container
removal; further starts, including keyed replays, return `409 invalid_state`.
Sibling executions are lost with reason `evicted` unless an earlier cancellation
or timeout intent already won. A `killed` record alone does not prove removal:
cleanup failures remain visible and are retried. Inspect the original outcome
before retrying business work in a different sandbox.

If a managed process observer exists but PID confirmation fails, the existing
observer and remote deadline remain active instead; the key stays retained and
no duplicate is started. See the [background contract](e2b-background-commands.md).

#### One-shot stdin

The ExecuteRequest stdin field sends UTF-8 text once, followed by EOF. At most
1 MiB of decoded bytes (including NULs) is accepted; omitted and empty mean
EOF. Oversized input is 413 payload_too_large. Execution-start JSON bodies are separately
bounded at 8 MiB to accommodate escaping. This is not interactive input.
Set stdin on ExecuteRequest and pass it to the execution start endpoint, then stream the
returned execution. Older servers ignore this field; require a server that
implements the input contract.

A keyed replay must match command, env, cwd and stdin. Input is staged in a
private temporary directory, opened and unlinked before the command runs,
with a bounded best-effort cleanup after startup failure or completion. If the
runtime is unreachable, paused, or cannot start a cleanup process, container
removal is the final cleanup boundary. Execution history retains only an input
fingerprint. List responses never contain input or output. Get and complete-SSE
snapshots only contain input when the command prints it. Runtime administrators can inspect live input; this is not a secret vault.

Non-empty stdin requires writable /tmp and /bin/mkdir, /bin/rm and /bin/rmdir
in the image. Missing staging tools cause backend_error before user execution.

Command metadata remains limited to 1 MiB total UTF-8 bytes, counting one
separator byte per argv element. Larger commands return 413 payload_too_large;
this keeps complete SSE events within the SDK framing limits even when stdin
uses the larger execution-request JSON budget.

### `GET /v1/sandboxes/{sandbox_id}/executions`

`200 {"executions": [Execution without stdout/stderr, ...]}`, newest first.

### `GET /v1/sandboxes/{sandbox_id}/executions/{execution_id}`

`200` Execution including retained `stdout` and `stderr`.

### `POST /v1/sandboxes/{sandbox_id}/executions/{execution_id}/cancel`

No body. Records canceled intent, sends SIGTERM to the process group, and waits up to 2 seconds for completion before SIGKILL. It then waits up to 8 more seconds before requesting container removal and marking the sandbox `killed` with `kill_reason: requested`. These are wait budgets: backend calls add time, and the caller disconnecting its cancel request does not shorten them — the server finishes the escalation on its own so a client timeout cannot turn a slow signal into container removal. After removal, it waits up to 2 more seconds and returns `200` with the latest Execution snapshot, which can still be nonterminal. Canceling an already completed execution returns it unchanged. See [cancellation and removal](#cancellation-and-removal).

### `GET /v1/sandboxes/{sandbox_id}/executions/{execution_id}/events`

Server-sent events; see below.

### `GET /v1/sandboxes/{sandbox_id}/files?path=/abs/path`

`200` with the raw bytes (`application/octet-stream`). `404 not_found` if missing. `path` must be absolute.

### `PUT /v1/sandboxes/{sandbox_id}/files?path=/abs/path`

Raw bytes in the body; creates parent directories; replaces existing files. `204`. Idempotent.

The file and every parent directory this write creates are owned by the container's default user, because the archive is extracted by a root process: without that, an uploaded file in a fresh directory is root-owned and a non-root guest cannot write its output beside it. Existing parents keep their own owner and mode.

<a id="directory-listing"></a>

### `GET /v1/sandboxes/{sandbox_id}/files/list`

Query `path=/work&limit=100&cursor=TOKEN` returns
`entries` with `name`, `size` (regular-file bytes, symlink target-path bytes,
zero for directories), `mode` (POSIX permission bits), `mtime` (UTC
RFC3339), `is_dir`, `type`, `uid` and `gid`, plus `next_cursor` when another
page exists. `type` is `file`, `dir`, `symlink` or `other`; a symlink is never
reported as the file it points at, and device nodes, sockets and FIFOs are
`other` rather than being silently labelled files. `owner`, `group` and
`symlink_target` appear when they are known: Docker's archive carries numeric
ids only, so the names are read from the guest's `/etc/passwd` and `/etc/group`
in one bounded exec, and stay absent when that lookup is unavailable. The
numeric `uid`/`gid` are always present and authoritative. Only immediate
children appear; child symlinks are not followed. Results sort by UTF-8 name
bytes. Limits are 1–1000 (default 100); reuse the opaque cursor with the same
path. Concurrent mutations do not provide snapshot consistency. Empty
directories return entries: []; missing paths are 404 and files are 400.

Paths must be absolute UTF-8, at most 4096 bytes, without NUL or parent
traversal. Repeated/unknown parameters, invalid limits, and malformed or
path-mismatched cursors return 400. Paused sandboxes resume first. Each page
scans Docker's recursive tar stream, capped at 64 MiB, 10000 entries and
30 seconds, retaining at most limit+1 page records and 10000 bounded
name/size/link-target records to resolve hardlink sizes. Large nested files
or trees can therefore return 413 even when immediate children are few;
this API is not a constant-time filesystem index. Runtime deadline failures
are 502. No archive is extracted on the host.

<a id="tar-transfer"></a>

### `GET /v1/sandboxes/{sandbox_id}/files/archive?path=/abs/path`

Returns `application/x-tar`. The archive contains the selected file/directory
basename as its root, so downloading `/work` produces `work/` and its descendants.
Both directions use uncompressed tar, bounded to 32 MiB encoded, 4096 entries
and 16 MiB total regular-file payload. Download validates before sending bytes.
Malformed or unsupported content is intentionally 400 for both directions;
on download this means the selected content cannot be represented by the
supported safe archive subset, not that the Docker runtime failed. A symlink
selected alone is rejected when its target is outside that selected archive;
download a parent directory containing both instead. Limits are 413. Paused sandboxes
resume first; runtime operations have a 30-second deadline.

### `PUT /v1/sandboxes/{sandbox_id}/files/archive?path=/abs/directory`

The body is tar (application/x-tar or application/octet-stream). A 204 means
extraction finished beneath the existing directory. Missing destinations are
404 and nondirectories are 400. Every header and payload is validated before
any filesystem mutation. Invalid later members cannot leave earlier members
written. Names are relative UTF-8, at most 4096 bytes each, with a 1 MiB
aggregate member/link path-text limit. NUL, backslash, absolute paths and parent
traversal components are rejected. Duplicate names, nondirectory
ancestors, device/FIFO/sparse members and extended attributes are rejected.
Only regular files, directories, symlinks and hardlinks are supported.
The supported symlink subset uses relative targets with no parent traversal
components, even when normalization would keep them inside the archive. Targets
must be regular-file or directory members in the archive; hardlinks must refer
to an earlier regular member by its archive-root-relative name. Unsupported
PAX fields are rejected. UID/GID/owner names and special permission bits are
not imported; ordinary permission bits, mtime and bytes are preserved.

Existing destination/ancestor symlinks return `400 invalid_request` before
extraction. Entries cannot create symlink ancestors for later writes. Container path checks
use sh as container root (matching Docker extraction privileges) with positional
arguments in bounded 256 KiB batches. This protects against supplied archive
escapes, but is not hardened isolation: an independently running container
process can change its filesystem between preflight and Docker extraction.
Callers must quiesce filesystem writers during upload. Runtime failure or
cancellation can leave partial extraction; there is no filesystem transaction.
SDKs therefore send archive PUT exactly once, unlike ordinary file PUT.
No uploaded data is extracted on the daemon host. The directory/archive path
and query validation rules are the same as directory listing.

## Event stream

`GET .../events` with `Accept: text/event-stream`.

```text
id: 1
event: stdout
data: {"data":"hello\n"}

id: 2
event: stderr
data: {"data":"warning\n"}

: ping

id: 3
event: complete
data: {"id":"exe_456","state":"completed","exit_code":0,"termination_reason":"exited", ...}

```

- Event IDs are decimal integers starting at 1 and increasing by 1 per event within an execution.
- Types: `stdout` and `stderr` carry `{"data": "<chunk>"}`, at most 32 KiB per chunk; `truncated` carries `{"limit_bytes": <n>}` and is sent at most once; `complete` carries the full Execution and is always the last event.
- Output is text. Chunks are cut on UTF-8 boundaries, so a valid multi-byte character is never split across two events; a client that concatenates the chunks in order reproduces the retained output exactly. Bytes that are not valid UTF-8 cannot be represented in a JSON string: each output stream replaces them with U+FFFD independently, both in the events and in the retained `stdout`/`stderr` field, so a program that writes binary data should encode it (for example with base64) rather than rely on the stream.
- `Last-Event-ID: <n>` replays only events with ID greater than `n`, then continues live. A non-integer value, or a value beyond the newest event of the execution, is `400 invalid_request`: no such event will ever exist, and a running execution would otherwise leave the caller waiting for a replay that cannot arrive.
- The server sends a `: ping` comment every 15 seconds while the stream is open, including while output is flowing.
- The server closes the stream after `complete`. A finished execution replays its events and closes immediately.
- Closing a stream never affects the execution. An open stream counts as activity for the idle timeout.

## Limits

| Limit | Value | Behavior when exceeded |
|---|---|---|
| Retained output per execution (stdout + stderr) | 1 MiB | the last chunk is cut to the budget, backing up to a UTF-8 boundary if necessary; one `truncated` event is sent and `output_truncated: true` is set; the process keeps running |
| Output events per execution | 4096 | same as above |
| Executions retained per sandbox | 200 | eligible completed records are dropped with their output and idempotency keys; all active executions and the most recently completed one are protected. `404 not_found` afterwards |
| Live sandboxes | 64 | `POST /v1/sandboxes` returns `429 rate_limited` and starts no container. A killed sandbox does not count |
| Retained sandbox records | 64 killed sandboxes | the oldest killed record is dropped whole, including its execution history. `404 not_found` afterwards. Live sandboxes are never dropped |
| Retained output, all sandboxes | 64 MiB target | eligible completed records are dropped, oldest sandbox first. All active executions and each sandbox's most recently completed one survive, so protected output can exceed this target |
| JSON request body | 1 MiB (execution starts: 8 MiB) | `413 payload_too_large` |
| Directory listing archive scan | 64 MiB / 10000 headers | `413 payload_too_large` |
| Directory listing runtime | 30 seconds | `502 backend_error` |
| Tar transfer size/count | 32 MiB encoded / 16 MiB payload / 4096 members / 1 MiB total path text | `413 payload_too_large` |
| Tar member/link name | 4096 bytes | `400 invalid_request` |
| Decoded stdin | 1 MiB | `413 payload_too_large` |
| Command metadata | 1 MiB, counting one separator byte per argument | `413 payload_too_large` |
| File upload | 10 MiB | `413 payload_too_large` |
| File download through Docker | 10 MiB | currently `502 backend_error` from archive extraction |

The stored output is exactly the data delivered in events, so a replay from ID 0 reproduces `stdout` and `stderr`. The events are the server's only copy: `stdout` and `stderr` in an execution response and in the `complete` event are assembled from them.

Per-execution output and event limits are enforced as output arrives. Execution-history pruning runs on admission and completion; live-sandbox admission is checked at creation. Killed-record and aggregate-output pruning run on the periodic reaper, nominally once a second, and can lag under load. The aggregate output target cannot override protected active or most recently completed records.

An idempotency key is remembered only while its execution record is retained.
History, aggregate-output, or killed-record pruning can remove it. Reusing a
forgotten key starts a new command; there is no guaranteed time-based or
200-start replay window. Keep results externally when they must outlive retention.

## Implementation limits

### Cancellation and removal

In the normal cancellation path, the handler waits for completion. In the current
container-removal fallback, the handler waits for a bounded period and returns
the latest execution snapshot; backend removal failures or delayed completion
can leave that snapshot nonterminal. Clients must inspect `state` and must not
treat an HTTP response alone as proof that every process has stopped. The SDK
has its own five-second cancellation deadline, so it can return
`ErrCancelUnconfirmed` while server cleanup is still in progress.

Container removal is best effort in `terminateLocked`. A sandbox can be recorded
as `killed` even if Docker removal fails; deletion returns `502 backend_error`,
including repeated deletes while removal remains pending. Once removal is
confirmed, deletion returns the stored record with `200`. A removal that fails or exceeds the server's backend
call timeout is recorded for the periodic reaper, which retries it on later
beats with a bounded budget per pass, so a slow runtime does not starve timeout
and eviction checks. Retries continue for the life of the process, but nothing
guarantees that a runtime that keeps failing removes the container; startup
cleanup with the same owner label is the fallback.

## Server behavior

- Listens on `127.0.0.1:8080` by default (`-addr` flag). Refuses to start on a non-loopback address unless `SANDBOX_API_KEY` is set.
- Each container is labeled `tiny-sandbox.owner=<owner>`, where `<owner>` is the `-owner` flag and defaults to the listen address. Without `-state-dir`, startup removes containers carrying its own label; incomplete cleanup refuses admission.
- An ordinary stop (SIGINT, SIGTERM) ends open event streams, waits for in-flight requests up to 10 seconds, then attempts container removal within a separate ten-second sweep budget. Removal is best effort; runtime failures can leave containers for owner-label cleanup on a later startup.
- State is in memory by default. Pin `-owner` when the listen address can change. Opt-in `-state-dir` journals intents and completed results, adopts eligible idle containers, and removes interrupted work as `unknown` without replay. Graceful durable shutdown preserves idle containers, not live commands. Lost live output is truncated; nonzero lost cursors return 409.
- Opt-in `-policy` enforces aggregate reservations and image/network allowlists before creation and verifies kernel limits. Reservations survive unconfirmed removal. Journal failures close readiness and create/start admission. See [operations](operations.md) for configuration, defaults, storage trust and recovery/network limitations.

## Implementation references

- [HTTP handlers](../internal/daemon/handler.go): routing, authentication, activity updates, and SSE framing.
- [Sandbox store](../internal/daemon/store.go): lifecycle, deadlines, and best-effort removal.
- [Execution engine](../internal/daemon/engine.go): idempotency, cancellation, and file access.
- [Reaper](../internal/daemon/reaper.go): timeout and eviction checks.
- [Server entry point](../cmd/sandboxd/main.go): startup cleanup and HTTP shutdown.
