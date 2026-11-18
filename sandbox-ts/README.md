# @tiny-sandbox/sdk

TypeScript SDK for tiny-sandbox: lifecycle, binary files, authenticated SSE,
bounded retries, idempotent starts and confirmed AbortSignal cancellation.
ESM, Node 22+ or modern browsers, no runtime dependencies.

This package is built from the repository; npm publication is a separate step.
From this directory, build the package:

```sh
npm ci
npm pack
```

Install the emitted `.tgz` in your application with `npm install /path/to/package.tgz`.
Use client and server builds from compatible revisions.

~~~ts
import { Client, SandboxError } from '@tiny-sandbox/sdk';

const client = new Client({ domain: 'http://localhost:8080' });
const sandbox = await client.createSandbox({ timeout_seconds: 300 });
try {
  const execution = await client.startExecution(sandbox.id, {
    command: ['sh', '-c', 'echo hello; sleep 2; echo done'],
  });
  for await (const event of client.streamExecution(sandbox.id, execution.id)) {
    if (event.type === 'stdout' || event.type === 'stderr') console.log(event.data);
    if (event.type === 'complete') console.log(event.execution.termination_reason);
  }
  const result = await client.exec(sandbox.id, ['sh', '-c', 'exit 42']);
  if (result.reason === 'exited') console.log(result.execution.exit_code);
} catch (error) {
  if (error instanceof SandboxError) console.error(error.code, error.execution);
  else throw error;
} finally {
  await client.killSandbox(sandbox.id);
}
~~~

Pass `{ signal: controller.signal }` as the final method argument to abort.
Aborting streams cancels remote work with fresh five-second recovery; breaking
iteration or throwing from callbacks only closes the stream. Inspect
`cancel_unconfirmed` errors before assuming work stopped. Completed nonzero
commands resolve outcomes; transport/API/cancellation failures reject.

See the [SDK guide](../docs/sdk-typescript.md) for the complete method, error,
retry and compatibility contract. Browser usage requires same-origin hosting
or a CORS-configured proxy; sandboxd does not itself enable CORS. Do not bundle
privileged bearer credentials.

From the repository root, verify against real Docker:

```sh
bash scripts/e2e-typescript.sh artifacts/typescript-e2e
```

Evidence is retained under `artifacts/typescript-e2e`.

## Optional E2B facade

`import { Sandbox, CommandExitError } from '@tiny-sandbox/sdk/e2b'` exposes the
E2B 2.64.0 Sandbox subset for Node: lifecycle, foreground/background Bash commands
with serial callbacks, compatible errors, text/binary files and bounded directory lists.
It has no E2B runtime dependency or cloud-key requirement. Set
`SANDBOX_E2B_IMAGE` to a local compatibility image digest and use the matching
server checkout. All command operations require the authenticated server capability
`e2b.background.v1`; an absent flag or old endpoint fails with `incompatible_server`
before starting or signaling work. Authentication errors remain distinct. The
capability cache belongs to each client and its immutable endpoint/credential.

```ts
import { Sandbox, type CommandHandle } from '@tiny-sandbox/sdk/e2b';

const sandbox = await Sandbox.create(); // SANDBOX_E2B_IMAGE pins the local image
const command: CommandHandle = await sandbox.commands.run('sleep 2; printf done', {
  background: true, timeoutMs: 60_000,
  onStdout: async text => { console.log(text); },
});
const saved = { sandboxId: sandbox.sandboxId, pid: command.pid };
console.log(command.executionId); // tiny-sandbox immutable execution identity extension
await command.disconnect(); // local observation only; command keeps running

// In another process, after Sandbox.connect(saved.sandboxId):
const attached = await sandbox.commands.connect(saved.pid, { timeoutMs: 60_000 });
console.log(await attached.wait());
await sandbox.kill(); // explicit sandbox cleanup when finished
```

- `background: true` returns a `CommandHandle` even for fast exits. Omitted/false
  returns `CommandResult`; a dynamic boolean returns the union.
- Both foreground and background `run` set a **remote command deadline**, default
  60,000 ms. This corrects v0.0.2's local-only behavior. Zero disables it, not
  sandbox lifetime. `timeoutMs` must be a nonnegative safe integer at most
  9,223,372,036,854; fractional milliseconds and deferred options are rejected.
- The independent observation clock starts at `run`/`commands.connect` entry.
  Connect's clock is local only; it neither renews sandbox TTL nor resets the
  original command deadline. Local timeout does not confirm remote termination.
- Handles consume immediately, expose live `stdout`/`stderr`, optional `exitCode`
  and `error`, and share/cache repeated and concurrent `wait()` calls. Callbacks
  are awaited serially. Callback rejection preserves the original value even
  when `wait()` is called later; it never cancels work.
- `disconnect()` promptly closes observation, including during silent commands
  or pending/self-disconnecting callbacks. It is idempotent. A disconnected
  handle cannot resume via `wait()`; cached results/errors remain readable.
- A fresh `commands.connect(pid)` observes only output after its atomic attach
  boundary. Completed attach has empty output with the real exit outcome.
  Same-handle transport retries use its event cursor and immutable execution ID,
  never another PID lookup. Native `getExecution` retrieves retained full logs.
- `handle.kill()` targets its original execution, even after disconnect or PID
  reuse. `commands.kill(pid)` resolves the newest retained managed incarnation
  once. Both request immediate managed process-group SIGKILL, returning true
  only for confirmed signaling/termination, or false for known completion/absence.
  An uncertain response or container-removal fallback raises an identity-bearing
  `backend_error`, never success/false. Kill is deliberately not automatically
  replayed after a lost acknowledgement; retry explicitly by the same handle.
- Ordinary nonzero exits throw `CommandExitError`; truncation, cancellation, OOM,
  eviction and unknown termination retain diagnostic codes and real evidence.
  No cleanup/finalizer sends remote cancel. Kill uses the configured
  `requestTimeoutMs` (default 30 seconds). Per-command request timeouts, signals,
  interactive stdin, PTYs and process listing are not supported.

See the [background contract](../docs/e2b-background-commands.md),
[migration example](../examples/e2b/README.md) and
[supported subset](../docs/e2b-compatibility.md). The native package root
retains its browser support and default remote-cancel behavior.

### Additive native methods

`Client.getCapabilities()` returns `{ capabilities }`. `resolveCommand(sandboxId,
pid)` returns `{ execution, last_event_id }`; `killExecution(sandboxId, executionId)`
returns `{ execution, signaled }` only after a confirmed response. Managed starts
use `timeout_ms` (including explicit zero), mutually exclusive with
`timeout_seconds`. Execution responses can include a confirmed guest `pid`.
`streamExecution` accepts `lastEventId` to start after an attach watermark.
Existing native defaults, seconds-based deadlines and graceful `/cancel` behavior
are unchanged; native calls do not automatically require the E2B capability gate.

### Local adapter regressions

```sh
npm run build
npm run check
node --test tests/*.test.mjs
```

These exercise public package exports with controlled HTTP/SSE faults, not real
guest-process evidence. Run the installed Docker background acceptance profile
separately before claiming complete backend/adapter compatibility.
