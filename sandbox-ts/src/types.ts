/** Open response enums let older clients preserve values from newer servers. */
export type OpenString<T extends string> = T | (string & {});
export type Action = 'kill' | 'pause';
export type TerminationReason = 'exited' | 'canceled' | 'timeout' | 'oom' | 'evicted' | 'unknown';

export interface Sandbox {
  id: string;
  state: OpenString<'pending' | 'running' | 'paused' | 'killed'>;
  image: string;
  memory_mb: number;
  /** Additive controls can be absent on older development servers. */
  max_concurrent_executions?: number;
  pids_limit?: number;
  nano_cpus?: number;
  network?: OpenString<'bridge' | 'none'>;
  timeout_seconds: number;
  timeout_action: OpenString<Action>;
  idle_timeout_seconds: number;
  idle_timeout_action: OpenString<Action>;
  created_at: string;
  expires_at: string | null;
  kill_reason: OpenString<'requested' | 'timeout' | 'idle_timeout' | 'evicted'> | null;
  [field: string]: unknown;
}

export interface Execution {
  id: string;
  sandbox_id: string;
  command: string[];
  /** The requested working directory; null when the request set none. Absent on servers without cwd support. */
  cwd?: string | null;
  state: OpenString<'running' | 'completed'>;
  exit_code: number | null;
  termination_reason: OpenString<TerminationReason> | null;
  /** List responses omit output. No artificial empty-string defaults. */
  stdout?: string;
  stderr?: string;
  output_truncated: boolean;
  started_at: string;
  finished_at: string | null;
  [field: string]: unknown;
}

export type CompletedExecution = Execution & { state: 'completed' };

/** New wire reasons map to unknown, keeping consumer switches exhaustive. */
export type ExecutionOutcome = {
  [R in Exclude<TerminationReason, 'unknown'>]: {
    reason: R;
    execution: CompletedExecution & { termination_reason: R };
  }
}[Exclude<TerminationReason, 'unknown'>] | {
  reason: 'unknown';
  terminationReason: string | null;
  execution: CompletedExecution;
};

export interface CreateSandboxRequest {
  image?: string;
  memory_mb?: number;
  max_concurrent_executions?: number;
  pids_limit?: number;
  nano_cpus?: number;
  network?: 'bridge' | 'none';
  timeout_seconds?: number;
  timeout_action?: Action;
  idle_timeout_seconds?: number;
  idle_timeout_action?: Action;
  env?: Readonly<Record<string, string>>;
}

export interface UpdateSandboxRequest { timeout_seconds?: number; extend_seconds?: number }

export interface ExecuteRequest {
  command: readonly string[];
  idempotency_key?: string;
  timeout_seconds?: number;
  /** Added to the image's environment for this execution only. Never echoed back. */
  env?: Readonly<Record<string, string>>;
  /** Absolute working directory; omit for the image's own. */
  cwd?: string;
  /** One-shot UTF-8 text, at most 1 MiB, followed by EOF. Never echoed. */
  stdin?: string;
}

export interface RequestOptions { signal?: AbortSignal }

export interface ExecutionOptions extends RequestOptions {
  /** Default true. False aborts only local I/O and never cancels remote work. */
  cancelRemote?: boolean;
}

export type Event =
  | { id: number; type: 'stdout' | 'stderr'; data: string }
  | { id: number; type: 'truncated'; limit_bytes: number }
  | { id: number; type: 'complete'; execution: CompletedExecution }
  | { id: number; type: 'unknown'; event_type: string; data: string };

export type EventHandler = (event: Event) => void | Promise<void>;

export interface ClientConfig {
  /** Defaults to SANDBOX_DOMAIN in Node, then http://localhost:8080. */
  domain?: string;
  /** Defaults to SANDBOX_API_KEY in Node. Never put keys in browser bundles. */
  apiKey?: string;
  /** Zero/omitted: 3; negative: no retries. Same convention as the Go SDK. */
  maxRetries?: number;
  /** Fetch-compatible transport; it must honor AbortSignal and stream bodies. */
  fetch?: typeof globalThis.fetch;
}

/** Metadata of an immediate child; POSIX permission bits and UTC RFC3339 mtime. */
export interface FileEntry { name: string; size: number; mode: number; mtime: string; is_dir: boolean }
export interface FileList { entries: FileEntry[]; next_cursor?: string }
export interface ListFilesOptions extends RequestOptions { limit?: number; cursor?: string }
