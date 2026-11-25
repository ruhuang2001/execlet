---
title: "Execution and streaming"
description: "Start a command, resume its retained output, and confirm how it ended."
---

Start a command once, then use its execution ID to follow its output, inspect
its result, or cancel it. The command and its event stream have separate lifetimes.

```text
POST .../executions        ──► execution id
GET  .../executions/{id}/events   ──► SSE stream, reconnectable
```

Closing the HTTP stream does not cancel the command. It can keep running until
it exits, a deadline applies, or it is explicitly stopped. An SDK may send a
separate cancel request when you cancel its observation context or signal.

## The event stream

Events are server-sent events with decimal IDs starting at 1, increasing by one
per event. Four types:

| Type | Data | Notes |
| --- | --- | --- |
| `stdout` | `{"data": "..."}` | at most 32 KiB per chunk |
| `stderr` | `{"data": "..."}` | same limit, independent stream |
| `truncated` | `{"limit_bytes": N}` | the server discarded some output |
| `complete` | the full execution object | always last, and closes the stream |

Output is **text**. Chunks are cut on UTF-8 boundaries so a multi-byte character
is never split across two events, and invalid bytes are replaced with U+FFFD
within each stream independently. If you need exact bytes — a JPEG, a tarball —
do not run `cat` and parse stdout. Use the file API, which is byte-exact.

## Reconnecting

Send the last ID you processed as `Last-Event-ID`. The server returns retained
events with greater IDs, then continues live. If the output was truncated,
some events may already be gone.

Set `SB` and `EXE` to the IDs printed by `execletctl run` without `--rm`.
Leave `LAST_EVENT_ID` unset to start from the beginning, or set it to the
last processed ID when reconnecting:

```sh
curl --no-buffer --fail-with-body -H 'Accept: text/event-stream' \
  -H "Authorization: Bearer ${SANDBOX_API_KEY:-}" \
  -H "Last-Event-ID: ${LAST_EVENT_ID:-0}" \
  "${SANDBOX_DOMAIN:-http://127.0.0.1:8080}/v1/sandboxes/$SB/executions/$EXE/events"
```

<Warning>
A negative, non-integer, or too-new `Last-Event-ID` returns
`400 invalid_request`. If a restart discarded the live output, a nonzero
cursor returns `409 invalid_state`; read the execution snapshot instead.
</Warning>

The server sends a `ping` comment every 15 seconds so proxies do not reap an
idle stream. Each sandbox allows 32 concurrent streams; more returns `429`
until one closes.

## Retention limits

Each execution retains up to **1 MiB** of stdout and stderr combined and
**4,096 output events**. By default it keeps the beginning and stops storing
new output at the limit. Set `output_tail: true` on start to keep recent
output instead; older events can disappear even while a stream is open.

- Each sandbox keeps at most **200** execution records, always protecting
  active executions and the most recently completed one. An evicted record
  returns `404 not_found`.
- Eligible records and their idempotency keys can be pruned by history or
  aggregate-output limits. **No fixed replay window is guaranteed.**
- When tail retention drops earlier bytes, `stdout_offset` and `stderr_offset`
  give the absolute byte index of the first retained byte, and
  `output_truncated` is true.

Check `output_truncated` before treating a transcript as complete. If your
command must preserve a full transcript or binary artifact, write it to files
and retrieve them before deleting the sandbox.

## Cancelling

Native `POST .../executions/{id}/cancel` records a cancellation intent and
signals the process group with SIGTERM. If it does not observe completion
within two seconds, it tries SIGKILL. After up to eight more seconds it
attempts to remove the container. That fallback can stop other commands and
discard the writable layer; it can also be needed if the in-guest signal
helper cannot start. Signaling a group does not cover processes that left it.

Check the returned state and `termination_reason`. A cancel request can race
with completion or return before a terminal result is confirmed. If SDK
cancellation reports `cancel_unconfirmed`, inspect the execution by ID before
starting replacement work. Disconnecting a cancel request itself does not
shorten the server's escalation once it is underway.

## Concurrency

`max_concurrent_executions` is set at creation, between 1 and 8, default 1. It
exists because concurrent commands in one container interfere in ways that are
hard to attribute: they share a filesystem, an environment, and often a working
directory. Raise it when you know the workloads are independent.

## Idempotent starts

Starting an execution accepts an `idempotency_key`: at most 128 bytes,
**scoped to one sandbox**. A retry with the same key returns the original
execution instead of starting a second one — the only way a retry can be safe
for a command with side effects.

Retry the same request with the same key. Reusing a retained key with a
different `command`, `cwd`, `env`, `stdin`, `timeout_ms`, or `output_tail`
returns `409 idempotency_conflict` rather than running different work. A valid
change to `timeout_seconds` on a replay does not change the original deadline.

<Note>
A key is remembered only while its execution record is retained. When history
drops the record, the key goes with it; a later retry with that key then starts
a **new** execution. Do not rely on a key outliving the record it protects.
</Note>
