---
title: "TypeScript SDK"
description: "Browser-safe fetch and SSE with AbortSignal, discriminated outcomes, and no runtime dependencies."
---

## Install a local build

Set `EXECLET_SRC` to the absolute path of the [checkout](/installation#use-the-sdks-from-a-checkout)
that built your daemon. In a new directory for this example, build a tarball
and install it into a local consumer:

```sh
npm init -y
npm --prefix "$EXECLET_SRC/sandbox-ts" ci
SDK_TARBALL=$(npm pack "$EXECLET_SRC/sandbox-ts" --silent)
npm install "./$SDK_TARBALL"
```

The installed package is `@execlet/sdk`. It runs in Node 22+ or a modern
browser. It has no runtime dependencies or Node built-in imports: the host
supplies `fetch`, `ReadableStream`, `TextDecoder`,
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
discriminated union. With the daemon running, save this as `demo.mjs` and run
`node demo.mjs`:

```ts
import { Client } from "@execlet/sdk";

const client = new Client();
const sandbox = await client.createSandbox({ image: "alpine:3.20", network: "none" });
try {
  const outcome = await client.execStream(sandbox.id, ["sh", "-c", "echo hi; exit 3"], (event) => {
    if (event.type === "stdout") process.stdout.write(event.data);
    if (event.type === "stderr") process.stderr.write(event.data);
  });
  if (outcome.reason === "exited") {
    console.log(`exit ${outcome.execution.exit_code}`);
  } else if (outcome.reason === "unknown") {
    console.error("unexplained", outcome.terminationReason);
  } else {
    console.error(outcome.reason);
  }
} finally {
  await client.killSandbox(sandbox.id);
}
```

It prints `hi` and `exit 3`. The nonzero command exit is not a rejected promise.
`execStream` starts, streams, and resolves an outcome. The `unknown` variant
carries the original value in `terminationReason`.

Only `exited` with exit code zero is command success. `unknown` is a failure
variant, never a success.

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

for await (const event of client.streamExecution(sandboxID, exeID, { signal: controller.signal })) {
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
- Aborting the caller's signal requests cancellation by default. Observation
  rejects even if cancellation is confirmed; check for `cancel_unconfirmed`
  before assuming the command stopped. Set `cancelRemote: false` on
  `streamExecution` to end observation without sending cancellation.

Unknown event types surface as `type: "unknown"` with the original `event_type`
and raw data rather than being dropped; their IDs still advance the cursor.

## Retries

Repeatable operations such as GET, ordinary file PUT, pause/resume/cancel,
and keyed starts retry transport failures and 429/500/502/503/504.
`startExecution` generates a key if you do not supply one. Backoff is
`min(2000ms, 200ms * 2^attempt)`, uniformly sampled between half and the full
cap. A positive integer `Retry-After` overrides it; SSE reconnects ignore it.

**Attempted once, never retried:** sandbox creation, lifetime `PATCH`, and
archive uploads. Their effects are not safely repeatable.

## Browser notes

CORS and authentication are your deployment's problem, not the SDK's: the
daemon sets no permissive CORS headers, so a browser app talks to a proxy you
control. Keep the API key off the client — expose your backend, not the daemon,
to a browser.
