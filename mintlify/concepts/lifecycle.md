---
title: "Sandbox lifecycle"
description: "States, timeouts, and what pause actually preserves."
---

A sandbox is a Docker container plus a lifecycle record. The states are small
on purpose:

```text
pending ──► running ◄──► paused
               │            │
               └────┬───────┘
                    ▼
                 killed
```

`killed` is terminal. Nothing comes back from it; you create a new sandbox.

## Timeouts

Two independent clocks, each with an independently chosen action.

| Timeout | Default | Counts | Disabled by |
| --- | --- | --- | --- |
| Lifetime (`timeout_seconds`) | 300 | from creation | `0` |
| Idle (`idle_timeout_seconds`) | 0 (off) | while no execution runs and no event stream is open | leaving it at `0` |

Each selects `kill` or `pause` through `timeout_action` and `idle_timeout_action`.
The default is `kill` for both, so a forgotten sandbox costs you nothing.

<Warning>
Lifetime keeps counting while a sandbox is paused under the default
`timeout_action: kill`. Pausing does not buy time. Use `timeout_action: pause`
when you want the sandbox to survive its own deadline; a resume then starts a
fresh interval of the same length, because otherwise the resumed sandbox would
expire again immediately.
</Warning>

Pausing freezes every process in the sandbox, including a running execution.
That execution stays `running` and continues from the same instruction after
resume. This is not the same as a filesystem snapshot: a snapshot keeps files,
pause keeps memory.

An open event stream counts as activity for the idle timeout. So does starting
an execution, which resumes a paused sandbox first. Reading files can also
resume a paused sandbox — file access is a legitimate reason for it to be alive.

## Kill reasons

When a sandbox dies, `kill_reason` says who did it. This is the field you want
in a log, not the exit code of whatever command happened to be running.

| `kill_reason` | Meaning |
| --- | --- |
| `requested` | You asked for it |
| `timeout` | Lifetime deadline with action `kill` |
| `idle_timeout` | Idle deadline with action `kill` |
| `evicted` | The container stopped or disappeared without the server asking |

`evicted` is how you find out the host did something behind your back: OOM
killer at the host level, `docker rm`, or a machine reboot.

## What survives what

This table is the one to check before designing around a lifecycle choice.

| Event | Filesystem | Process memory | Execution history |
| --- | --- | --- | --- |
| Pause → resume | kept | **kept** | kept |
| Daemon restart, recovery on | kept | lost | records reconciled |
| Sandbox killed | lost | lost | retained per history limits |
| Host restart | lost | lost | lost |

<Note>
Recovery restores sandbox records and their filesystems. It does not restore
running processes, and it deliberately does not replay interrupted commands —
a command with unknown side effects is not re-run. An interrupted execution is
reported as `unknown`, which is a failure outcome, not a success.
</Note>

## Deleting a sandbox with work in flight

Deleting a sandbox while an execution runs terminates that execution with
reason `canceled`. It does not wait for the command to finish, and it does not
pretend the command exited on its own.

## Retrying is a client decision

Nothing here retries for you. Whether it is safe to create a new sandbox after
a lost response depends on what your command did. For the requests where a
retry is safe, the API carries idempotency keys; see
[Execution semantics](/concepts/execution).
