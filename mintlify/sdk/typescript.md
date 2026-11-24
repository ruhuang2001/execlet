---
title: "TypeScript SDK"
description: "Browser-safe fetch and SSE with AbortSignal, discriminated outcomes, and no runtime dependencies."
---

```sh
npm install @execlet/sdk
```

Node 22+ or a modern browser. No runtime dependencies and no Node built-in
imports: the host supplies `fetch`, `ReadableStream`, `TextDecoder`,
`AbortController`, and Web Crypto. Declarations support TypeScript 5.9+ with DOM
types.

## Connect

```ts
import { Client } from "@execlet/sdk";

const client = new Client({ domain: "http://localhost:8080", maxRetries: 3 });
```

| Option | Meaning |
| --- | --- |
| `domain` | Option, then `SANDBOX_DOMAIN`, then `http://localhost:8080`. Credentials, query strings, and non-HTTP(S) schemes are rejected |
| `apiKey` | Option, then `SANDBOX_API_KEY`. Sent as a bearer header, including on streams |
| `maxRetries` | Omitted or `0` means three; negative disables; positive counts additional attempts |
| `fetch` | Injectable transport. Must honor `AbortSignal` and streaming bodies |

Environment access is optional and needs no Node imports, which is what makes
the same build usable in a browser and a worker.

## Outcomes, not exceptions, for program results

A completed command **resolves**, even when it failed. The result is a
discriminated union:

```ts
const sandbox = await client.createSandbox({ timeout_seconds: 300 });

// execStream starts, streams, and resolves an ExecutionOutcome.
const outcome = await client.execStream(
  sandbox.id,
  ["sh", "-c", "echo hi; exit 3"],
  (event) => {
    if (event.type === "stdout" || event.type === "stderr") process.stdout.write(event.data);
  },
);

switch (outcome.reason) {
  case "exited":
    // outcome.execution.exit_code === 0 is the only success
    break;
  case "timeout":
  case "oom":
  case "evicted":
  case "canceled":
    break;
  case "unknown":
    // outcome carries the original termination_reason
    break;
}
```

Only `exited` with exit code zero is command success. `unknown` is a failure
variant, never a success. An exhaustive switch is the point: adding a variant
later becomes a compile error rather than a silent fall-through.

Thrown `SandboxError` is reserved for things that are not program results: API
errors, transport failures, protocol errors, callback errors, and cancellation.

```ts
import { SandboxError } from "@execlet/sdk";

try {
  await client.startExecution(sandboxID, { command: ["false"] });
} catch (err) {
  if (err instanceof SandboxError) {
    console.log(err.code, err.statusCode, err.retryable);
  }
}
```

`retryable` classifies a failure; it does **not** authorize retrying an unsafe
operation. Creation is still yours to decide.

## Streaming

```ts
const controller = new AbortController();

for await (const event of client.streamEvents(sandboxID, exeID, { signal: controller.signal })) {
  if (event.type === "stdout") process.stdout.write(event.data);
  if (event.type === "complete") break;
}
```

Four behaviors worth relying on:

- Reconnect sends `Last-Event-ID` and suppresses events at or below it, so
  delivery within one call is ordered and duplicate-free.
- The `maxRetries` budget is cumulative and never resets on output. EOF before
  `complete` is retryable; `complete` ends iteration without reconnecting.
- Breaking out of `for await`, or calling `iterator.return()`, closes the
  connection. **It does not cancel the execution.** Abandoning an iterator
  without closing it is not a cleanup mechanism.
- Aborting the caller's signal *does* cancel, and raises even after cleanup
  succeeds.

Unknown event types surface as `type: "unknown"` with the original `event_type`
and raw data rather than being dropped; their IDs still advance the cursor.

## Retries

Repeatable operations — GET, DELETE, ordinary file PUT, pause/resume/cancel,
and keyed starts — retry transport failures and 429/500/502/503/504. Backoff is
`min(2000ms, 200ms * 2^attempt)`, uniformly sampled between half and the full
cap. A positive integer `Retry-After` overrides it; SSE reconnects ignore it.

**Attempted once, never retried:** sandbox creation, relative lifetime
`PATCH`, and archive uploads. Their effects are not safely repeatable.

## Browser notes

CORS and authentication are your deployment's problem, not the SDK's: the
daemon sets no permissive CORS headers, so a browser app talks to a proxy you
control. Keep the API key off the client — expose your backend, not the daemon,
to a browser.
