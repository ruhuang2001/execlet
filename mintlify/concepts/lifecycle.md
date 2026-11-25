---
title: "Sandbox lifecycle"
description: "States, timeouts, and what pause actually preserves."
---

A sandbox is a Docker container plus a lifecycle record. Keep it running to
reuse its files, pause it to suspend work, or delete it to release resources.

```text
pending ──► running ◄──► paused
               │            │
               └────┬───────┘
                    ▼
                 killed
```

`killed` is terminal. Nothing comes back from it; you create a new sandbox.

## Timeouts

Sandbox lifetime and idle time are separate from a command's deadline.

| Timeout | Default | Counts | Disabled by |
| --- | --- | --- | --- |
| Lifetime (`timeout_seconds`) | 300 s | From creation by default; check `expires_at` after an update | `0` |
| Idle (`idle_timeout_seconds`) | 0 (off) | Since the last activity, checked while running with no active execution or event stream | `0` |

Each selects `kill` or `pause` through `timeout_action` and `idle_timeout_action`.
The default is `kill` for both. Paused sandboxes still consume resources and
reserve [policy capacity](/operations/policy).

<Warning>
Lifetime keeps counting while a sandbox is paused under the default
`timeout_action: kill`. Pausing does not buy time. Use `timeout_action: pause`
when you want a sandbox to pause at its lifetime deadline. A resume then
starts a fresh interval using its configured `timeout_seconds`.
</Warning>

Pause suspends container work without changing an active execution's `running`
state. It is not a filesystem snapshot or a reset of a command deadline.
Verify pause behavior on your [Docker runtime](/operations/isolation#running-under-gvisor).

An open event stream prevents idle expiry. Starting an execution or reading
files can resume a paused sandbox. Inspecting a sandbox's metadata does not
need to resume it.

## Kill reasons

Use the sandbox's `kill_reason` for lifecycle logging, not the exit code of
the command that happened to be running.

| `kill_reason` | Meaning |
| --- | --- |
| `requested` | You asked for it |
| `timeout` | Lifetime deadline with action `kill` |
| `idle_timeout` | Idle deadline with action `kill` |
| `evicted` | The container stopped or disappeared without the server asking |

## What survives what

The table describes the sandbox's writable container layer, not external storage.

| Event | Writable layer | Live commands | Execution history |
| --- | --- | --- | --- |
| Pause → resume | Kept | Suspended, then resumed | Kept |
| Daemon restart with recovery | Kept only for eligible idle containers that survived | Not restored | Retained records reconciled |
| Confirmed sandbox removal | Removed | Stopped | Kept within retention limits |
| Host or Docker restart | No survival guarantee | Not restored | Journal records may survive with recovery enabled |

[Restart recovery](/operations/recovery) does not replay interrupted commands.
Their effects may already have happened even though the result is `unknown`.

## Deleting a sandbox with work in flight

Deleting a sandbox requests container removal and records `canceled` for
in-flight executions unless an earlier intent already applies. A `killed`
record alone is not proof that Docker removed the container. If the delete
request reports unconfirmed cleanup, check the daemon's pending removals.

## Retrying is a client decision

Nothing here retries for you. Whether it is safe to create a new sandbox after
a lost response depends on what your command did. For the requests where a
retry is safe, the API carries idempotency keys; see
[Execution semantics](/concepts/execution).
