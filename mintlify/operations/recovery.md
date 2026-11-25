---
title: "Restart recovery"
description: "What the journal restores, what it refuses to guess, and why it never replays a command."
---

Set `-state-dir` to recover retained execution records and eligible idle
containers after a daemon restart. Without a journal, startup removes
containers with the same owner label and starts with empty state.

From the [built checkout](/quickstart#build-the-binaries), with the Docker
socket configured, start the daemon with a private directory and a stable,
unique owner. Keep the same values when restarting this deployment:

```sh
./bin/execletd \
  -state-dir "$HOME/.local/state/execlet" \
  -owner prod-host-1
```

<Warning>
Do not share an owner between daemons on the same Docker engine, even with
separate state directories. Otherwise one daemon can remove the other's
containers. The owner label is for reconciliation, not access control.
</Warning>

The directory must be private (`0700`), and its records and lock are private
regular files (`0600`). A kernel file lock prevents a second writer. A wrong
owner, an unsupported format, or a corrupt record graph **rejects startup**
rather than reconciling against a state it cannot trust.

## What recovery does, in order

Before accepting traffic:

1. **Validate** the complete record graph, and that terminal outcomes agree with their events.
2. **Reconcile exact owned container IDs.** Eligible idle containers keep their files and completed execution records. In policy mode, limits and budgets are revalidated.
3. **Reconcile interrupted executions.** Discard their sandboxes and record `completed` with reason `unknown`, a null exit code, and `output_truncated=true`.
4. **Remove owned orphans and unsafe containers.** If a safe state cannot be established, admission is refused.

<Note>
After a crash, an execution still recorded as running becomes `unknown`, even
if it finished before the crash. Execlet never replays it. A graceful shutdown
with recovery enabled instead tries to cancel active commands and remove their
sandboxes before exiting.
</Note>

## What survives

| Artifact | Survives a restart |
| --- | --- |
| Sandbox records | Yes, subject to validation |
| Filesystem of an idle container | Yes, if the exact container survived and passed recovery checks |
| Completed execution records, outcomes, retained output | Yes, subject to retention |
| Idempotency keys | Yes, while their records live |
| **Live output of a running execution** | **No** — never journaled |
| Live commands or terminal sessions | Not resumed |
| Sandboxes using filtered networking | Not adopted |
| Paused containers under policy mode | No — the enforcement probe cannot run without resuming them |

A nonzero SSE cursor into lost live output returns `409 invalid_state`. Reading
the snapshot instead shows the `unknown`/truncated result, not a fabricated
complete transcript.

## The journal is not encrypted

It stores sandbox metadata, start intents, bounded terminal events, idempotency
keys, and the invocation fingerprint key. Sandbox environments, argv, and
retained output **can contain secrets**.

Treat its ancestors and its host account as trusted, and never mount it into a
guest. Network filesystems, shared writers, tamper resistance, and
state-format migration are unsupported.

## When writes fail

A journal write failure makes `/readyz` return 503 and blocks new sandboxes
and executions. If a terminal result cannot be persisted, the result is
reported as `unknown`, not successful completion.

```text
1. Repair storage.
2. Restart with the SAME -owner and -state-dir.
3. Do not clear the journal to force readiness.
4. Inspect `unknown` outcomes before deciding whether business work can be retried.
```

## Backups and downgrades

Back up the state directory **while the daemon is stopped**. Roll back binaries
only with a compatible journal: rolling back a binary is not a state-format
rollback, and an older binary may not understand a newer journal.

## What this is not

It is **control-record recovery and idle-container adoption**, not live-process
takeover, not memory checkpoints, and not durable full logs. The fsync cost of
checkpoints bounds throughput, and a post-crash idle expiry uses the last
checkpoint rather than exact activity. This is not an HA database.

If you need live execution recovery or multiple hosts, that is out of scope —
see [self-hosting](/operations/self-hosting#what-is-not-included) for why.
