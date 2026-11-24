---
title: "Execution and streaming"
description: "Executions are resources. Output reconnects without loss or duplication."
---

Running a command is not a request that returns text. It is a resource that
produces an event stream, and the two have different lifetimes.

```text
POST .../executions        ──► execution id
GET  .../executions/{id}/events   ──► SSE stream, reconnectable
```

Closing the stream never affects the execution. Your process can die, your
network can flap, and the command keeps running until it finishes or something
cancels it.

## The event stream

Events are server-sent events with decimal IDs starting at 1, increasing by one
per event. Four types:

| Type | Data | Notes |
| --- | --- | --- |
| `stdout` | `{"data": "..."}` | at most 32 KiB per chunk |
| `stderr` | `{"data": "..."}` | same limit, independent stream |
| `truncated` | `{"limit_bytes": N}` | sent at most once, when retention evicts output |
| `complete` | the full execution object | always last, and closes the stream |

Output is **text**. Chunks are cut on UTF-8 boundaries so a multi-byte character
is never split across two events, and invalid bytes are replaced with U+FFFD
within each stream independently. If you need exact bytes — a JPEG, a tarball —
do not run `cat` and parse stdout. Use the file API, which is byte-exact.

## Reconnecting

Send the last ID you saw as `Last-Event-ID`. The server replays only events with
a greater ID, then continues live. No gaps, no duplicates.

```sh
curl -N -H 'Authorization: Bearer ...' \
  "$SANDBOX_DOMAIN/v1/sandboxes/$SB/executions/$EXE/events"
```

After a drop, resume from event 42 by sending that ID:

```sh
curl -N -H 'Authorization: Bearer ...' \
  -H 'Last-Event-ID: 42' \
  "$SANDBOX_DOMAIN/v1/sandboxes/$SB/executions/$EXE/events"
```

<Warning>
A non-integer `Last-Event-ID`, or one beyond the newest event, is
`400 invalid_request` rather than a silent restart from zero. That is
deliberate: silently replaying everything would look like duplicated output.
</Warning>

The server sends a `ping` comment every 15 seconds so proxies do not reap an
idle stream. Each sandbox allows 32 concurrent streams; more returns `429`
until one closes.

## Retention limits

Output is not infinite, and the limit is stated rather than discovered:

- Each sandbox keeps at most **200** execution records, always protecting
  active executions and the most recently completed one. After that, the record
  is `404 not_found`.
- Eligible records and their idempotency keys can be pruned by history or
  aggregate-output limits. **No fixed replay window is guaranteed.**
- When tail retention drops earlier bytes, `stdout_offset` and `stderr_offset`
  give the absolute byte index of the first retained byte, and
  `output_truncated` is true.

If your agent needs the full transcript of a long build, capture it as you
stream. The record is a convenience, not a durable log.

## Cancelling

Cancellation is verified, not requested and forgotten. The server records the
intent, sends SIGTERM to the process group, waits up to 2 seconds, sends
SIGKILL, waits up to 8 more seconds, and only then asks Docker to remove the
container. The execution's reason becomes `canceled`.

Two properties worth knowing:

- **The whole process group, not just the leader.** A grandchild that outlives
  its parent is still killed.
- **Waiting is not optional.** A caller that disconnects its own cancel request
  does not shorten the escalation; the server finishes it so a client timeout
  cannot turn a slow signal into container removal.

One honest degradation: when the process tree has filled the sandbox's pid
limit, the in-container helper that signals the group cannot get a task slot.
Escalation then runs to container removal, which destroys the sandbox along
with the execution.

## Concurrency

`max_concurrent_executions` is set at creation, between 1 and 8, default 1. It
exists because concurrent commands in one container interfere in ways that are
hard to attribute: they share a filesystem, an environment, and often a working
directory. Raise it when you know the workloads are independent.

## Idempotent starts

Starting an execution accepts an `idempotency_key`: at most 128 characters,
**scoped to one sandbox**. A retry with the same key returns the original
execution instead of starting a second one — the only way a retry can be safe
for a command with side effects.

The key is part of a fingerprint. Reusing it with a different `command`, `cwd`,
`env`, or `stdin` is `409 idempotency_conflict` rather than a silent replay of
something else. `timeout_ms` and the output retention mode also participate,
so changing either changes the identity.

<Note>
A key is remembered only while its execution record is retained. When history
drops the record, the key goes with it; a later retry with that key then starts
a **new** execution. Do not rely on a key outliving the record it protects.
</Note>
