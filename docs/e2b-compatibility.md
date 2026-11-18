# E2B compatibility subset and acceptance

Status: Python and TypeScript adapters, local image recipe and installed-package
acceptance implemented in this checkout; publication is a separate step. The subset was decided on
2026-11-16 against tiny-sandbox source revision
`840fc84082a80022bfdb0b84b274f26382834d83`. Only E2B is the migration reference.
The baseline is source-derived; no E2B Cloud account or key is required.
No live E2B comparison or full E2B compatibility is claimed.

## 1. What compatibility means

The first release targets **E2B basic Sandbox application-source compatibility**
for synchronous Python 3.11+ and Node.js 22+ TypeScript. The Python reference
package requires 3.11+; this does not raise the native SDK's minimum version.
An in-scope application changes
its dependency, imports, connection configuration, and documented runtime-image
selection; its command strings, callback code, result access and error branches
remain unchanged. This is not E2B HTTP/envd protocol compatibility: the original
E2B SDK cannot simply point at sandboxd. Do not replace or impersonate the `e2b`
package. The entry points are `tiny_sandbox.e2b` and
`@tiny-sandbox/sdk/e2b`, included in the existing SDK distributions without
an E2B runtime dependency. See the [migration example](../examples/e2b/README.md).

Keep native Go/Python/TypeScript APIs and argv execution unchanged. The optional
compatibility surface uses the same execution, retention and cleanup machinery.
It must not duplicate remote-start recovery or silently weaken native guarantees.

## 2. Pinned reference and evidence

| Reference | Frozen baseline | Inspection performed |
|---|---|---|
| Python SDK | `e2b==2.64.0`, published 2026-11-13 | Downloaded wheel; inspected sync Sandbox, commands, handles, filesystem and errors |
| TypeScript SDK | npm `e2b@2.64.0` | Downloaded published bundle and declarations; inspected the corresponding interfaces and timeout conversion |
| Python wheel SHA-256 | `a5fff266802661796539574878d82dbb8a17823be3013a6aeff1460b4cff1682` | Verified downloaded bytes |
| npm integrity | `sha512-ywiGIEEXE9GsyQazWNORpMif1RMx1zIjjFJec4I5a8+G0HbFBn6ZpSypafwSxzjYl9ZZJ/p+sQMYQUUYnLi0ZA==` | Verified downloaded bytes |

Reference sources, inspected 2026-11-16:

- Release metadata: `https://pypi.org/project/e2b/2.64.0/` and `https://registry.npmjs.org/e2b/2.64.0`.
- Shell contract: `https://docs.e2b.dev/commands.md`.
- Lifetime: `https://docs.e2b.dev/sandbox.md` and `https://docs.e2b.dev/faq/sandbox-lifetime.md`.
- Files: `https://docs.e2b.dev/filesystem/read-write.md`.
- Connect TTL: `https://github.com/e2b-dev/E2B/blob/main/spec/openapi.yml`, `/v2/sandboxes/{sandboxID}/connect`. This is a dated live specification, not an immutable SDK release.
- Published Python source: `sandbox_sync/commands/command.py`, `sandbox_sync/commands/command_handle.py`, `sandbox/commands/command_handle.py`, `sandbox_sync/main.py`, `sandbox_sync/filesystem/filesystem.py`.
- Published TypeScript bundle: `CommandStartOpts`, `CommandResult`, `CommandExitError`, `WriteInfo`, `EntryInfo`, `Commands.start`, `SandboxApi`, and `timeoutToSeconds`.

The acceptance harness pins these SDKs and records its source-derived expectations
in `scripts/e2b-compatibility/reference.json`; it does not use `latest`. Record
the local image digest, guest version, runtime/tool versions and date with each
run. A separately authorized, billable cloud run is an optional drift check, not
a prerequisite for the local gate. Such a run must also record its reference
template: pinning the SDK does not pin E2B's hosted control plane. Any observed
disagreement blocks the affected claim until recorded and resolved. Source
inspection defines expectations; it does not prove our implementation passes.

## 3. First-release allowlist

Names below use Python / TypeScript spelling. Unlisted options are not supported,
even if the native API has a similarly named capability. Validation happens
before creating a sandbox, starting a process or modifying a file.

| Surface | Accepted subset | Required observable behavior |
|---|---|---|
| `Sandbox.create` | Omitted template or `base`; `timeout` / `timeoutMs`; `envs`; adapter connection configuration | Returns a sandbox object with `sandbox_id` / `sandboxId`, `commands` and `files`. Default lifetime 300 seconds, kill on expiry, no implicit idle timeout. |
| `Sandbox.connect` | Static form with an ID and optional `timeout` / `timeoutMs`; running sandboxes only | Returns the same sandbox, including files and create-time env. TTL is extended, never shortened: later of current deadline and now plus requested duration; omitted duration is 300 seconds. It is not a read-only health probe. |
| `sandbox.set_timeout` / `setTimeout` | Positive duration on a running sandbox | Replaces the deadline with now plus duration, including shortening. Does not change an execution's independent native deadline or resurrect a killed sandbox. |
| `sandbox.kill` | Instance form; Python `with Sandbox.create()` also performs cleanup | Boolean result as in the pinned SDK: success for a removed sandbox, false for missing/already removed. A failed or unconfirmed removal is an error, not false or successful cleanup. |
| `sandbox.commands.run` | Command string, `envs`, absolute `cwd`, `on_stdout` / `onStdout`, `on_stderr` / `onStderr`, `timeout` / `timeoutMs`; foreground only | Bash login-shell execution; successful `CommandResult`; nonzero exits raise the compatible command-exit exception with retained output. Omitted or false `background`/`stdin` and omitted or true `retain_output`/`retainOutput` are accepted. |
| `sandbox.files.write` | One path and one in-memory value: Python str/bytes/bytearray; TS string/Uint8Array, including Buffer | Creates parents, replaces existing regular-file contents and returns `WriteInfo`, not void. |
| `sandbox.files.read` | `text` default or `bytes` | Text is a string; byte reads are Python bytearray / TS Uint8Array. Preserve raw bytes; empty files and missing files are distinct. |
| `sandbox.files.list` | One directory, omitted depth or depth 1 | Returns an EntryInfo array, not a cursor envelope. Adapter consumes all native pages within the declared budget or fails explicitly. |

Sandbox durations in this subset must be positive finite values: Python integer
seconds; TypeScript milliseconds converted with `ceil(ms / 1000)`, matching the
published SDK. Zero is not mapped to native unlimited lifetime. Command wait
timeouts are different: nonnegative seconds / milliseconds, default 60 / 60000,
with zero disabling only the local wait deadline. Fractional command seconds
must not be rounded to native integer execution timeouts.

### Runtime and file prerequisites

- `base` means an explicitly configured local compatibility image, not an E2B cloud template ID or a promise of identical preinstalled packages. Freeze its digest with the release. No silent fallback to the current Alpine image.
- The first supported image profile provides Bash, a real unprivileged `user` account, `/home/user`, writable home/tmp and the tools needed by the three acceptance workflows. Record tool/package versions. Running application commands as root is not an acceptable substitute.
- Default command cwd and relative file paths resolve against that user's home, not the client cwd. Command-level `cwd` does not change later commands or the base for file operations. Do not normalize symlink paths in a way that changes filesystem semantics.
- Create-time env belongs to the sandbox and survives a new SDK object's connect. Per-command env overrides it for that invocation only. Do not retain it solely on the creating client. Never log or echo raw env values in execution metadata.
- File ownership, permissions, type and modification time must be read from the sandbox, not fabricated from the configured default user. `WriteInfo` contains name/path/type and optional metadata; `EntryInfo` also includes size/mode/permissions/owner/group/modified_time or modifiedTime, plus a symlink target when applicable. Preserve each language's enum/date shape.
- Cross-provider cases initially use UTF-8 paths without NUL or parent traversal, ordinary text/binary files, directories and listing of symlinks. Symlink read/write, recursive listing, special files and concurrently mutated directories are outside this profile and must not be advertised as tested.
- Adapter configuration uses tiny-sandbox credentials and endpoint settings. It never silently reads E2B cloud credentials or sends requests to E2B. Unknown feature options are rejected rather than forwarded to a server that might ignore them. Supported client/server builds are shipped as a tested pair.

## 4. Semantic rules that must not be approximated

### Shell and result/error behavior

Pass the entire command as the final argument of `/bin/bash -l -c`; do not split
it into argv words, interpolate env into the shell text, or substitute `/bin/sh`.
The native argv endpoint retains its current meaning. Login-shell startup files
and cwd behavior are part of the image fixture and must be tested together.

Success exposes stdout, stderr, exit_code / exitCode and error. Python's
`CommandExitException` and TypeScript's `CommandExitError` expose the same result
fields on a nonzero ordinary exit. Preserve `isinstance` / `instanceof` checks
through classes exported from the compatibility entry point; callers must change
their error imports as well as their Sandbox import. Do not compare exact error
message wording. Missing files and sandboxes use their respective not-found
families; invalid arguments, authentication and waiting timeouts retain the
pinned SDK's public error families. Missing runtime exit evidence is not code 0.

Output callbacks may receive different chunk sizes from E2B. Compare concatenated
bytes/text and per-stream ordering, not callback counts or a total ordering
between stdout and stderr. Await TypeScript callbacks in sequence. Preserve
ordinary callback failures without retrying the command or killing the process.

### Waiting is not remote cancellation

The published Python command implementation applies `timeout` to the process
connection; the TypeScript bundle similarly applies `timeoutMs` to the RPC stream.
The compatibility deadline therefore stops waiting and reports TimeoutException
/ TimeoutError; it does not set native ExecuteRequest.timeout_seconds and does
not invoke native automatic cancellation. A later file side effect must still
occur if the process and sandbox remain healthy. This needs a live reference test.
The same distinction applies when the start response is lost: do not pass the
compatibility wait deadline into a native recovery path that cancels the process.
Recover a known invocation with its original key, without starting another one;
if acceptance remains unknown, report that uncertainty with the available identity.

E2B `stdin` is a boolean keeping the input channel open. tiny-sandbox's one-shot
text input is **not** a compatible implementation. `stdin=True/true` and text in
that field are rejected before execution in this release.

### Limits and uncertainty remain visible

Keep current documented native budgets, including 10 MiB files, 1 MiB combined
retained command output, and bounded directory scans. These are explicit profile
limits, not claims about E2B limits. Output truncation raises an adapter error
with a stable `output_truncated` code and partial evidence; never return a normal
successful result containing only a prefix. This error does not itself kill the
process. Over-limit directory results must not masquerade as a complete list.

Native `oom`, `evicted`, `unknown` and unconfirmed cleanup remain distinguishable
adapter errors carrying the original reason and available execution identity.
Do not invent E2B exception classes for them or relabel all of them as a normal
nonzero command exit. These diagnostic extensions are outside the parity promise.

A create without an idempotency key is not automatically replayed after an
ambiguous response. Execution starts retain native keyed recovery. Keeping a
stream alive or reconnecting it does not renew sandbox lifetime. Native server
restart still loses state; this subset does not provide E2B persistence or VM
isolation guarantees.

## 5. Explicitly deferred

No Code Interpreter/run_code, Python AsyncSandbox, browser/edge runtime claim,
PTY, interactive stdin, background command handles, PID-based connect/kill,
pause/resume/snapshots, port URLs, network policies, volumes, secrets, template
builds, custom cloud template IDs, user switching, custom shell, bulk/streaming
file transfers, metadata filters, or create idempotency in the first release.

Native capabilities such as one-shot stdin, tar transfers and pause remain
available through native APIs; their existence does not establish E2B parity.
`retainOutput: false`, depth greater than 1 and other deferred options must fail
clearly before side effects. A future addition requires a concrete migration
fixture that cannot work with the current allowlist.

## 6. Acceptance cases

The table defines the target contract; the executable assertions are enumerated
in `reference.json`. Run applicable assertions in both languages. `Parity` means
source-derived E2B application semantics exercised against real Docker, not a
mandatory call to E2B Cloud. `Local` cases verify our transport and explicit
limits; they do not establish E2B server behavior. A passing case proves its
recorded assertions, not every broader requirement mentioned in the table.

| ID | Kind | Discriminating input / failure | Required observation |
|---|---|---|---|
| A01 | Parity | Create, then kill; kill again and connect to an absent ID | Opaque ID/property spelling works; real container/guest becomes inaccessible; repeat kill is false; absent connect raises the sandbox-not-found family. |
| A02 | Parity | Bash-only `[[ ... ]]`, a pipeline and a quoted path with spaces | Expected output and exit status; running argv-split text or substituting sh fails this case. Check default user/home and explicit cwd. |
| A03 | Parity | Create env A=base; one command A=override; second command; reconnect from a fresh process; second sandbox | Outputs override, base, base; second sandbox has no A. An env value containing spaces, dollar signs and command substitution stays data. |
| A04 | Parity | Produce distinct stdout/stderr with Chinese and emoji text spanning multiple reads; exit 37 | Both streams match independently; nonzero exception has exit 37 and full output. Success exit 0 uses CommandResult. No duplicated callback delivery. |
| A05 | Parity | Command waits two seconds then writes a marker; client timeout 1.25 seconds / 1250 ms; sandbox lifetime 30 seconds | Wait times out, but the marker later appears and the sandbox remains usable. Also test timeout 0 and ordinary callback failure without remote termination. |
| A06 | Parity | Create for 60 seconds; shorten to 10; separately extend a nearly expired sandbox to 30 | Expiry is measured from each set-timeout call, not creation or the previous deadline. Test TS 1501 ms -> 2 seconds separately. Renewal after kill cannot recreate state. |
| A07 | Parity | Connect from a fresh client; explicit shorter and longer durations; omitted duration | Same ID/files/env; shorter connect duration cannot shorten remaining lifetime, longer duration extends it, omitted duration applies the 300-second connect rule. |
| A08 | Parity | UTF-8 text with final newline; binary bytes 0..255; zero-byte file; relative path versus absolute home path | Exact contents and Python/TS return types; relative paths do not use host cwd. Missing file raises not-found rather than returning empty data. |
| A09 | Parity | Write a nested file, then replace longer content with one byte | Parents exist, old suffix is gone, WriteInfo is populated and mode/owner reflect the guest. Invalid destination must not report successful write. |
| A10 | Parity | Empty directory; mixed file/directory/symlink entries; static directory containing 1007 names | Array and metadata field/type shapes match the reference; no native cursor leaks or silently missing page. Compare sets where upstream ordering is not promised. |
| A11 | Parity | Python context manager exits normally and by exception; TS caller uses finally | Cleanup actually ends the sandbox in both flows; exceptions are not converted to successful results. Record cleanup failure separately. |
| A12 | Local | unsupported stdin/background/user/template/depth/retain-output option and misspelled option | Fail before create/start/file mutation; no ignored option, extra sandbox, process or changed file. Unsupported method calls fail explicitly. |
| A13 | Local | File payload exactly 10 MiB and one byte larger; output exactly 1 MiB and one byte larger; event and scan caps | Boundary behavior matches the declared budgets. Oversized upload preserves the old file; truncated output is an explicit error with evidence, never successful full output. |
| A14 | Local | Drop an accepted start response; drop SSE after delivered output and replay retained events | Side-effect counter increments once; reconnect resumes without missing/duplicate delivered data. Exhausted recovery reports uncertainty and preserves available IDs/key. |
| A15 | Local | Backend removal fails after a delete response; caller deadline expires during cleanup | No confirmed-cleanup success based solely on HTTP 200 or the stored killed state. Backend evidence and orphan cleanup are checked independently. |
| A16 | Local | Wrong credentials; unmatched SDK/server build; server restart; old server ignores new fields | No accidental E2B requests, leaked credentials, unacknowledged control assumptions or promise that vanished state can reconnect. |

The first acceptance profile issues foreground commands serially within each
sandbox. Concurrent application calls are not a parity claim in this release;
native bounded concurrency remains available through native APIs.

For timing cases, record monotonic timestamps, before/after deadlines and
reference jitter; do not assert exact wall-clock completion times. Positive and
negative controls must differ beyond that jitter. Source inspection is not a
substitute for A05-A07 or other server-dependent cases.

## 7. Migration fixtures and release gate

Three executable application workflows define whether this work is useful:

1. **Artifact task:** upload a UTF-8 data file and a binary fixture, run a script, list and download its outputs, inspect a deliberate nonzero failure, clean up.
2. **Multi-turn workspace:** create with env, write files in one process, reconnect in another, run a second task and renew lifetime, then kill.
3. **Long foreground task:** receive stdout/stderr progressively, exercise a wait deadline and a broken connection, prove whether work continued, explicitly clean up the sandbox. This does not claim background-handle compatibility.

For each language, keep one application body and two bootstrap configurations:
official SDK versus adapter. Only imports, credentials/endpoint and fixture image
selection may differ; no provider-specific branches in application behavior or
error handling. Normalize opaque IDs, timestamps, callback segmentation and
unordered directory enumeration, not exit codes, content, error families or
whether processes continue.

Release requires the shipped adapter and pinned image to pass every supported
Parity assertion and Local guard, the three workflows in both languages, native
regression gates, and independently checked cleanup. Record SDK/runtime/image
revisions, commands, outcomes, failure evidence and cleanup results in a fresh
run directory. Missing adapters, images or other required prerequisites are
**not run**, never pass.

`runner.py compare <tiny-report.json>` checks one report against `reference.json`;
it does not need a second cloud report. It requires all 19 cases in both
languages, the current suite digest, every required affirmative check and
confirmed cleanup. The current profile has 147 expectations per language
(294 total): millisecond conversion is TypeScript-only and context cleanup
failure chaining is Python-only. An `any` entry requires at least one listed alternative; it
does not claim unexecuted branches passed. A successful probe comparison alone
is not the release gate: probe identity, untested contract requirements and
native regressions still matter.

## 8. Implemented adapters and delivery

The facades reuse native keyed starts, SSE replay, files and lifecycle updates.
They opt into `cancel_remote=False` / `cancelRemote: false` for command waits;
the native clients retain remote cancellation by default. Relative lifetime
PATCH requests are sent once, because replaying a relative deadline can extend
it again. Truncation stops local waiting with partial output and an execution
ID. A command may still occupy the execution slot after a timeout, callback
failure or truncation; no automatic retry or kill is implied.

| Configuration | Python | TypeScript | Meaning |
|---|---|---|---|
| Endpoint | `endpoint` | `endpoint` | Native SDK defaults: `SANDBOX_DOMAIN`, then localhost:8080 |
| Credential | `api_key` | `apiKey` | tiny-sandbox credential; defaults to `SANDBOX_API_KEY`, never `E2B_API_KEY` |
| Request budget | `request_timeout` | `requestTimeoutMs` | Positive seconds / milliseconds; defaults to 30 / 30000 for lifecycle and each file operation, including its pagination |
| Local base image | `image` | `image` | Create-only digest, or `SANDBOX_E2B_IMAGE`; no tag-only or Alpine fallback |

Command waiting uses its own `timeout` / `timeoutMs`, not the connection request
budget. `get_info(id)` / `getInfo(id)` is a read-only observation helper exposing
only ID, state, local template name and end time; it does not renew lifetime or
promise E2B's complete SandboxInfo schema. Pass connection configuration when
not using environment defaults.

The [image recipe](../examples/e2b/Dockerfile) pins its Debian base, installs
Bash/Python/coreutils and runs as UID 1000 `user`. Its apt repositories are not
snapshot-pinned: rebuilding can change package versions and the final digest.
Build once, select the resulting immutable image ID and record guest versions
with acceptance. No image or SDK is published by the test scripts.

`installed.sh` builds a wheel and npm tarball, installs each into an isolated
consumer, verifies E2B is absent at runtime, checks the installed TypeScript
declarations, and runs A01–A16/W01–W03 plus compare. Import paths are the real
package entry points, without probe overrides. See the
[acceptance instructions](../scripts/e2b-compatibility/README.md).

### Evidence boundaries (2026-11-18)

Earlier disposable probe results demonstrated server prerequisites only. The
formal installed-package gate additionally checks finite-timeout callbacks,
accepted-start deadline uncertainty with retained keys and no cancellation,
SDK callback exception identity, repeated failed cleanup and Python cause
chaining. Unsupported explicit options fail before side effects.

W03 now observes a distinct marker produced only by the timed-out command: it
must be absent before the command and immediately after the local timeout, then
appear later. A counter left by an earlier completed command is not evidence
that timed-out work continued. The comparison gate rejects checks with missing
or non-boolean verdicts instead of treating a check name as proof.

The contract is broader than the current executable assertions. For example,
A16 exercises authentication, restart and additive unknown fields against the
current server; it does not yet test an older server or enforce an SDK/server
build pairing. The named unsupported-option checks do not establish rejection
of every possible unsupported method. Keep these gaps visible in the release
matrix rather than interpreting a 19-case PASS as full contractual coverage.
