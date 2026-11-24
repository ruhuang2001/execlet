---
title: "Typed failures"
description: "Why exit 137 is not an answer, and what to branch on instead."
---

A command exited with code 137. Something sent it SIGKILL. Was it your cancel?
A deadline? The kernel's OOM killer? The host evicting the container? The exit
code alone cannot tell you, and every choice you make from it is a guess.

execlet records the reason.

## Two different questions

Do not conflate them. They are answered by different fields, on different
objects.

| Question | Field | On |
| --- | --- | --- |
| Why did this command end? | `termination_reason` | the execution |
| Why does this sandbox no longer exist? | `kill_reason` | the sandbox |

A single execution can end with `timeout` while its sandbox keeps running. A
sandbox can be `evicted` while three executions end with `evicted` and a fourth
finished normally two seconds earlier.

## Termination reasons

| Reason | Condition | Typical reaction |
| --- | --- | --- |
| `exited` | The process exited with no higher-priority reason | Inspect `exit_code` |
| `canceled` | You cancelled it, or the sandbox was deleted | Expected; do not retry blindly |
| `timeout` | Lifetime kill, native timeout, or managed `timeout_ms` | Raise the deadline or split the work |
| `oom` | Positive cgroup OOM evidence, single-execution sandbox | Reduce memory use or raise the limit |
| `evicted` | The container stopped or disappeared | Infrastructure problem; recreate and retry |
| `unknown` | Unexplained 137 with concurrency > 1, unconfirmed start, or interrupted by recovery | Treat as failure; never as success |

The classifier is ordered: the first recorded intent wins. `canceled` and
`timeout` are intents the server writes **before** it acts, so a later intent
never overwrites the first — if you cancelled at the same moment a deadline
fired, you get `canceled`, because that is what you asked for.

<Warning>
**Exit code 137 never implies `oom`.** With more than one execution slot
configured, the server cannot attribute the kill to a specific execution, so it
reports `unknown`. If your agent treats unknown as retryable, add an attempt
budget; if it treats it as fatal, log it — `unknown` is a place where the
system is telling you it does not know, and inventing an answer there is how
agents get stuck in loops.
</Warning>

## Kill reasons

`requested`, `timeout`, `idle_timeout`, `evicted`. See
[Sandbox lifecycle](/concepts/lifecycle#kill-reasons).

## HTTP errors

Error bodies are `{"code": "...", "message": "..."}` and `code` is the stable
field. Branch on it, not on the message text and not on the status alone — two
different situations both return 409.

| Status | `code` | Retryable |
| --- | --- | --- |
| 400 | `invalid_request` | no |
| 401 | `unauthorized` | no |
| 404 | `not_found` | no |
| 409 | `invalid_state` | no |
| 409 | `busy` | no — wait for a slot to free, then retry |
| 409 | `idempotency_conflict` | no — the key means something else now |
| 413 | `payload_too_large` | no |
| 429 | `rate_limited` | yes, after `Retry-After` |
| 500 | `internal` | yes |
| 502 | `backend_error` | yes |
| 503 | `unavailable` | yes |

`429` and `503` may carry `Retry-After` in seconds. Honor it when it is a
positive integer; a zero, an HTTP-date, or a malformed value means use your own
backoff.

## What SDKs do automatically

Go and TypeScript retry the documented repeatable requests on 429, 500, 502,
503, and 504, with backoff capped at 2 seconds and jitter. **Starting a sandbox
is never retried automatically** — creating a second container because a
response was lost is not something a library should decide for you.

Stream reconnects use the same backoff and do not consume a `Retry-After` from
the response, because a stream has to reconnect faster than a request deadline.

<Note>
The Python client's zero value for `max_retries` means "no retries", not
"default". This differs from Go and TypeScript, where `0` selects three.
</Note>

## A decision table for agents

The shortest thing that behaves sensibly:

```text
exited 0            → success
exited non-zero     → failed, surface stderr
timeout             → decide: bigger budget, or smaller task
canceled            → you asked; do not retry
oom                 → lower memory per step, or raise the limit
evicted             → infrastructure; recreate and retry once
unknown             → failure; log the inputs; retry at most once
```
