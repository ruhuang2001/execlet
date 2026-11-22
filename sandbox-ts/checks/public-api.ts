import { Client, SandboxError, type Event, type ExecutionOutcome, type Sandbox } from '../src/index.js';

function describe(result: ExecutionOutcome): string {
  switch (result.reason) {
    case 'exited': return String(result.execution.exit_code);
    case 'canceled': return result.execution.id;
    case 'timeout': return result.execution.id;
    case 'oom': return result.execution.id;
    case 'evicted': return result.execution.id;
    case 'unknown': return String(result.terminationReason);
    default: { const exhaustive: never = result; return exhaustive; }
  }
}

async function consumer(client = new Client(), signal?: AbortSignal) {
  const options = signal ? { signal } : {};
  const sandbox = await client.createSandbox({ timeout_seconds: 0, nano_cpus: 0 }, options);
  const started = await client.startExecution(sandbox.id, { command: ['echo', 'hello'] }, options);
  const placed = await client.startExecution(sandbox.id, { command: ['pwd'], env: { MY_VAR: 'x' }, cwd: '/tmp' }, options);
  const placedCwd: string | null | undefined = placed.cwd;
  const events: AsyncIterable<Event> = client.streamExecution(sandbox.id, started.id, options);
  for await (const event of events) {
    switch (event.type) {
      case 'stdout': case 'stderr': event.data satisfies string; break;
      case 'truncated': event.limit_bytes satisfies number; break;
      case 'complete': event.execution.id satisfies string; break;
      case 'unknown': event.event_type satisfies string; break;
      default: { const exhaustive: never = event; void exhaustive; }
    }
  }
  await client.streamExecution(sandbox.id, started.id, async event => { void event; }, options);
  describe(await client.exec(sandbox.id, ['false'], options));
  describe(await client.execStream(sandbox.id, ['true'], () => {}, options));
  await client.writeFile(sandbox.id, '/tmp/file', new Uint8Array([0, 255]), options);
  const bytes: Uint8Array = await client.readFile(sandbox.id, '/tmp/file', options);
  for (const entry of (await client.listFiles(sandbox.id, '/tmp', options)).entries) {
    entry.type satisfies string;
    entry.uid satisfies number;
    entry.gid satisfies number;
  }
  const future: Sandbox['state'] = 'future_state';
  const retryable: boolean = new SandboxError('transport', 'failed').retryable;
  void [bytes, future, retryable, placedCwd];
  const flags: string[] = (await client.getCapabilities(options)).capabilities;
  const managed = await client.startExecution(sandbox.id, { command: ['true'], timeout_ms: 1250 }, options);
  const pid: number | undefined = managed.pid;
  const attached = await client.resolveCommand(sandbox.id, pid!, options);
  const suffix = client.streamExecution(sandbox.id, attached.execution.id, {
    ...options, lastEventId: attached.last_event_id, cancelRemote: false,
  });
  const killed: boolean = (await client.killExecution(sandbox.id, managed.id, options)).signaled;
  void [flags, pid, suffix, killed];
}
void consumer;
