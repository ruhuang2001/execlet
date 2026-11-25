import { readFrames } from './sse.js';
import { checkSignal, SandboxError, statusError, transportError } from './errors.js';
import { decodeJSON, sleep, Transport } from './http.js';
import { consume, streamEvents } from './stream.js';
import type {
  FileEntry, FileList, ListFilesOptions, ClientConfig, CompletedExecution, CreateSandboxRequest, Event, EventHandler,
  ExecuteRequest, Execution, ExecutionOptions, ExecutionOutcome, RequestOptions, Sandbox, UpdateSandboxRequest,
  Capabilities, CommandAttachment, CreateVolumeRequest, KillExecutionResponse, StreamExecutionOptions, Volume, VolumeMount,
} from './types.js';

const volumeCapability = 'volumes.v1';
const volumesPath = '/v1/volumes';
const mib = 1024 ** 2;

function validateVolume(volume: Volume): void {
  if (typeof volume?.id !== 'string' || !volume.id || !Number.isSafeInteger(volume.size_bytes) || volume.size_bytes <= 0 ||
      typeof volume.created_at !== 'string' || !volume.created_at) {
    throw new SandboxError('decode', 'invalid volume metadata');
  }
}

function volumePath(id: string): string {
  if (typeof id !== 'string' || !id || id === '.' || id === '..') {
    throw new SandboxError('invalid_request', 'sandbox: invalid volume_id');
  }
  return volumesPath + '/' + encodeURIComponent(id);
}

function validateFileEntry(entry: FileEntry): void {
  if (!entry || typeof entry.name !== 'string' || !Number.isInteger(entry.size) || !Number.isInteger(entry.mode) ||
      typeof entry.mtime !== 'string' || typeof entry.is_dir !== 'boolean' || typeof entry.type !== 'string' ||
      !Number.isInteger(entry.uid) || !Number.isInteger(entry.gid) ||
      [entry.owner, entry.group, entry.symlink_target].some(v => v !== undefined && typeof v !== 'string')) {
    throw new SandboxError('decode', 'invalid file metadata');
  }
}

function sandboxPath(id: string): string { return '/v1/sandboxes/' + encodeURIComponent(id); }
function terminalPath(sandboxID:string,id:string):string{return sandboxPath(sandboxID)+'/terminals/'+encodeURIComponent(id)}
function executionPath(sandboxID: string, executionID: string): string {
  return sandboxPath(sandboxID) + '/executions/' + encodeURIComponent(executionID);
}

function newKey(): string {
  try {
    const bytes = globalThis.crypto.getRandomValues(new Uint8Array(16));
    return Array.from(bytes, byte => byte.toString(16).padStart(2, '0')).join('');
  } catch (cause) {
    throw new SandboxError('invalid_config', 'sandbox: Web Crypto is required to generate idempotency keys', { cause });
  }
}

export function executionOutcome(execution: Execution): ExecutionOutcome {
  if (execution.state !== 'completed') {
    throw new SandboxError('invalid_state', 'sandbox: execution has not completed', { execution });
  }
  const completed = execution as CompletedExecution;
  const reason = completed.termination_reason;
  switch (reason) {
    case 'exited': return { reason: 'exited', execution: { ...completed, termination_reason: 'exited' } };
    case 'canceled': return { reason: 'canceled', execution: { ...completed, termination_reason: 'canceled' } };
    case 'timeout': return { reason: 'timeout', execution: { ...completed, termination_reason: 'timeout' } };
    case 'oom': return { reason: 'oom', execution: { ...completed, termination_reason: 'oom' } };
    case 'evicted': return { reason: 'evicted', execution: { ...completed, termination_reason: 'evicted' } };
    default: return { reason: 'unknown', terminationReason: reason, execution: completed };
  }
}

export class Client {
  private readonly transport: Transport;
  private capabilities: readonly string[] | undefined;

  constructor(config: ClientConfig = {}) { this.transport = new Transport(config); }

  async getCapabilities(options: RequestOptions = {}): Promise<Capabilities> {
    const result = await this.transport.json<Capabilities>('GET', '/v1/capabilities', true, options.signal);
    if (!Array.isArray(result?.capabilities) || result.capabilities.some(flag => typeof flag !== 'string')) {
      throw new SandboxError('decode', 'invalid capability response');
    }
    return result;
  }

  async requireCapabilities(required: readonly string[], options: RequestOptions = {}): Promise<void> {
    checkSignal(options.signal);
    if (required.length === 0) return;
    if (this.capabilities === undefined) {
      try {
        const response = await this.getCapabilities(options);
        this.capabilities = [...response.capabilities];
      } catch (cause) {
        if (!(cause instanceof SandboxError) || !([404, 405, 501].includes(cause.statusCode) ||
            (cause.statusCode === 0 && cause.code === 'decode'))) throw cause;
        throw new SandboxError('incompatible_server', 'server does not advertise required capabilities', { cause });
      }
    }
    const missing = required.filter(flag => !this.capabilities!.includes(flag));
    if (missing.length) throw new SandboxError('incompatible_server', 'server lacks ' + missing.join(', '));
  }

  async createVolume(request: CreateVolumeRequest, options: RequestOptions = {}): Promise<Volume> {
    const size = request?.size_bytes;
    if (!Number.isSafeInteger(size) || size < 16 * mib || size > 1024 ** 4 || size % mib !== 0) {
      throw new SandboxError('invalid_request', 'sandbox: size_bytes must be a whole MiB between 16 MiB and 1 TiB');
    }
    await this.requireCapabilities([volumeCapability], options);
    const volume = await this.transport.json<Volume>('POST', volumesPath, false, options.signal, { size_bytes: size });
    validateVolume(volume);
    return volume;
  }

  async listVolumes(options: RequestOptions = {}): Promise<Volume[]> {
    await this.requireCapabilities([volumeCapability], options);
    const list = await this.transport.json<{ volumes: Volume[] }>('GET', volumesPath, true, options.signal);
    if (!Array.isArray(list?.volumes)) {
      throw new SandboxError('decode', 'volumes must be an array');
    }
    for (const volume of list.volumes) validateVolume(volume);
    return list.volumes;
  }

  async getVolume(volumeID: string, options: RequestOptions = {}): Promise<Volume> {
    const path = volumePath(volumeID);
    await this.requireCapabilities([volumeCapability], options);
    const volume = await this.transport.json<Volume>('GET', path, true, options.signal);
    validateVolume(volume);
    return volume;
  }

  async deleteVolume(volumeID: string, options: RequestOptions = {}): Promise<void> {
    const path = volumePath(volumeID);
    await this.requireCapabilities([volumeCapability], options);
    await this.transport.request('DELETE', path, '503-only', options.signal);
  }

  async createSandbox(request: CreateSandboxRequest = {}, options: RequestOptions = {}): Promise<Sandbox> {
    const { mounts, ...snapshot } = { ...request, ...(request.env ? { env: { ...request.env } } : {}) };
    const volumeMounts: VolumeMount[] = [];
    if (mounts !== undefined) {
      if (!Array.isArray(mounts)) {
        throw new SandboxError('invalid_request', 'sandbox: mounts must be an array');
      }
      if (mounts.length > 8) {
        throw new SandboxError('invalid_request', 'sandbox: at most eight volume mounts are allowed');
      }
      for (const mount of mounts) {
        const { volume_id, path, read_only } = mount ?? {};
        if (typeof path === 'string' && path.length > 4096) {
          throw new SandboxError('invalid_request', 'sandbox: mount path exceeds 4096 bytes');
        }
        if (typeof volume_id !== 'string' || !volume_id || typeof path !== 'string' || !path.startsWith('/mnt/') ||
            path.slice(5).split('/').some(part => !part || part === '.' || part === '..' || /[^A-Za-z0-9_.-]/.test(part))) {
          throw new SandboxError('invalid_request', 'sandbox: mounts require a volume_id and a canonical path below /mnt');
        }
        if (read_only !== undefined && typeof read_only !== 'boolean') {
          throw new SandboxError('invalid_request', 'sandbox: mount read_only must be a boolean');
        }
        if (volumeMounts.some(prior => volume_id === prior.volume_id || path === prior.path ||
            path.startsWith(prior.path + '/') || prior.path.startsWith(path + '/'))) {
          throw new SandboxError('invalid_request', 'sandbox: mounts cannot repeat a volume or overlap');
        }
        volumeMounts.push({ volume_id, path, ...(read_only === undefined ? {} : { read_only }) });
      }
    }
    const required = [];
    if (snapshot.runtime !== undefined) required.push('sandbox.runtime.v1');
    if (snapshot.env && Object.keys(snapshot.env).length) required.push('execution.env-cwd.v1');
    if (snapshot.max_concurrent_executions !== undefined) required.push('execution.concurrent.v1');
    if (volumeMounts.length) required.push(volumeCapability);
    await this.requireCapabilities(required, options);
    return this.transport.json('POST', '/v1/sandboxes', false, options.signal,
      volumeMounts.length ? { ...snapshot, mounts: volumeMounts } : snapshot);
  }

  getSandbox(sandboxID: string, options: RequestOptions = {}): Promise<Sandbox> {
    return this.transport.json('GET', sandboxPath(sandboxID), true, options.signal);
  }

  updateSandbox(sandboxID: string, request: UpdateSandboxRequest, options: RequestOptions = {}): Promise<Sandbox> {
    return this.transport.json('PATCH', sandboxPath(sandboxID), false, options.signal, request);
  }

  async listSandboxes(options: RequestOptions = {}): Promise<Sandbox[]> {
    const list = await this.transport.json<{ sandboxes: Sandbox[] }>('GET', '/v1/sandboxes', true, options.signal);
    return list.sandboxes;
  }

  pauseSandbox(sandboxID: string, options: RequestOptions = {}): Promise<Sandbox> {
    return this.transport.json('POST', sandboxPath(sandboxID) + '/pause', true, options.signal);
  }

  resumeSandbox(sandboxID: string, options: RequestOptions = {}): Promise<Sandbox> {
    return this.transport.json('POST', sandboxPath(sandboxID) + '/resume', true, options.signal);
  }

  killSandbox(sandboxID: string, options: RequestOptions = {}): Promise<Sandbox> {
    return this.transport.json('DELETE', sandboxPath(sandboxID), true, options.signal);
  }

  async startExecution(sandboxID: string, request: ExecuteRequest, options: ExecutionOptions = {}): Promise<Execution> {
    checkSignal(options.signal);
    if (!Array.isArray(request.command) || request.command.length === 0 || request.command.some(arg => typeof arg !== 'string')) {
      throw new SandboxError('invalid_request', 'sandbox: command must be a nonempty string array');
    }
    if (request.timeout_ms !== undefined &&
        (!Number.isSafeInteger(request.timeout_ms) || request.timeout_ms < 0 || request.timeout_ms > 9_223_372_036_854 ||
         request.timeout_seconds !== undefined)) {
      throw new SandboxError('invalid_request', 'sandbox: timeout_ms must be a nonnegative integer within the server duration range, exclusive with timeout_seconds');
    }
    const keyed: ExecuteRequest = {
      ...request, command: [...request.command], ...(request.env ? { env: { ...request.env } } : {}),
      idempotency_key: request.idempotency_key || newKey(),
    };
    const required = [];
    if ((keyed.env && Object.keys(keyed.env).length) || keyed.cwd) required.push('execution.env-cwd.v1');
    if (keyed.output_tail) required.push('execution.output-tail.v1');
    if (keyed.stdin) required.push('execution.stdin.v1');
    if (keyed.timeout_ms !== undefined) required.push('e2b.background.v1');
    await this.requireCapabilities(required, options);
    try {
      return await this.start(sandboxID, keyed, options.signal);
    } catch (cause) {
      if (!options.signal?.aborted) {
        if (cause instanceof SandboxError) {
          throw new SandboxError(cause.code, cause.message, {
            statusCode: cause.statusCode, cause, idempotencyKey: keyed.idempotency_key!,
            ...(cause.execution ? { execution: cause.execution } : {}),
          });
        }
        throw cause;
      }
      if (options.cancelRemote === false) {
        throw new SandboxError('wait_timeout', 'stopped waiting; remote start may have been accepted', {
          cause, idempotencyKey: keyed.idempotency_key!,
        });
      }
      throw await this.recover(sandboxID, undefined, options.signal.reason, keyed);
    }
  }

  private start(sandboxID: string, request: ExecuteRequest, signal?: AbortSignal): Promise<Execution> {
    return this.transport.json('POST', sandboxPath(sandboxID) + '/executions', true, signal, request);
  }

  getExecution(sandboxID: string, executionID: string, options: RequestOptions = {}): Promise<Execution> {
    return this.transport.json('GET', executionPath(sandboxID, executionID), true, options.signal);
  }

  async listExecutions(sandboxID: string, options: RequestOptions = {}): Promise<Execution[]> {
    const list = await this.transport.json<{ executions: Execution[] }>(
      'GET', sandboxPath(sandboxID) + '/executions', true, options.signal,
    );
    return list.executions;
  }

  cancelExecution(sandboxID: string, executionID: string, options: RequestOptions = {}): Promise<Execution> {
    return this.transport.json('POST', executionPath(sandboxID, executionID) + '/cancel', true, options.signal);
  }

  async resolveCommand(sandboxID: string, pid: number, options: RequestOptions = {}): Promise<CommandAttachment> {
    if (!Number.isSafeInteger(pid) || pid <= 0) {
      return Promise.reject(new SandboxError('invalid_request', 'sandbox: pid must be a positive safe integer'));
    }
    await this.requireCapabilities(['e2b.background.v1'], options);
    return this.transport.json('GET', sandboxPath(sandboxID) + '/commands/' + pid, true, options.signal);
  }

  async killExecution(sandboxID: string, executionID: string, options: RequestOptions = {}): Promise<KillExecutionResponse> {
    checkSignal(options.signal);
    await this.requireCapabilities(['e2b.background.v1'], options);
    const identity = { id: executionID, sandbox_id: sandboxID };
    try {
      const result = await this.transport.json<KillExecutionResponse>(
        'POST', executionPath(sandboxID, executionID) + '/kill', false, options.signal,
      );
      if (typeof result?.signaled !== 'boolean' || result.execution?.state !== 'completed' ||
          result.execution.id !== executionID || result.execution.sandbox_id !== sandboxID ||
          typeof result.execution.termination_reason !== 'string' || !result.execution.termination_reason ||
          (result.signaled && result.execution.termination_reason !== 'canceled') ||
          !(result.execution.exit_code === null || Number.isInteger(result.execution.exit_code))) {
        throw new SandboxError('backend_error', 'sandbox: kill termination was not confirmed', { execution: identity });
      }
      return result;
    } catch (cause) {
      if (cause instanceof SandboxError) {
        const uncertain = cause.statusCode >= 500 || ['transport', 'decode', 'canceled'].includes(cause.code);
        throw new SandboxError(uncertain ? 'backend_error' : cause.code,
          uncertain ? 'sandbox: kill outcome is unconfirmed; ' + cause.message : cause.message, {
          cause, statusCode: cause.statusCode, execution: { ...cause.execution, ...identity },
        });
      }
      throw new SandboxError('backend_error', 'sandbox: kill outcome is unconfirmed', { cause, execution: identity });
    }
  }

  streamExecution(sandboxID: string, executionID: string, options?: StreamExecutionOptions): AsyncIterableIterator<Event>;
  streamExecution(sandboxID: string, executionID: string, onEvent: EventHandler, options?: StreamExecutionOptions): Promise<Execution>;
  streamExecution(sandboxID: string, executionID: string,
    handlerOrOptions: EventHandler | StreamExecutionOptions = {}, options: StreamExecutionOptions = {}): AsyncIterableIterator<Event> | Promise<Execution> {
    const opts = typeof handlerOrOptions === 'function' ? options : handlerOrOptions;
    const events = streamEvents(this.transport, executionPath(sandboxID, executionID) + '/events', opts,
      () => opts.cancelRemote === false
        ? Promise.resolve(new SandboxError('wait_timeout', 'stopped waiting; remote execution was not canceled', {
          execution: { id: executionID, sandbox_id: sandboxID }, cause: opts.signal?.reason,
        }))
        : this.recover(sandboxID, executionID, opts.signal?.reason));
    return typeof handlerOrOptions === 'function' ? consume(events, handlerOrOptions) : events;
  }

  async exec(sandboxID: string, command: readonly string[], options: RequestOptions = {}): Promise<ExecutionOutcome> {
    const started = await this.startExecution(sandboxID, { command }, options);
    return executionOutcome(await consume(this.streamExecution(sandboxID, started.id, options)));
  }

  async execStream(sandboxID: string, command: readonly string[], onEvent: EventHandler,
    options: RequestOptions = {}): Promise<ExecutionOutcome> {
    const started = await this.startExecution(sandboxID, { command }, options);
    return executionOutcome(await this.streamExecution(sandboxID, started.id, onEvent, options));
  }

  async statFile(sandboxID: string, path: string, options: RequestOptions = {}): Promise<FileEntry> {
    await this.requireCapabilities(['files.stat.v1'], options);
    const entry = await this.transport.json<FileEntry>(
      'GET',
      sandboxPath(sandboxID) + '/files/stat?path=' + encodeURIComponent(path),
      true,
      options.signal
    );
    validateFileEntry(entry);
    return entry;
  }

  async destroySandbox(sandboxID: string, options: RequestOptions = {}): Promise<Sandbox> {
    await this.requireCapabilities(['sandbox.purge.v1'], options);
    return this.transport.json('DELETE', sandboxPath(sandboxID) + '?purge=true', false, options.signal);
  }

  async openTerminal(
    sandboxID: string,
    options: RequestOptions & { rows?: number; cols?: number; command?: string[]; cwd?: string } = {}
  ): Promise<string> {
    await this.requireCapabilities(['terminals.v1'], options);
    const { signal, ...body } = options;
    const result = await this.transport.json<{ terminal_id: string }>(
      'POST',
      sandboxPath(sandboxID) + '/terminals',
      false,
      signal,
      body
    );
    if (typeof result?.terminal_id !== 'string' || !result.terminal_id)
      throw new SandboxError('decode', 'missing terminal_id');
    return result.terminal_id;
  }

  async terminalInput(sandboxID: string, id: string, data: Uint8Array, options: RequestOptions = {}): Promise<void> {
    await this.requireCapabilities(['terminals.v1'], options);
    await this.transport.request('POST', terminalPath(sandboxID, id) + '/input', false, options.signal, data);
  }

  async resizeTerminal(
    sandboxID: string,
    id: string,
    rows: number,
    cols: number,
    options: RequestOptions = {}
  ): Promise<void> {
    await this.requireCapabilities(['terminals.v1'], options);
    await this.transport.request(
      'POST',
      terminalPath(sandboxID, id) + '/resize',
      true,
      options.signal,
      JSON.stringify({ rows, cols })
    );
  }

  async closeTerminal(sandboxID: string, id: string, options: RequestOptions = {}): Promise<void> {
    await this.requireCapabilities(['terminals.v1'], options);
    await this.transport.request('DELETE', terminalPath(sandboxID, id), false, options.signal);
  }

  async *streamTerminal(
    sandboxID: string,
    id: string,
    options: RequestOptions & { offset?: number } = {}
  ): AsyncGenerator<Uint8Array> {
    await this.requireCapabilities(['terminals.v1'], options);
    let offset = options.offset ?? 0;
    try {
      const response = await this.transport.open(terminalPath(sandboxID, id) + '/events', {
        method: 'GET',
        headers: { Accept: 'text/event-stream', 'Last-Event-ID': String(offset) },
        ...(options.signal ? { signal: options.signal } : {})
      });
      if (!response.ok) throw statusError(response.status, await response.text());
      if (!response.body) throw new SandboxError('stream', 'missing terminal body');
      for await (const frame of readFrames(response.body)) {
        if (frame.type === 'closed') return;
        if (frame.type !== 'stdout') continue;
        const next = Number(frame.id);
        if (!Number.isSafeInteger(next) || next <= 0) throw new SandboxError('stream', 'invalid terminal offset');
        if (next <= offset) continue;
        const payload = decodeJSON<{ data: string }>(new TextEncoder().encode(frame.data));
        if (typeof payload?.data !== 'string') throw new SandboxError('stream', 'invalid terminal output');
        offset = next;
        yield Uint8Array.from(atob(payload.data), (c) => c.charCodeAt(0));
      }
      throw new SandboxError('transport', 'terminal stream closed without a closed event');
    } catch (error) {
      checkSignal(options.signal);
      throw transportError(error);
    }
  }

  async listFiles(sandboxID: string, path: string, options: ListFilesOptions = {}): Promise<FileList> {
    const query = new URLSearchParams({ path });
    if (options.limit !== undefined) query.set('limit', String(options.limit));
    if (options.cursor !== undefined) query.set('cursor', options.cursor);
    await this.requireCapabilities(['files.list.v1'], options);
    const page = await this.transport.json<FileList>('GET', sandboxPath(sandboxID) + '/files/list?' + query, true, options.signal);
    if (!Array.isArray(page?.entries) || (page.next_cursor !== undefined && typeof page.next_cursor !== 'string')) {
      throw new SandboxError('decode', 'invalid file list');
    }
    for (const entry of page.entries) {
      validateFileEntry(entry);
    }
    return page;
  }

  async readArchive(sandboxID: string, path: string, options: RequestOptions = {}): Promise<Uint8Array> {
    await this.requireCapabilities(['files.archive.v1'], options);
    return this.transport.request('GET', sandboxPath(sandboxID) + '/files/archive?path=' + encodeURIComponent(path), true, options.signal);
  }

  async writeArchive(sandboxID: string, path: string, data: Uint8Array, options: RequestOptions = {}): Promise<void> {
    const bytes = new Uint8Array(data);
    await this.requireCapabilities(['files.archive.v1'], options);
    await this.transport.request('PUT', sandboxPath(sandboxID) + '/files/archive?path=' + encodeURIComponent(path), false,
      options.signal, bytes);
  }

  readFile(sandboxID: string, path: string, options: RequestOptions = {}): Promise<Uint8Array> {
    return this.transport.request('GET', sandboxPath(sandboxID) + '/files?path=' + encodeURIComponent(path), true, options.signal);
  }

  async writeFile(sandboxID: string, path: string, data: Uint8Array, options: RequestOptions = {}): Promise<FileEntry | undefined> {
    const { status, bytes } = await this.transport.requestStatus('PUT', sandboxPath(sandboxID) + '/files?path=' + encodeURIComponent(path), true,
      options.signal, new Uint8Array(data));
    if (status === 204) return undefined;
    const written = decodeJSON<FileEntry>(bytes);
    validateFileEntry(written);
    return written;
  }

  private async recover(sandboxID: string, executionID: string | undefined, cause: unknown,
    request?: ExecuteRequest): Promise<SandboxError> {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), 5000);
    let execution: Execution | Pick<Execution, 'id' | 'sandbox_id'> | undefined = executionID === undefined ?
      undefined : { id: executionID, sandbox_id: sandboxID };
    const key = request?.idempotency_key;
    try {
      if (request) {
        const recovered = await this.start(sandboxID, request, controller.signal);
        if (typeof recovered?.id !== 'string' || !recovered.id || recovered.sandbox_id !== sandboxID) {
          throw new Error('recovered start did not confirm execution identity');
        }
        execution = recovered;
      }
      if (!execution) throw new Error('no execution identity');
      const expectedID = execution.id;
      let snapshot = await this.transport.json<Execution>('POST', executionPath(sandboxID, expectedID) + '/cancel', false, controller.signal);
      for (;;) {
        if (snapshot?.id !== expectedID || snapshot.sandbox_id !== sandboxID) {
          throw new Error('cancellation snapshot identifies another execution');
        }
        execution = snapshot;
        if (snapshot.state !== 'running') break;
        await sleep(50, controller.signal);
        snapshot = await this.getExecution(sandboxID, expectedID, { signal: controller.signal });
      }
      const reason = snapshot.termination_reason;
      const knownReasons = ['canceled', 'timeout', 'oom', 'evicted', 'exited'];
      if (snapshot.state !== 'completed' || typeof reason !== 'string' || !knownReasons.includes(reason) ||
          (reason === 'exited' && !Number.isInteger(snapshot.exit_code))) {
        throw new Error('server cancellation response did not confirm terminal outcome');
      }
      return new SandboxError('canceled', 'sandbox: caller aborted; remote execution is terminal', {
        execution, cause, ...(key ? { idempotencyKey: key } : {}),
      });
    } catch (recoveryCause) {
      return new SandboxError('cancel_unconfirmed', 'sandbox: could not confirm remote execution stopped', {
        cause: recoveryCause, ...(execution ? { execution } : {}), ...(key ? { idempotencyKey: key } : {}),
      });
    } finally { clearTimeout(timer); }
  }
}
