import { checkSignal, SandboxError } from './errors.js';
import { Transport } from './http.js';
import { consume, streamEvents } from './stream.js';
import type {
  FileList, ListFilesOptions, ClientConfig, CompletedExecution, CreateSandboxRequest, Event, EventHandler,
  ExecuteRequest, Execution, ExecutionOptions, ExecutionOutcome, RequestOptions, Sandbox, UpdateSandboxRequest,
  Capabilities, CommandAttachment, KillExecutionResponse, StreamExecutionOptions,
} from './types.js';

function sandboxPath(id: string): string { return '/v1/sandboxes/' + encodeURIComponent(id); }
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

/** Convert completed execution data; nonzero exits and server deadlines resolve. */
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

/** One HTTP endpoint, no global deadline and no runtime dependencies. */
export class Client {
  private readonly transport: Transport;
  private capabilities: readonly string[] | undefined;

  constructor(config: ClientConfig = {}) { this.transport = new Transport(config); }

  /** Authenticated feature discovery; basic legacy calls need no capabilities. */
  getCapabilities(options: RequestOptions = {}): Promise<Capabilities> {
    return this.transport.json('GET', '/v1/capabilities', true, options.signal);
  }

  async requireCapabilities(required: readonly string[], options: RequestOptions = {}): Promise<void> {
    checkSignal(options.signal);
    if (required.length === 0) return;
    if (this.capabilities === undefined) {
      try {
        const response = await this.getCapabilities(options);
        if (!Array.isArray(response?.capabilities) || response.capabilities.some(v => typeof v !== 'string')) {
          throw new SandboxError('decode', 'invalid capability response');
        }
        this.capabilities = [...response.capabilities];
      } catch (cause) {
        if (!(cause instanceof SandboxError) || [401, 403].includes(cause.statusCode) ||
            (cause.code !== 'decode' && ![404, 405, 501].includes(cause.statusCode))) throw cause;
        throw new SandboxError('incompatible_server', 'server does not advertise required capabilities', { cause });
      }
    }
    const missing = required.filter(flag => !this.capabilities!.includes(flag));
    if (missing.length) throw new SandboxError('incompatible_server', 'server lacks ' + missing.join(', '));
  }

  async createSandbox(request: CreateSandboxRequest = {}, options: RequestOptions = {}): Promise<Sandbox> {
    const snapshot = { ...request, ...(request.env ? { env: { ...request.env } } : {}) };
    const required = [];
    if (snapshot.env && Object.keys(snapshot.env).length) required.push('execution.env-cwd.v1');
    if (snapshot.max_concurrent_executions !== undefined) required.push('execution.concurrent.v1');
    await this.requireCapabilities(required, options);
    return this.transport.json('POST', '/v1/sandboxes', false, options.signal, snapshot);
  }

  getSandbox(sandboxID: string, options: RequestOptions = {}): Promise<Sandbox> {
    return this.transport.json('GET', sandboxPath(sandboxID), true, options.signal);
  }

  /** Relative lifetime updates are sent once, never replayed after uncertainty. */
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
    // An already aborted start sends nothing, including recovery requests.
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

  /** Returns the server snapshot; state may still be running during removal. */
  cancelExecution(sandboxID: string, executionID: string, options: RequestOptions = {}): Promise<Execution> {
    return this.transport.json('POST', executionPath(sandboxID, executionID) + '/cancel', true, options.signal);
  }

  /** Resolve the newest retained managed PID incarnation without renewing sandbox lifetime. */
  async resolveCommand(sandboxID: string, pid: number, options: RequestOptions = {}): Promise<CommandAttachment> {
    if (!Number.isSafeInteger(pid) || pid <= 0) {
      return Promise.reject(new SandboxError('invalid_request', 'sandbox: pid must be a positive safe integer'));
    }
    await this.requireCapabilities(['e2b.background.v1'], options);
    return this.transport.json('GET', sandboxPath(sandboxID) + '/commands/' + pid, true, options.signal);
  }

  /** Immediate managed group kill. Never retry an uncertain acknowledgement as a false result. */
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
          !(result.execution.exit_code === null || Number.isInteger(result.execution.exit_code))) {
        throw new SandboxError('backend_error', 'sandbox: kill termination was not confirmed', { execution: identity });
      }
      return result;
    } catch (cause) {
      if (cause instanceof SandboxError) {
        // A lost response may follow a successful signal. Replaying it could
        // turn uncertainty into an apparently definitive already-done false.
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

  listFiles(sandboxID: string, path: string, options: ListFilesOptions = {}): Promise<FileList> {
    const query = new URLSearchParams({ path });
    if (options.limit !== undefined) query.set('limit', String(options.limit));
    if (options.cursor !== undefined) query.set('cursor', options.cursor);
    return this.transport.json('GET', sandboxPath(sandboxID) + '/files/list?' + query, true, options.signal);
  }

  readArchive(sandboxID: string, path: string, options: RequestOptions = {}): Promise<Uint8Array> {
    return this.transport.request('GET', sandboxPath(sandboxID) + '/files/archive?path=' + encodeURIComponent(path), true, options.signal);
  }

  /** One attempt: extraction is not atomic and cannot safely replay ambiguous uploads. */
  async writeArchive(sandboxID: string, path: string, data: Uint8Array, options: RequestOptions = {}): Promise<void> {
    await this.transport.request('PUT', sandboxPath(sandboxID) + '/files/archive?path=' + encodeURIComponent(path), false,
      options.signal, data);
  }

  readFile(sandboxID: string, path: string, options: RequestOptions = {}): Promise<Uint8Array> {
    return this.transport.request('GET', sandboxPath(sandboxID) + '/files?path=' + encodeURIComponent(path), true, options.signal);
  }

  async writeFile(sandboxID: string, path: string, data: Uint8Array, options: RequestOptions = {}): Promise<void> {
    await this.transport.request('PUT', sandboxPath(sandboxID) + '/files?path=' + encodeURIComponent(path), true,
      options.signal, new Uint8Array(data));
  }

  /** One fresh bounded recovery attempt, sharing the deadline across replay and cancel. */
  private async recover(sandboxID: string, executionID: string | undefined, cause: unknown,
    request?: ExecuteRequest): Promise<SandboxError> {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), 5000);
    let execution: Execution | Pick<Execution, 'id' | 'sandbox_id'> | undefined = executionID === undefined ?
      undefined : { id: executionID, sandbox_id: sandboxID };
    const key = request?.idempotency_key;
    try {
      if (request) execution = await this.start(sandboxID, request, controller.signal);
      if (!execution) throw new Error('no execution identity');
      const canceledExecution = await this.cancelExecution(sandboxID, execution.id, { signal: controller.signal });
      execution = canceledExecution;
      if (canceledExecution.state !== 'completed') {
        throw new Error('server cancellation response is not terminal');
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
