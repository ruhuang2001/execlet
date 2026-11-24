---
title: "Restart recovery"
description: "What the journal restores, what it refuses to guess, and why it never replays a command."
---

With `-state-dir`, execlet writes a local journal and can recover across a
restart. Without it, state is in memory and a restart **removes owned
containers**.

```sh
execletd \
  -state-dir /var/lib/execlet \
  -owner prod-host-1
```

<Warning>
Use a stable, unique `-owner` and never share it between daemons — even with
different state directories. The owner is how a daemon proves which containers
are its own. Two daemons sharing one will adopt each other's work.
</Warning>

The directory must be private (`0700`), and its records and lock are private
regular files (`0600`). A kernel file lock prevents a second writer. A wrong
owner, an unsupported format, or a corrupt record graph **rejects startup**
rather than reconciling against a state it cannot trust.

## What recovery does, in order

Before accepting traffic:

1. **Validate** the complete record graph, and that terminal outcomes agree with their events.
2. **Reconcile exact owned container IDs.** Eligible idle containers keep their files and completed execution records. In policy mode, limits and budgets are revalidated.
3. **Mark interrupted executions** `completed` with reason `unknown`, a null exit code, and `output_truncated=true` — then remove their whole sandbox, including siblings.
4. **Remove owned orphans and unsafe containers.** If a safe state cannot be established, admission is refused.

<Note>
Step 3 is the most important behavior in this document. A command was running
when the daemon died. Its side effects may or may not have happened. execlet
does not re-run it and does not infer success from a surviving process. It says
`unknown` and fails the sandbox — **even if the command actually finished**.
That is the honest answer, and it is a failure outcome in every SDK.
</Note>

## What survives

| Artifact | Survives a restart |
| --- | --- |
| Sandbox records | Yes, subject to validation |
| Filesystem of an idle container | Yes |
| Completed execution records, outcomes, retained output | Yes, subject to retention |
| Idempotency keys | Yes, while their records live |
| **Live output of a running execution** | **No** — never journaled |
| Running processes | No |
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

A journal write failure closes `/readyz` and stops new sandbox and execution
admission. It never acknowledges a completion it did not persist.

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
