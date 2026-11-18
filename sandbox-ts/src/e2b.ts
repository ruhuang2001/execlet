/** E2B 2.64.0 application-source subset. No E2B package or cloud transport. */
import { Client } from './client.js';
import { SandboxError as NativeError } from './errors.js';
import type { Event, Execution, FileEntry, Sandbox as NativeSandbox } from './types.js';

const HOME = '/home/user';
const CONNECTION = ['endpoint', 'apiKey', 'requestTimeoutMs'];

export interface ConnectionOptions { endpoint?: string; apiKey?: string; requestTimeoutMs?: number }
export interface CreateOptions extends ConnectionOptions {
  image?: string; timeoutMs?: number; envs?: Record<string, string>;
}
export interface ConnectOptions extends ConnectionOptions { timeoutMs?: number }
export interface CommandConnectOptions {
  timeoutMs?: number;
  onStdout?: (data: string) => void | Promise<void>;
  onStderr?: (data: string) => void | Promise<void>;
}
export interface CommandOptions extends CommandConnectOptions {
  envs?: Record<string, string>; cwd?: string;
  background?: boolean; stdin?: false; retainOutput?: true;
}
export interface CommandResult { stdout: string; stderr: string; exitCode: number; error?: string }
/** An observation session, not ownership of the remote process. */
export interface CommandHandle {
  readonly pid: number;
  /** tiny-sandbox extension: immutable execution identity for diagnostics/native recovery. */
  readonly executionId: string;
  readonly stdout: string;
  readonly stderr: string;
  readonly exitCode: number | undefined;
  readonly error: string | undefined;
  wait(): Promise<CommandResult>;
  disconnect(): Promise<void>;
  kill(): Promise<boolean>;
}
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
  constructor(result: CommandResult, details: ErrorDetails = {}) {
    super('Command exited with code ' + result.exitCode, {
      ...details, execution: { ...details.execution, stdout: result.stdout, stderr: result.stderr },
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
  if (error.statusCode === 401 || error.statusCode === 403) Kind = AuthenticationError;
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
    this.commands = new Commands(native, sandbox.id, requestTimeout);
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

// A Client owns one immutable endpoint and credential. Never share a gate globally
// or let one observer's abort cancel another observer's capability request.
async function requireBackground(native: Client, signal: AbortSignal): Promise<void> {
  await native.requireCapabilities(['e2b.background.v1'], { signal });
}
function commandDuration(value: number | undefined): number {
  const milliseconds = value === undefined ? 60_000 : value;
  if (!Number.isSafeInteger(milliseconds) || milliseconds < 0 || milliseconds > 9_223_372_036_854) {
    throw new InvalidArgumentError('timeoutMs must be nonnegative safe-integer milliseconds within the server duration range');
  }
  return milliseconds;
}
function commandPid(pid: number): void {
  if (!Number.isSafeInteger(pid) || pid <= 0) throw new InvalidArgumentError('pid must be a positive safe integer');
}
function callbacks(options: CommandConnectOptions): void {
  for (const cb of [options.onStdout, options.onStderr]) {
    if (cb !== undefined && typeof cb !== 'function') throw new InvalidArgumentError('output callbacks must be functions');
  }
}
function commandIdentity(execution: Execution, sandboxId: string, pid?: number): void {
  if (!execution || typeof execution.id !== 'string' || !execution.id || execution.sandbox_id !== sandboxId ||
      !Number.isSafeInteger(execution.pid) || execution.pid! <= 0 || (pid !== undefined && execution.pid !== pid) ||
      !['running', 'completed'].includes(execution.state)) {
    throw new AdapterError('command start/attach identity was not confirmed; remote work may exist', {
      code: 'start_unconfirmed', ...(execution ? { execution } : {}),
    });
  }
}
async function commandError(error: unknown, native: Client, sandboxId: string, signal: AbortSignal,
  partial: Partial<Execution> = {}): Promise<unknown> {
  if (!(error instanceof NativeError)) return error;
  const details: ErrorDetails = {
    code: error.code, statusCode: error.statusCode, cause: error,
    execution: { ...error.execution, ...partial },
    ...(error.idempotencyKey ? { idempotencyKey: error.idempotencyKey } : {}),
  };
  if (error.statusCode !== 401 && error.statusCode !== 403 && (error.statusCode === 404 || error.code === 'not_found')) {
    try {
      const sandbox = await native.getSandbox(sandboxId, { signal });
      if (sandbox.state !== 'running') return new SandboxNotFoundError('sandbox is no longer running', details);
    } catch (missing) {
      if (!(missing instanceof NativeError)) return missing;
      return translate(new NativeError(missing.code, missing.message, {
        statusCode: missing.statusCode, cause: missing, execution: details.execution as Execution,
      }));
    }
    return new NotFoundError('managed command is no longer retained', details);
  }
  return translate(new NativeError(error.code, error.message, {
    statusCode: error.statusCode, cause: error, execution: details.execution as Execution,
    ...(error.idempotencyKey ? { idempotencyKey: error.idempotencyKey } : {}),
  }));
}
async function killCommand(native: Client, sandboxId: string, executionId: string, requestTimeout: number): Promise<boolean> {
  return request(requestTimeout, async signal => {
    try {
      await requireBackground(native, signal);
      return (await native.killExecution(sandboxId, executionId, { signal })).signaled;
    } catch (error) {
      const translated = await commandError(error, native, sandboxId, signal, { id: executionId, sandbox_id: sandboxId });
      if (translated instanceof NotFoundError && !(translated instanceof SandboxNotFoundError)) return false;
      throw translated;
    }
  });
}

class Commands {
  constructor(private readonly native: Client, private readonly id: string, private readonly requestTimeout: number) {}
  run(command: string, options: CommandOptions & { background: true }): Promise<CommandHandle>;
  run(command: string, options?: CommandOptions & { background?: false }): Promise<CommandResult>;
  run(command: string, options: CommandOptions): Promise<CommandHandle | CommandResult>;
  async run(command: string, options: CommandOptions = {}): Promise<CommandHandle | CommandResult> {
    unknown(options, ['envs', 'cwd', 'timeoutMs', 'onStdout', 'onStderr', 'background', 'stdin', 'retainOutput']);
    text(command, 'command');
    const milliseconds = commandDuration(options.timeoutMs);
    const env = environment(options.envs);
    if ((options.background !== undefined && typeof options.background !== 'boolean') ||
        (options.stdin !== undefined && options.stdin !== false) ||
        (options.retainOutput !== undefined && options.retainOutput !== true)) {
      throw new InvalidArgumentError('background must be boolean; only closed stdin and retained output are supported');
    }
    if (options.cwd !== undefined) {
      text(options.cwd, 'cwd');
      if (!options.cwd.startsWith('/')) throw new InvalidArgumentError('cwd must be absolute');
    }
    callbacks(options);
    // Copy all options before the first await so retries and callbacks cannot
    // observe caller mutations. This clock includes discovery and accepted-start retries.
    const selected = { ...options };
    const wait = deadline(milliseconds);
    let started: Execution;
    try {
      await requireBackground(this.native, wait.signal);
      started = await this.native.startExecution(this.id, {
        command: ['/bin/bash', '-l', '-c', command], cwd: selected.cwd ?? HOME, timeout_ms: milliseconds,
        ...(env === undefined ? {} : { env }),
      }, { signal: wait.signal, cancelRemote: false });
      commandIdentity(started, this.id);
    } catch (error) {
      wait.close();
      throw translate(error);
    }
    const handle = new ObservedCommand(this.native, this.id, started, selected, wait, this.requestTimeout);
    return selected.background ? handle : handle.wait();
  }

  async connect(pid: number, options: CommandConnectOptions = {}): Promise<CommandHandle> {
    unknown(options, ['timeoutMs', 'onStdout', 'onStderr']);
    commandPid(pid);
    const milliseconds = commandDuration(options.timeoutMs);
    callbacks(options);
    const selected = { ...options };
    const wait = deadline(milliseconds);
    try {
      await requireBackground(this.native, wait.signal);
      const attached = await this.native.resolveCommand(this.id, pid, { signal: wait.signal });
      commandIdentity(attached?.execution, this.id, pid);
      if (!Number.isSafeInteger(attached.last_event_id) || attached.last_event_id < 0) {
        throw new AdapterError('server attach watermark was not confirmed', { code: 'incompatible_server', execution: attached.execution });
      }
      return new ObservedCommand(this.native, this.id, attached.execution, selected, wait, this.requestTimeout, attached.last_event_id);
    } catch (error) {
      try { throw await commandError(error, this.native, this.id, wait.signal); }
      finally { wait.close(); }
    }
  }

  async kill(pid: number, options: Record<string, never> = {}): Promise<boolean> {
    unknown(options);
    commandPid(pid);
    return request(this.requestTimeout, async signal => {
      let executionId: string | undefined;
      try {
        await requireBackground(this.native, signal);
        const attached = await this.native.resolveCommand(this.id, pid, { signal });
        commandIdentity(attached?.execution, this.id, pid);
        executionId = attached.execution.id;
        return (await this.native.killExecution(this.id, executionId, { signal })).signaled;
      } catch (error) {
        const translated = await commandError(error, this.native, this.id, signal,
          executionId ? { id: executionId, sandbox_id: this.id } : {});
        if (translated instanceof NotFoundError && !(translated instanceof SandboxNotFoundError)) return false;
        throw translated;
      }
    });
  }
}

class ObservedCommand implements CommandHandle {
  private readonly identity: { id: string; sandbox_id: string; pid: number };
  private readonly controller = new AbortController();
  private events: AsyncIterableIterator<Event> | undefined;
  private output = { stdout: '', stderr: '' };
  private code: number | undefined;
  private message: string | undefined;
  private settled = false;
  private readonly result: Promise<CommandResult>;
  private resolve!: (value: CommandResult) => void;
  private reject!: (error: unknown) => void;
  private readonly timeout = () => this.fail(new TimeoutError('stopped observing; remote termination is not confirmed', {
    code: 'wait_timeout', execution: this.partial(),
  }));

  constructor(private readonly native: Client, private readonly sandboxId: string, execution: Execution,
    private readonly options: CommandConnectOptions, private readonly clock: ReturnType<typeof deadline>,
    private readonly requestTimeout: number, attachCursor?: number) {
    this.identity = { id: execution.id, sandbox_id: sandboxId, pid: execution.pid! };
    this.result = new Promise((resolve, reject) => { this.resolve = resolve; this.reject = reject; });
    // Observation starts without wait(). A rejected callback can even be
    // `undefined`; preserve that value, without an unhandled-rejection window.
    void this.result.catch(() => {});
    clock.signal.addEventListener('abort', this.timeout, { once: true });
    if (clock.signal.aborted) this.timeout();
    else if (execution.output_truncated) this.fail(this.truncated(execution));
    else if (attachCursor !== undefined && execution.state === 'completed') this.complete(execution);
    else {
      this.events = native.streamExecution(sandboxId, execution.id, {
        signal: this.controller.signal, cancelRemote: false, lastEventId: attachCursor ?? 0,
      });
      void this.observe().catch(error => this.fail(error));
    }
  }

  get pid(): number { return this.identity.pid; }
  get executionId(): string { return this.identity.id; }
  get stdout(): string { return this.output.stdout; }
  get stderr(): string { return this.output.stderr; }
  get exitCode(): number | undefined { return this.code; }
  get error(): string | undefined { return this.message; }

  wait(options: Record<string, never> = {}): Promise<CommandResult> {
    unknown(options);
    return this.result;
  }
  async disconnect(options: Record<string, never> = {}): Promise<void> {
    unknown(options);
    this.fail(new AdapterError('command observation disconnected', { code: 'disconnected', execution: this.partial() }));
  }
  kill(options: Record<string, never> = {}): Promise<boolean> {
    unknown(options);
    return killCommand(this.native, this.sandboxId, this.identity.id, this.requestTimeout);
  }

  private partial(execution: Partial<Execution> = {}): Partial<Execution> {
    // A terminal native snapshot contains retained history, not this session's suffix.
    return { ...execution, ...this.identity, ...this.output };
  }
  private truncated(execution: Partial<Execution> = {}): AdapterError {
    return new AdapterError('command output was truncated', {
      code: 'output_truncated', execution: this.partial({ ...execution, output_truncated: true }),
    });
  }
  private close(): void {
    this.clock.close();
    this.clock.signal.removeEventListener('abort', this.timeout);
    this.controller.abort();
    // Never await user callbacks or the consumer task (a callback may itself
    // await disconnect). Iterator return closes an idle/suspended SSE reader.
    if (this.events?.return) void this.events.return().catch(() => {});
  }
  private fail(error: unknown): void {
    if (this.settled) return;
    this.settled = true;
    try { this.message = error instanceof Error ? error.message : String(error); }
    catch { this.message = 'command observation failed'; }
    this.reject(error);
    this.close();
  }
  private complete(execution: Execution): void {
    if (this.settled) return;
    if (execution.id !== this.executionId || execution.sandbox_id !== this.sandboxId ||
        (execution.pid !== undefined && execution.pid !== this.pid) || execution.state !== 'completed') {
      this.fail(new AdapterError('terminal execution identity was not confirmed', { code: 'incomplete', execution: this.partial() }));
      return;
    }
    if (Number.isInteger(execution.exit_code)) this.code = execution.exit_code!;
    if (execution.output_truncated) { this.fail(this.truncated(execution)); return; }
    if (execution.termination_reason !== 'exited' || !Number.isInteger(execution.exit_code)) {
      const Kind = execution.termination_reason === 'timeout' ? TimeoutError : AdapterError;
      this.fail(new Kind('command did not report an ordinary exit', {
        code: execution.termination_reason || 'incomplete', execution: this.partial(execution),
      }));
      return;
    }
    const result: CommandResult = { ...this.output, exitCode: execution.exit_code! };
    if (result.exitCode !== 0) { this.fail(new CommandExitError(result, { execution: this.partial(execution) })); return; }
    this.settled = true;
    this.resolve(result);
    this.close();
  }
  private async observe(): Promise<void> {
    try {
      for await (const event of this.events!) {
        if (this.settled) return;
        if (event.type === 'stdout' || event.type === 'stderr') {
          this.output[event.type] += event.data;
          try {
            await (event.type === 'stdout' ? this.options.onStdout : this.options.onStderr)?.(event.data);
          } catch (error) { this.fail(error); return; }
        } else if (event.type === 'truncated') { this.fail(this.truncated()); return; }
        else if (event.type === 'complete') { this.complete(event.execution); return; }
        if (this.settled) return;
      }
      if (!this.settled) this.fail(new AdapterError('command stream ended without terminal evidence', {
        code: 'stream', execution: this.partial(),
      }));
    } catch (error) {
      if (!this.settled) this.fail(await commandError(error, this.native, this.sandboxId, this.clock.signal, this.partial()));
    }
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
