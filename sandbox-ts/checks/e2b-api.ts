// Application-source contract for the deliberately bounded compatibility API.
import { Sandbox, CommandExitError, FileType, type CommandResult, type EntryInfo } from '../src/e2b.js';

async function migration(image: string) {
  const sandbox = await Sandbox.create('base', { image, envs: { LANGUAGE: 'ts' }, timeoutMs: 90_000 });
  try {
    await sandbox.files.write('input.bin', new Uint8Array([0, 255, 10]));
    const bytes: Uint8Array = await sandbox.files.read('input.bin', { format: 'bytes' });
    const text: string = await sandbox.files.read('input.bin');
    const entries: EntryInfo[] = await sandbox.files.list('.', { depth: 1 });
    const result: CommandResult = await sandbox.commands.run('printf hello', {
      timeoutMs: 1250, background: false, stdin: false, retainOutput: true,
      onStdout: async chunk => { chunk.toUpperCase(); },
    });
    const connected = await Sandbox.connect(sandbox.sandboxId, { timeoutMs: 1501 });
    await connected.setTimeout(30_000);
    void [bytes, text, entries, result, FileType.FILE];
  } catch (error) {
    if (error instanceof CommandExitError) {
      const code: number = error.exitCode;
      const output: string = error.stdout;
      const result: CommandResult = error;
      void [code, output, result];
    } else throw error;
  } finally {
    const removed: boolean = await sandbox.kill();
    void removed;
  }
}
void migration;
