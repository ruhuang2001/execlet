import { Sandbox, type CommandHandle, type CommandResult } from 'e2b';

async function migration(sandbox: Sandbox, background: boolean) {
  const handle: CommandHandle = await sandbox.commands.run('printf ready', {
    background: true, timeoutMs: 1250,
    onStdout: async text => { text.toUpperCase(); },
  });
  const foreground: CommandResult = await sandbox.commands.run('true', {background: false});
  const omitted: CommandResult = await sandbox.commands.run('true');
  const dynamic: CommandHandle | CommandResult = await sandbox.commands.run('true', {background});
  const attached: CommandHandle = await sandbox.commands.connect(handle.pid, {
    timeoutMs: 0, onStderr: async text => { text.toLowerCase(); },
  });
  const result: CommandResult = await attached.wait();
  const output: string = attached.stdout;
  const errorOutput: string = attached.stderr;
  const exit: number | undefined = attached.exitCode;
  const error: string | undefined = attached.error;
  const disconnected: void = await attached.disconnect();
  const killed: boolean = await handle.kill();
  const killedByPid: boolean = await sandbox.commands.kill(handle.pid);
  // @ts-expect-error background true does not return a CommandResult
  const wrongResult: CommandResult = await sandbox.commands.run('true', {background: true});
  // @ts-expect-error foreground does not return a CommandHandle
  const wrongHandle: CommandHandle = await sandbox.commands.run('true', {background: false});
  void [foreground, omitted, dynamic, result, output, errorOutput, exit, error, disconnected,
    killed, killedByPid, wrongResult, wrongHandle];
}
void migration;
