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
E2B 2.64.0 basic Sandbox subset for Node: lifecycle, Bash commands with callbacks
and compatible errors, text/binary files and complete bounded directory lists.
It has no E2B runtime dependency or cloud-key requirement. Set
`SANDBOX_E2B_IMAGE` to a local compatibility image digest and use the matching
server checkout. Command timeouts stop waiting, not remote work; use
`finally { await sandbox.kill(); }` for cleanup. See the
[migration example](../examples/e2b/README.md) and
[supported subset](../docs/e2b-compatibility.md). The native package root
retains its browser support and default remote-cancel behavior.
