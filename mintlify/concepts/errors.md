---
title: "Typed failures"
description: "Why exit 137 is not an answer, and what to branch on instead."
---

A command returned exit code 137. It might have been killed, or it might have
explicitly exited with 137. Use Execlet's recorded outcome rather than guessing
from the exit code alone.

## Two different questions

| Question | Field | On |
| --- | --- | --- |
| Why did this command end? | `termination_reason` | the execution |
| Why did this sandbox become `killed`? | `kill_reason` | the sandbox |

A command can time out while its sandbox keeps running. Losing a sandbox can
end multiple in-flight executions without changing ones that already completed.

## Termination reasons

| Reason | Condition | Typical reaction |
| --- | --- | --- |
| `exited` | The process exited with no higher-priority reason | Inspect `exit_code` |
| `canceled` | A cancellation or sandbox deletion ended the command | Respect the cancellation |
| `timeout` | The sandbox expired with action `kill`, or a command deadline fired | Adjust the budget or split the work |
| `oom` | An OOM kill was observed and can be attributed to this execution | Reduce memory use or raise the limit |
| `evicted` | The runtime stopped the sandbox or it could no longer be observed | Check infrastructure and reconcile side effects before retrying |
| `unknown` | The outcome cannot be established, for example after an interrupted start or crash recovery | Treat as failure, not success |

When cancel and timeout race, the first intent the server records takes
precedence. A cancel request does not necessarily win over a deadline or a
command that already finished. Inspect the final snapshot.

<Warning>
**Exit code 137 alone does not imply `oom`.** Execlet checks for an increase in
OOM-kill evidence. It does not attribute that increase to one command when
multiple execution slots are configured or a terminal overlapped it. An
otherwise unexplained 137 can then be `unknown` if the sandbox is still alive.
</Warning>

## Kill reasons

`requested`, `timeout`, `idle_timeout`, `evicted`. See
[Sandbox lifecycle](/concepts/lifecycle#kill-reasons).

## HTTP errors

API errors use `{"code": "...", "message": "..."}`. Branch on `code`:
different errors can share an HTTP status. The retry column applies only to
operations safe to repeat.

| Status | `code` | Retryable |
| --- | --- | --- |
| 400 | `invalid_request` | no |
| 401 | `unauthorized` | no |
| 404 | `not_found` | no |
| 409 | `invalid_state` | no |
| 409 | `busy` | no — wait for a slot to free, then retry |
| 409 | `idempotency_conflict` | no — the key means something else now |
| 413 | `payload_too_large` | no |
| 429 | `rate_limited` | yes, with backoff |
| 500 | `internal` | yes |
| 502 | `backend_error` | yes |
| 503 | `unavailable` | yes |

Honor a positive `Retry-After` in seconds when present on a repeatable request;
otherwise use backoff.

## What SDKs do automatically

The [Go](/sdk/go#retry-rules-worth-remembering), [Python](/sdk/python), and
[TypeScript](/sdk/typescript#retries) clients retry repeatable requests after
transport failures and HTTP 429, 500, 502, 503, and 504. They use jittered
backoff capped at two seconds unless `Retry-After` applies. SSE reconnects
have a bounded retry budget and ignore that header.

**Sandbox creation is not retried automatically.** A missing response is not
proof that no container was created. Execution starts reuse an
[idempotency key](/concepts/execution#idempotent-starts) across retries, while the
server retains that execution record.

<Note>
The Python client's zero value for `max_retries` means "no retries", not
"default". This differs from Go and TypeScript, where `0` selects three.
</Note>

## A decision table for agents

```text
exited 0            → success
exited non-zero     → failed, surface stderr
timeout             → decide: bigger budget, or smaller task
canceled            → do not retry automatically
oom                 → lower memory per step, or raise the limit
evicted             → check infrastructure before recreating the sandbox
unknown             → inspect by execution ID; reconcile side effects before retrying
```
