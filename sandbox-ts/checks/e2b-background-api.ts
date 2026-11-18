// Also exercise generated declarations, not just the implementation's source types.
import { Sandbox, type CommandHandle, type CommandResult } from '../dist/e2b.js';

async function migration(sandbox: Sandbox, background: boolean) {
  const handle: CommandHandle = await sandbox.commands.run('printf ready', {
    background: true, timeoutMs: 1250,
    onStdout: async text => { text.toUpperCase(); },
  });
  const foreground: CommandResult = await sandbox.commands.run('true', { background: false });
  const omitted: CommandResult = await sandbox.commands.run('true');
  const dynamic: CommandHandle | CommandResult = await sandbox.commands.run('true', { background });
  const attached: CommandHandle = await sandbox.commands.connect(handle.pid, {
    timeoutMs: 0, onStderr: async text => { text.toLowerCase(); },
  });
  const result: CommandResult = await attached.wait();
  const output: string = attached.stdout;
  const errorOutput: string = attached.stderr;
  const exit: number | undefined = attached.exitCode;
  const error: string | undefined = attached.error;
  const identity: string = attached.executionId;
  const disconnected: void = await attached.disconnect();
  const killed: boolean = await handle.kill();
  const killedByPid: boolean = await sandbox.commands.kill(handle.pid);
  // @ts-expect-error background true returns a handle, never an immediate result
  const wrongResult: CommandResult = await sandbox.commands.run('true', { background: true });
  // @ts-expect-error foreground returns a result, not a handle
  const wrongHandle: CommandHandle = await sandbox.commands.run('true', { background: false });
  // @ts-expect-error dynamic boolean must retain the union
  const wrongDynamic: CommandResult = await sandbox.commands.run('true', { background });
  // @ts-expect-error an observation deadline belongs to run/connect, never wait
  await attached.wait({ timeoutMs: 10 });
  // @ts-expect-error request-specific timeouts are explicitly deferred
  await sandbox.commands.run('true', { requestTimeoutMs: 10 });
  // @ts-expect-error signals are explicitly deferred
  await sandbox.commands.connect(42, { signal: new AbortController().signal });
  // @ts-expect-error unsupported stdin is not silently ignored
  await sandbox.commands.run('true', { stdin: true });
  // @ts-expect-error no callback options on kill
  await sandbox.commands.kill(42, { onStdout: () => {} });
  void [foreground, omitted, dynamic, result, output, errorOutput, exit, error, identity, disconnected,
    killed, killedByPid, wrongResult, wrongHandle, wrongDynamic];
}
void migration;
