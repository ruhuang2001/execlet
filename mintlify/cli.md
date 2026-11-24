---
title: "CLI reference"
description: "execletctl: a thin, scriptable layer over the SDK with stable status lines and exit codes."
---

`execletctl` is a thin layer over the [Go SDK](/sdk/go). It does not talk HTTP
directly, with one exception: `doctor`'s health probe, because it has to work
when the SDK does not.

```sh
execletctl [--domain URL] [--api-key KEY] <command> [args]
```

Global flags come **before** the subcommand and override `SANDBOX_DOMAIN` and
`SANDBOX_API_KEY`.

## Streams are separated on purpose

| Stream | Contents |
| --- | --- |
| stdout | command stdout; inspect, list, lifecycle, version, help, doctor results |
| stderr | command stderr, progress, status lines, errors |

That split is what makes `execletctl run -- cmd > out.txt` capture your
program's output and nothing else.

## Commands

| Command | Behavior |
| --- | --- |
| `doctor` | Environment and connectivity checks |
| `run [flags] -- CMD [ARG...]` | Create or reuse a sandbox, run, stream, print the final status |
| `run [flags] -- code 'PYTHON'` | Shorthand for `python3 -c 'PYTHON'`; new sandboxes use `python:3.12-alpine` unless `--image` is given |
| `shell SANDBOX [TERMINAL]` | Open a PTY, or reconnect by its printed ID. Ctrl-] detaches; shell exit closes the terminal |
| `attach SANDBOX EXECUTION` | Stream an existing execution from the beginning; Ctrl+C cancels it |
| `logs SANDBOX EXECUTION` | Print retained output and current status without streaming |
| `inspect SANDBOX` | Print the sandbox |
| `list` | One line per sandbox: `ID STATE IMAGE CREATED` |
| `pause` / `resume` / `kill SANDBOX` | Lifecycle operations; print the resulting sandbox |
| `files list SANDBOX PATH` | One directory page as JSON |
| `files archive upload SANDBOX PATH LOCAL_TAR` | Validate and extract a tar into an existing directory |
| `files archive download SANDBOX PATH LOCAL_TAR` | Download a validated tar, saved atomically |
| `mcp --workspace DIR --image IMAGE` | Serve sandbox tools over stdin/stdout. See [MCP](/guides/mcp) |
| `version`, `help` | Build version (injected via `-ldflags`), usage |

## `run` flags

| Flag | Notes |
| --- | --- |
| `--sandbox ID` | Reuse instead of create. Errors when combined with creation flags |
| `--image` | Image for a newly created sandbox |
| `--memory MB`, `--pids-limit N`, `--nano-cpus N` | Resource limits; `--pids-limit` ≥ 64 |
| `--network bridge\|filtered\|none` | Default `bridge` |
| `--timeout S`, `--timeout-action kill\|pause` | Sandbox lifetime |
| `--idle-timeout S`, `--idle-action kill\|pause` | Idle deadline |
| `--exec-timeout S` | Deadline for **this command** only; valid with `--sandbox` |
| `--env NAME=VALUE` | Repeatable; applies to the execution, not the sandbox |
| `--cwd DIR` | Absolute path; applies to the execution |
| `--stdin-file FILE` | One-shot stdin |
| `--max-concurrent-executions N` | 1–8, creation only, default 1 |
| `--rm` | Request cleanup of a sandbox this invocation created |

<Note>
`--env` and `--cwd` apply to the **execution**, so they compose with
`--sandbox`. The environment is additive to the image's and is not stored on
the sandbox, so a second `run` does not inherit it.

Numeric flags are forwarded only when you give them: `--timeout 0` reaches the
server as an explicit zero and disables that timeout, while omitting the flag
keeps the server default. Negative values are usage errors.
</Note>

## Status lines

These formats are stable and the e2e suite matches them.

```text
sandbox: sbx_123 (created; remove with: execletctl kill sbx_123)
execution: exe_456
execution exe_456: completed (exited), exit 0
```

The first line reads `sandbox: sbx_123` when `--sandbox` was given. The last
line is printed whenever the final result is known, **including after Ctrl+C**.
If streaming fails without a final result, the CLI prints the initial running
snapshot instead.

## Exit codes

| Situation | Code |
| --- | --- |
| Success | 0 |
| Command exited 1–125 | that code |
| Cancellation (`ErrCanceled`) | 130 |
| Usage or flag parse error | 2 |
| Everything else: API errors, OOM, timeout, eviction, exit ≥ 126 | 1 |

126 and 127 are not passed through. In shell convention they mean "not
executable" and "command not found", which describe the sandbox environment
failing, not your command. A script branching on them would misread the
situation, so they collapse to 1.

## `doctor`

Prints `<check>: ok`, `ok (<detail>)`, or `fail (<detail>)`, ending in `Ready`
or `not ready`. Hints go to stderr. Exit 0 only if every applicable check
passed.

1. `config` — domain, and whether a key is set (the key is never printed).
2. `health` — `GET /healthz` within 3 seconds.
3. `auth` — probes a fixed sandbox ID; `404` means authorized, `401` means the key is wrong.
4. `docker` — only for a loopback domain; runs `docker info`.
5. `sandbox` — creates a sandbox, runs `echo doctor-ok`, checks the output, kills it.

Check 5 is the one that matters. The others prove the door is open; this one
proves a container can start.

## `doctor mcp`

For the MCP workflow, `doctor mcp` proves an offline sandbox can run your
toolchain before you wire it into a client:

```sh
execletctl doctor mcp --profile pytest --workspace .
execletctl doctor mcp --profile go --workspace . --check-deps
```

On success it prints **only** client configuration JSON with absolute paths. A
failure prints a diagnostic to stderr and nothing to stdout, so you can pipe the
output straight into a config file.

<Warning>
`--collect` syncs and collects tests, which **executes project code**. Use it
only for a project you trust.
</Warning>
