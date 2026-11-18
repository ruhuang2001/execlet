/** E2B 2.64.0 application-source subset. No E2B package or cloud transport. */
import { Client } from './client.js';
import { SandboxError as NativeError } from './errors.js';
import type { Execution, FileEntry, Sandbox as NativeSandbox } from './types.js';

const HOME = '/home/user';
const CONNECTION = ['endpoint', 'apiKey', 'requestTimeoutMs'];

export interface ConnectionOptions { endpoint?: string; apiKey?: string; requestTimeoutMs?: number }
export interface CreateOptions extends ConnectionOptions {
  image?: string; timeoutMs?: number; envs?: Record<string, string>;
}
export interface ConnectOptions extends ConnectionOptions { timeoutMs?: number }
export interface CommandOptions {
  envs?: Record<string, string>; cwd?: string; timeoutMs?: number;
  onStdout?: (data: string) => void | Promise<void>;
  onStderr?: (data: string) => void | Promise<void>;
  background?: false; stdin?: false; retainOutput?: true;
}
export interface CommandResult { stdout: string; stderr: string; exitCode: number; error?: string }
export enum FileType { FILE = 'file', DIR = 'dir', SYMLINK = 'symlink' }
export interface WriteInfo { name: string; path: string; type?: FileType; metadata?: Record<string, string> }
export interface EntryInfo extends WriteInfo {
  size: number; mode: number; permissions: string; owner: string; group: string;
  modifiedTime: Date; symlinkTarget?: string;
}
export interface SandboxInfo { sandboxId: string; state: string; templateId: string; endAt: Date }

interface ErrorDetails {
  code?: string; statusCode?: number; execution?: Partial<Execution>;
  idempotencyKey?: string; cause?: unknown;
}
export class SandboxError extends Error {
  readonly code: string;
  readonly statusCode: number | undefined;
  readonly executionId: string | undefined;
  readonly idempotencyKey: string | undefined;
  readonly terminationReason: string | null | undefined;
  readonly outputTruncated: boolean;
  readonly stdout: string;
  readonly stderr: string;
  constructor(message: string, details: ErrorDetails = {}) {
    super(message, { cause: details.cause });
    this.name = new.target.name;
    this.code = details.code ?? '';
    this.statusCode = details.statusCode;
    this.executionId = details.execution?.id;
    this.idempotencyKey = details.idempotencyKey;
    this.terminationReason = details.execution?.termination_reason;
    this.outputTruncated = details.execution?.output_truncated ?? false;
    this.stdout = details.execution?.stdout ?? '';
    this.stderr = details.execution?.stderr ?? '';
  }
}
export class NotFoundError extends SandboxError {}
export class SandboxNotFoundError extends NotFoundError {}
export class FileNotFoundError extends NotFoundError {}
export class AuthenticationError extends SandboxError {}
export class InvalidArgumentError extends SandboxError {}
export class TimeoutError extends SandboxError {}
/** Diagnostic extension for native limits and uncertain outcomes, not an exit. */
export class AdapterError extends SandboxError {}
export class CommandExitError extends SandboxError {
  readonly exitCode: number;
  readonly error?: string;
  constructor(result: CommandResult) {
    super('Command exited with code ' + result.exitCode, {
      execution: { stdout: result.stdout, stderr: result.stderr },
    });
    this.exitCode = result.exitCode;
    if (result.error !== undefined) this.error = result.error;
  }
}

function unknown(options: object, allowed: readonly string[] = []): void {
  if (!options || typeof options !== 'object' || Array.isArray(options)) {
    throw new InvalidArgumentError('options must be an object');
  }
  const extra = Object.keys(options).filter(key => !allowed.includes(key));
  if (extra.length) throw new InvalidArgumentError('unsupported options: ' + extra.join(', '));
}
function text(value: unknown, name: string): asserts value is string {
  if (typeof value !== 'string' || value.includes('\0') || new TextDecoder().decode(new TextEncoder().encode(value)) !== value) {
    throw new InvalidArgumentError(name + ' must be UTF-8 text without NUL');
  }
}
function duration(value: number, lifetime = false): number {
  if (typeof value !== 'number' || !Number.isFinite(value) || value < 0 || (lifetime && value <= 0)) {
    throw new InvalidArgumentError('timeout must be finite ' + (lifetime ? 'positive' : 'nonnegative') + ' milliseconds');
  }
  const seconds = Math.ceil(value / 1000);
  if (lifetime && seconds > 9_223_372_036) throw new InvalidArgumentError('lifetime exceeds server duration range');
  return lifetime ? seconds : value;
}
function environment(value: Record<string, string> | undefined): Record<string, string> | undefined {
  if (value === undefined) return undefined;
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new InvalidArgumentError('envs must be a string mapping');
  for (const [key, item] of Object.entries(value)) {
    text(key, 'environment name'); text(item, 'environment value');
    if (!key || key.includes('=')) throw new InvalidArgumentError('invalid environment name');
  }
  return { ...value };
}
function guestPath(value: string): string {
  text(value, 'path');
  if (!value || value.split('/').includes('..')) throw new InvalidArgumentError('path must be nonempty without parent traversal');
  const absolute = value.startsWith('/') ? value : HOME + '/' + value;
  const path = '/' + absolute.split('/').filter(part => part && part !== '.').join('/');
  if (new TextEncoder().encode(path).length > 4096) throw new InvalidArgumentError('path exceeds 4096 bytes');
  return path;
}
function translate(error: unknown, file = false): unknown {
  if (!(error instanceof NativeError)) return error;
  let Kind: typeof SandboxError = AdapterError;
  if (error.statusCode === 401) Kind = AuthenticationError;
  else if (error.code === 'wait_timeout' || error.code === 'canceled') Kind = TimeoutError;
  else if (error.statusCode === 404 || error.code === 'not_found') Kind = file ? FileNotFoundError : SandboxNotFoundError;
  else if (error.code === 'invalid_state') Kind = SandboxNotFoundError;
  else if (error.statusCode === 400 || error.code === 'invalid_config' || error.code === 'invalid_request') Kind = InvalidArgumentError;
  return new Kind(error.message, {
    code: error.code, statusCode: error.statusCode, cause: error,
    ...(error.execution ? { execution: error.execution } : {}),
    ...(error.idempotencyKey ? { idempotencyKey: error.idempotencyKey } : {}),
  });
}
function client(options: ConnectionOptions): { native: Client; requestTimeout: number } {
  for (const key of ['endpoint', 'apiKey'] as const) if (options[key] !== undefined) text(options[key], key);
  const requestTimeout = duration(options.requestTimeoutMs ?? 30_000);
  if (requestTimeout === 0) throw new InvalidArgumentError('requestTimeoutMs must be positive');
  try {
    return { native: new Client({
      ...(options.endpoint === undefined ? {} : { domain: options.endpoint }),
      ...(options.apiKey === undefined ? {} : { apiKey: options.apiKey }),
    }), requestTimeout };
  } catch (error) { throw translate(error); }
}

/** One total monotonic deadline; long delays must not overflow setTimeout. */
function deadline(milliseconds: number): { signal: AbortSignal; close: () => void } {
  const controller = new AbortController();
  const until = performance.now() + milliseconds;
  let timer: ReturnType<typeof setTimeout> | undefined;
  const tick = () => {
    const left = until - performance.now();
    if (left <= 0) controller.abort();
    else timer = setTimeout(tick, Math.min(left, 2_147_483_647));
  };
  if (milliseconds > 0) tick();
  return { signal: controller.signal, close: () => clearTimeout(timer) };
}
async function request<T>(milliseconds: number, operation: (signal: AbortSignal) => Promise<T>): Promise<T> {
  const wait = deadline(milliseconds);
  try { return await operation(wait.signal); }
  catch (error) { throw translate(error); }
  finally { wait.close(); }
}
function date(value: unknown): Date {
  const parsed = typeof value === 'string' ? new Date(value) : new Date(NaN);
  if (!Number.isFinite(parsed.getTime())) throw new AdapterError('missing server timestamp', { code: 'incompatible_server' });
  return parsed;
}

export class Sandbox {
  readonly sandboxId: string;
  readonly commands: Commands;
  readonly files: Files;
  private constructor(sandbox: NativeSandbox, private readonly native: Client, private readonly requestTimeout: number) {
    this.sandboxId = sandbox.id;
    this.commands = new Commands(native, sandbox.id);
    this.files = new Files(native, sandbox.id, requestTimeout);
  }

  static create(options?: CreateOptions): Promise<Sandbox>;
  static create(template: string, options?: CreateOptions): Promise<Sandbox>;
  static async create(templateOrOptions: string | CreateOptions = {}, options: CreateOptions = {}): Promise<Sandbox> {
    const opts = typeof templateOrOptions === 'string' ? options : templateOrOptions;
    unknown(opts, [...CONNECTION, 'image', 'timeoutMs', 'envs']);
    if (typeof templateOrOptions === 'string' && templateOrOptions !== 'base') {
      throw new InvalidArgumentError('only the local base image profile is supported');
    }
    const seconds = duration(opts.timeoutMs ?? 300_000, true);
    const env = environment(opts.envs);
    const vars = (globalThis as { process?: { env?: Record<string, string | undefined> } }).process?.env;
    const image = opts.image ?? vars?.SANDBOX_E2B_IMAGE;
    if (typeof image !== 'string' || !/^(?:[^\s]+@)?sha256:[a-f0-9]{64}$/.test(image)) {
      throw new InvalidArgumentError('image or SANDBOX_E2B_IMAGE must pin a compatibility image digest');
    }
    const { native, requestTimeout } = client(opts);
    const sandbox = await request(requestTimeout, signal => native.createSandbox({
      image, timeout_seconds: seconds, timeout_action: 'kill', idle_timeout_seconds: 0,
      ...(env === undefined ? {} : { env }),
    }, { signal }));
    return new Sandbox(sandbox, native, requestTimeout);
  }

  static async connect(sandboxId: string, options: ConnectOptions = {}): Promise<Sandbox> {
    unknown(options, [...CONNECTION, 'timeoutMs']);
    text(sandboxId, 'sandboxId');
    const seconds = duration(options.timeoutMs ?? 300_000, true);
    const { native, requestTimeout } = client(options);
    const sandbox = await request(requestTimeout, async signal => {
      const current = await native.getSandbox(sandboxId, { signal });
      if (current.state !== 'running') throw new SandboxNotFoundError('connect requires a running sandbox');
      return native.updateSandbox(sandboxId, { extend_seconds: seconds }, { signal });
    });
    return new Sandbox(sandbox, native, requestTimeout);
  }

  static async getInfo(sandboxId: string, options: ConnectionOptions = {}): Promise<SandboxInfo> {
    unknown(options, CONNECTION); text(sandboxId, 'sandboxId');
    const { native, requestTimeout } = client(options);
    const value = await request(requestTimeout, signal => native.getSandbox(sandboxId, { signal }));
    if (value.state === 'killed') throw new SandboxNotFoundError('sandbox is no longer running');
    return { sandboxId: value.id, state: value.state, templateId: 'base', endAt: date(value.expires_at) };
  }

  async setTimeout(timeoutMs: number, options: Record<string, never> = {}): Promise<void> {
    unknown(options);
    const seconds = duration(timeoutMs, true);
    await request(this.requestTimeout, async signal => {
      if ((await this.native.getSandbox(this.sandboxId, { signal })).state !== 'running') {
        throw new SandboxNotFoundError('setTimeout requires a running sandbox');
      }
      await this.native.updateSandbox(this.sandboxId, { timeout_seconds: seconds }, { signal });
    });
  }

  async kill(options: Record<string, never> = {}): Promise<boolean> {
    unknown(options);
    try {
      return await request(this.requestTimeout, async signal => {
        const current = await this.native.getSandbox(this.sandboxId, { signal });
        // Repeat DELETE must still confirm removal of a stored killed record.
        await this.native.killSandbox(this.sandboxId, { signal });
        return current.state !== 'killed';
      });
    } catch (error) {
      if (error instanceof SandboxNotFoundError) return false;
      throw error;
    }
  }
}

class Commands {
  constructor(private readonly native: Client, private readonly id: string) {}
  async run(command: string, options: CommandOptions = {}): Promise<CommandResult> {
    unknown(options, ['envs', 'cwd', 'timeoutMs', 'onStdout', 'onStderr', 'background', 'stdin', 'retainOutput']);
    text(command, 'command');
    const milliseconds = duration(options.timeoutMs ?? 60_000);
    const env = environment(options.envs);
    if ((options.background !== undefined && options.background !== false) ||
        (options.stdin !== undefined && options.stdin !== false) ||
        (options.retainOutput !== undefined && options.retainOutput !== true)) {
      throw new InvalidArgumentError('only foreground, closed stdin and retained output are supported');
    }
    if (options.cwd !== undefined) {
      text(options.cwd, 'cwd');
      if (!options.cwd.startsWith('/')) throw new InvalidArgumentError('cwd must be absolute');
    }
    for (const cb of [options.onStdout, options.onStderr]) {
      if (cb !== undefined && typeof cb !== 'function') throw new InvalidArgumentError('output callbacks must be functions');
    }
    const wait = deadline(milliseconds);
    const partial: Partial<Execution> = { sandbox_id: this.id, stdout: '', stderr: '' };
    let callbackFailed = false;
    let callbackError: unknown;
    let execution: Execution;
    try {
      const started = await this.native.startExecution(this.id, {
        command: ['/bin/bash', '-l', '-c', command], cwd: options.cwd ?? HOME,
        ...(env === undefined ? {} : { env }),
      }, { signal: wait.signal, cancelRemote: false });
      partial.id = started.id;
      execution = await this.native.streamExecution(this.id, started.id, async event => {
        try {
          if (event.type === 'stdout') {
            partial.stdout += event.data; await options.onStdout?.(event.data);
          } else if (event.type === 'stderr') {
            partial.stderr += event.data; await options.onStderr?.(event.data);
          } else if (event.type === 'truncated') {
            partial.output_truncated = true;
            throw new AdapterError('command output was truncated', { code: 'output_truncated', execution: partial });
          }
        } catch (error) { callbackFailed = true; callbackError = error; throw error; }
      }, { signal: wait.signal, cancelRemote: false });
    } catch (error) {
      if (callbackFailed) throw callbackError;
      if (error instanceof NativeError) {
        throw translate(new NativeError(error.code, error.message, {
          statusCode: error.statusCode, cause: error, execution: { ...partial, ...error.execution } as Execution,
          ...(error.idempotencyKey ? { idempotencyKey: error.idempotencyKey } : {}),
        }));
      }
      throw error;
    } finally { wait.close(); }
    if (execution.output_truncated) throw new AdapterError('command output was truncated', { code: 'output_truncated', execution });
    if (execution.id !== partial.id || execution.sandbox_id !== this.id ||
        execution.termination_reason !== 'exited' || !Number.isInteger(execution.exit_code) ||
        typeof execution.stdout !== 'string' || typeof execution.stderr !== 'string') {
      throw new AdapterError('command did not report an ordinary exit', { code: execution.termination_reason || 'incomplete', execution });
    }
    const result: CommandResult = { exitCode: execution.exit_code!, stdout: execution.stdout, stderr: execution.stderr };
    if (result.exitCode !== 0) throw new CommandExitError(result);
    return result;
  }
}

function entry(raw: FileEntry, path: string): EntryInfo {
  const extra = raw as FileEntry & Record<string, unknown>;
  if (typeof extra.owner !== 'string' || typeof extra.group !== 'string' ||
      !['file', 'dir', 'symlink', 'other'].includes(String(extra.type))) {
    throw new AdapterError('guest metadata is unavailable', { code: 'incompatible_server' });
  }
  const bits = 'rwxrwxrwx';
  const permissions = Array.from({ length: 9 }, (_, i) => (raw.mode & (1 << (8 - i))) ? bits[i]! : '-');
  if (raw.mode & 0o4000) permissions[2] = raw.mode & 0o100 ? 's' : 'S';
  if (raw.mode & 0o2000) permissions[5] = raw.mode & 0o010 ? 's' : 'S';
  if (raw.mode & 0o1000) permissions[8] = raw.mode & 0o001 ? 't' : 'T';
  return {
    name: raw.name, path: (path === '/' ? '' : path) + '/' + raw.name,
    ...(extra.type === 'other' ? {} : { type: extra.type as FileType }),
    size: raw.size, mode: raw.mode, permissions: permissions.join(''),
    owner: extra.owner, group: extra.group, modifiedTime: date(raw.mtime),
    ...(typeof extra.symlink_target === 'string' ? { symlinkTarget: extra.symlink_target } : {}),
  };
}
class Files {
  constructor(private readonly native: Client, private readonly id: string, private readonly requestTimeout: number) {}
  private async call<T>(operation: () => Promise<T>, signal: AbortSignal): Promise<T> {
    try { return await operation(); }
    catch (error) {
      if (error instanceof NativeError && error.statusCode === 404) {
        const current = await this.native.getSandbox(this.id, { signal });
        if (current.state !== 'running') throw new SandboxNotFoundError('sandbox is no longer running');
      }
      throw translate(error, true);
    }
  }
  async write(path: string, data: string | Uint8Array, options: Record<string, never> = {}): Promise<WriteInfo> {
    unknown(options);
    const target = guestPath(path);
    if (typeof data !== 'string' && !(data instanceof Uint8Array)) throw new InvalidArgumentError('data must be string or Uint8Array');
    if (typeof data === 'string' && new TextDecoder().decode(new TextEncoder().encode(data)) !== data) {
      throw new InvalidArgumentError('text data must be UTF-8');
    }
    const payload = typeof data === 'string' ? new TextEncoder().encode(data) : new Uint8Array(data);
    if (payload.length > 10 << 20) throw new AdapterError('file exceeds 10 MiB', { code: 'payload_too_large' });
    return request(this.requestTimeout, async signal => {
      await this.call(() => this.native.writeFile(this.id, target, payload, { signal }), signal);
      const split = target.lastIndexOf('/');
      const parent = target.slice(0, split) || '/';
      const name = target.slice(split + 1);
      const found = (await this.listAll(parent, signal)).find(item => item.name === name);
      if (!found || found.type !== FileType.FILE) throw new AdapterError('write metadata was not confirmed', { code: 'write_unconfirmed' });
      return { name: found.name, path: found.path, type: found.type };
    });
  }
  read(path: string, options?: { format?: 'text' }): Promise<string>;
  read(path: string, options: { format: 'bytes' }): Promise<Uint8Array>;
  async read(path: string, options: { format?: 'text' | 'bytes' } = {}): Promise<string | Uint8Array> {
    unknown(options, ['format']);
    const target = guestPath(path);
    if (options.format !== undefined && !['text', 'bytes'].includes(options.format)) throw new InvalidArgumentError('format must be text or bytes');
    return request(this.requestTimeout, async signal => {
      const data = await this.call(() => this.native.readFile(this.id, target, { signal }), signal);
      return options.format === 'bytes' ? data : new TextDecoder().decode(data);
    });
  }
  list(path: string, options: { depth?: 1 } = {}): Promise<EntryInfo[]> {
    unknown(options, ['depth']);
    const target = guestPath(path);
    if (options.depth !== undefined && options.depth !== 1) throw new InvalidArgumentError('only depth 1 is supported');
    return request(this.requestTimeout, signal => this.listAll(target, signal));
  }
  private async listAll(path: string, signal: AbortSignal): Promise<EntryInfo[]> {
    const entries: EntryInfo[] = [];
    const seen = new Set<string>();
    let cursor = '';
    for (;;) {
      const page = await this.call(() => this.native.listFiles(this.id, path, { limit: 1000, cursor, signal }), signal);
      entries.push(...page.entries.map(item => entry(item, path)));
      if (entries.length > 10000) throw new AdapterError('directory exceeds 10000 entries', { code: 'payload_too_large' });
      cursor = page.next_cursor ?? '';
      if (!cursor) return entries;
      if (seen.has(cursor)) throw new AdapterError('directory cursor repeated', { code: 'incompatible_server' });
      seen.add(cursor);
    }
  }
}
