# CLI

Status: normative. Contract for `execletctl`. It is a thin layer over the [Go SDK](sdk.md); it must not talk HTTP directly except for `doctor`'s health probe.

## Global behavior

- Global flags `--domain` and `--api-key` go before the subcommand. Nonempty values override `SANDBOX_DOMAIN` and `SANDBOX_API_KEY`, for example `execletctl --domain http://127.0.0.1:8080 list`.
- Execution stdout goes to stdout; execution stderr, progress, and status lines go to stderr. Inspection, lifecycle, list, version, help, and doctor results use stdout. `execletctl run -- cmd > out.txt` captures the command's stdout only.
- Errors are printed to stderr as `execletctl: <message>`, followed by `hint: <next step>` when one is known.
- Ctrl+C or SIGTERM cancels the context; commands that wait on an execution cancel it remotely through the SDK.

## Commands

| Command | Behavior |
|---|---|
| `doctor` | environment and connectivity checks, see below |
| `run [flags] -- CMD [ARG...]` | create a sandbox (or use `--sandbox ID`), start `CMD`, stream output, print the final status |
| `run [flags] -- code 'PYTHON'` | shorthand for `python3 -c 'PYTHON'`; new sandboxes use the [digest-pinned Python 3.12 Alpine image with uv and pip](../internal/guestimage/python.go) unless `--image` is given |
| `shell SANDBOX [TERMINAL]` | open a PTY or reconnect by its printed ID. Ctrl-] detaches and shell exit closes the terminal |
| `mcp --workspace DIR --image IMAGE` | serve eight sandbox tools over standard input and output. [Cursor setup and workflow](../examples/mcp/README.md) |
| `attach SANDBOX EXECUTION` | stream an existing execution from the beginning; Ctrl+C cancels it |
| `logs SANDBOX EXECUTION` | print retained stdout/stderr and the current status without streaming |
| `inspect SANDBOX` | print the sandbox |
| `list` | print sandboxes, one per line: `ID STATE IMAGE CREATED` |
| `pause SANDBOX`, `resume SANDBOX`, `kill SANDBOX` | lifecycle operations; print the resulting sandbox |
| `files list SANDBOX PATH [flags]` | list one directory page as JSON |
| `files archive upload SANDBOX PATH LOCAL_TAR` | validate and extract a tar into an existing directory |
| `files archive download SANDBOX PATH LOCAL_TAR` | download a validated tar with an atomic local save |
| `version` | print the build version (injected with `-ldflags`) |
| `help` | usage |

If your deployment allowlists only `python:3.12-alpine`, pass `--image python:3.12-alpine` to keep using it, or allow the exact new image reference linked above. Changing the client default does not change daemon policy.

`run` flags: `--sandbox ID`, `--image`, `--memory MB`, `--max-concurrent-executions N`, `--pids-limit N`, `--nano-cpus N`, `--network bridge|filtered|none`, `--timeout S`, `--timeout-action kill|pause`, `--idle-timeout S`, `--idle-action kill|pause`, `--exec-timeout S`, `--env NAME=VALUE`, `--cwd DIR`, `--stdin-file FILE`, `--rm` (request cleanup of a sandbox created by this invocation). `--max-concurrent-executions` is a creation-only flag (1–8, default 1). Creation flags together with `--sandbox` are an error. `--rm` does not remove a sandbox supplied through `--sandbox`. Successful cleanup is quiet. If deletion fails or the reply does not confirm the requested sandbox is killed, the CLI prints `cleanup unconfirmed` with the sandbox ID on stderr. Stdout and exit status still report the command outcome. Retry cleanup with `execletctl kill ID`.

`--env` and `--cwd` apply to the execution, not to the sandbox, so they are
valid together with `--sandbox`. Repeat `--env` to set more than one variable;
a value that already contains `=` is split on the first one, and an argument
without `=` is a usage error (exit 2). The environment is additive to the
image's, and it is not stored on the sandbox, so a second `run` does not inherit
it. `--cwd` must be an absolute path; the server rejects anything else. A
directory that does not exist does not fail the invocation outright: the
execution ends with status 126 and `sandbox: cwd is not a usable directory:
<path>` on stderr. Because 126 is not a command exit, the CLI reports it as a
sandbox failure and exits 1, like the other execution-level failures in
[exit codes](#exit-codes); the recorded status is in the `execution:` line.

`--exec-timeout` bounds the command this invocation starts, not the sandbox. It
is forwarded on the execution request, so unlike the creation flags it is valid
together with `--sandbox`, and it is not a creation option.

Numeric creation flags are forwarded when the caller gives them. `--timeout 0`
and `--idle-timeout 0` therefore reach the server as explicit zeros and disable
that timeout; omitting the flag keeps the server default. `--exec-timeout 0`
likewise disables the execution deadline. A negative value is a
usage error (exit 2) for every numeric flag. `--memory` has no "disabled" value:
zero reaches the server and is rejected with an API error, because the server's
minimum is 16 MB.

Resource defaults are 512 MB memory, 1024 tasks (including threads), no CPU
limit, and bridge networking. `--pids-limit` must be at least 64: zero or a
negative value is a usage error, and values 1 through 63 are rejected by the
server. `--nano-cpus 1000000000` limits the sandbox to one CPU; `0` is unlimited.
`--network none` disables external networking. These options only apply when
creating a sandbox.

These are defaults without administrator policy. With `execletd -policy`,
omitted CPU/network use policy defaults, explicit unlimited CPU is refused,
and aggregate budgets and image/network allowlists also apply. See
[deployment policy](operations.md#administrator-policy). The CLI does not add
E2B-style background handles; use the Python/TypeScript adapters for those.

## Status lines (stderr)

These formats are stable; end-to-end tests match them.

```text
sandbox: sbx_123 (created; remove with: execletctl kill sbx_123)
execution: exe_456
execution exe_456: completed (exited), exit 0
```

The first line reads `sandbox: sbx_123` when `--sandbox` was given. The last line is printed whenever the final execution is known, including after Ctrl+C. If streaming fails without a final result, the CLI prints the initial running snapshot instead. Without `--rm`, a sandbox created by `run` is kept for inspection, except when the start itself was interrupted. The CLI then requests cleanup of the sandbox it just created, including when the server could not confirm command cancellation.

## Exit codes

| Situation | Exit code |
|---|---|
| success | 0 |
| command exited with code 1–125 | that code |
| cancellation reported as `ErrCanceled` | 130 |
| invalid usage or flag parsing | 2 |
| anything else (API errors, OOM, timeout, eviction, exit codes 126 and above) | 1 |

Exit codes 126 and 127 are not passed through. In shell convention they mean
"command found but not executable" and "command not found"; both describe
failures of the sandbox environment rather than of the command, and a caller
scripting around `execletctl` would misread them. They collapse to 1 like every
other non-command failure.

## Hints

| Condition | Hint |
|---|---|
| connection refused | `start execletd or set SANDBOX_DOMAIN` |
| 401 | `set a valid SANDBOX_API_KEY or --api-key` |
| 404 | `check the sandbox and execution IDs` |
| `oom` | `increase --memory or reduce the command's memory use` |
| `timeout` | `increase --timeout or --exec-timeout` |
| `evicted` | `the container was stopped outside execletd` |
| `busy` | `wait for the running execution or cancel it` |

## doctor

Prints checks to stdout as `<check>: ok`, `<check>: ok (<detail>)`, or
`<check>: fail (<detail>)`. Remote Docker checks print `docker: skipped (remote domain)`. The summary is `Ready` or `not ready`; failure hints go to stderr.
Exit 0 only if all applicable checks passed.

1. `config`: domain and whether an API key is set (never print the key).
2. `health`: `GET /healthz` within 3 seconds.
3. `auth`: `GET` the fixed probe ID `sbx_doctor_probe`; `404` means authorized, `401` means the key is wrong or missing.
4. `docker`: only for a local domain (loopback or `localhost`); `docker info` reachable.
5. `sandbox`: create a sandbox, run `echo doctor-ok`, check the output, kill the sandbox.

For the [MCP workflows](../examples/mcp/README.md), `doctor mcp` opens `--workspace DIR` (default `.`), checks daemon health, probes `--image IMAGE` in a temporary offline sandbox and destroys it. By default it does not sync project files. Success prints only the client configuration JSON with absolute paths. A failed check exits nonzero with a diagnostic on stderr and no configuration on stdout.

- `--profile pytest` (default): checks `python3 -m pytest --version` in `execlet-mcp:local`. Add `--collect` to sync and collect tests. Collection executes project code; use it only for a project you trust.
- `--profile go`: checks `go version` in `execlet-mcp-go:local`. Add `--check-deps` from a module root to sync and list package and test dependencies for `./...` using the image's local toolchain and module cache. This does not run tests. Rebuild the image after changing `go.mod` or `go.sum`.

## Directory listing

`execletctl files list SANDBOX /work [--limit 100] [--cursor TOKEN]` prints one
JSON page to stdout, including entry metadata and next_cursor when more
entries exist. Flags follow the two positional arguments. Limit is 1–1000.
Use the cursor with the same path; ordering is bytewise and not a snapshot.
Failures use normal CLI error reporting and exit status.

## One-shot input

Use `--stdin-file PATH` to send a local UTF-8 file, or `--stdin-file -` to read
standard input once. Both enforce a 1 MiB limit before creating a sandbox.
```sh
printf hello | execletctl run --rm --stdin-file - -- cat
```

## Tar transfer

`execletctl files archive upload SANDBOX /destination local.tar` uploads an
uncompressed tar (32 MiB encoded limit enforced locally) into an existing container directory. The whole archive is
validated before extraction. `execletctl files archive download SANDBOX /work local.tar`
saves a validated tar containing `work/` and descendants with mode
`0600` after an atomic temporary-file save and rename. Download/save errors
leave an existing destination intact;
the CLI never extracts the tar on the host. Upload accepts only regular local
files and is attempted once. Quiesce destination writers before upload and
inspect remote state after failure; runtime errors can leave partial extraction.

## Concurrent executions

Use `--max-concurrent-executions N` when creating a sandbox (1–8, default 1). Start commands in separate invocations with `--sandbox ID`; canceling one normally leaves siblings running. Pause and resume affect every execution. Container-removal fallback affects siblings. Above one configured slot, unexplained exit 137 is `unknown`, a failure reported by the CLI as exit 1. See the [HTTP policy](api.md#concurrent-execution-policy).

## Implementation references

- [Global flags, output, and exit codes](../cmd/execletctl/main.go).
- [Run flags and cleanup](../cmd/execletctl/run.go).
- [Attach, logs, and lifecycle commands](../cmd/execletctl/ops.go).
- [Doctor checks](../cmd/execletctl/doctor.go).
