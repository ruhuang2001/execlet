export type OpenString<T extends string> = T | (string & {});
export type Action = 'kill' | 'pause';
export type TerminationReason = 'exited' | 'canceled' | 'timeout' | 'oom' | 'evicted' | 'unknown';

export interface Volume {
  id: string;
  size_bytes: number;
  created_at: string;
  [field: string]: unknown;
}

export interface CreateVolumeRequest { size_bytes: number }

export interface VolumeMount {
  volume_id: string;
  path: string;
  read_only?: boolean;
}

export interface Sandbox {
  agent_mode?: OpenString<'guest' | 'legacy' | 'unavailable'>;
  protected?: boolean;
  id: string;
  state: OpenString<'pending' | 'running' | 'paused' | 'killed'>;
  image: string;
  memory_mb: number;
  max_concurrent_executions?: number;
  pids_limit?: number;
  nano_cpus?: number;
  network?: OpenString<'bridge' | 'none' | 'filtered'>;
  runtime?: OpenString<'runc' | 'runsc'>;
  timeout_seconds: number;
  timeout_action: OpenString<Action>;
  idle_timeout_seconds: number;
  idle_timeout_action: OpenString<Action>;
  created_at: string;
  expires_at: string | null;
  kill_reason: OpenString<'requested' | 'timeout' | 'idle_timeout' | 'evicted'> | null;
  mounts?: VolumeMount[];
  [field: string]: unknown;
}

export interface Execution {
  id: string;
  sandbox_id: string;
  pid?: number;
  command: string[];
  cwd?: string | null;
  state: OpenString<'running' | 'completed'>;
  exit_code: number | null;
  termination_reason: OpenString<TerminationReason> | null;
  stdout?: string;
  stderr?: string;
  output_truncated: boolean;
  started_at: string;
  finished_at: string | null;
  [field: string]: unknown;
}

export type CompletedExecution = Execution & { state: 'completed' };

export interface Capabilities { capabilities: string[] }
export interface CommandAttachment { execution: Execution; last_event_id: number }
export interface KillExecutionResponse { execution: CompletedExecution; signaled: boolean }

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
  network?: 'bridge' | 'none' | 'filtered';
  runtime?: 'runc' | 'runsc';
  timeout_seconds?: number;
  timeout_action?: Action;
  idle_timeout_seconds?: number;
  idle_timeout_action?: Action;
  env?: Readonly<Record<string, string>>;
  mounts?: readonly VolumeMount[];
}

export interface UpdateSandboxRequest { timeout_seconds?: number; extend_seconds?: number }

export interface ExecuteRequest {
  output_tail?: boolean;
  command: readonly string[];
  idempotency_key?: string;
  timeout_seconds?: number;
  timeout_ms?: number;
  env?: Readonly<Record<string, string>>;
  cwd?: string;
  stdin?: string;
}

export interface RequestOptions { signal?: AbortSignal }

export interface ExecutionOptions extends RequestOptions {
  cancelRemote?: boolean;
}

export interface StreamExecutionOptions extends ExecutionOptions {
  lastEventId?: number;
}

export type Event =
  | { id: number; type: 'stdout' | 'stderr'; data: string }
  | { id: number; type: 'truncated'; limit_bytes: number }
  | { id: number; type: 'complete'; execution: CompletedExecution }
  | { id: number; type: 'unknown'; event_type: string; data: string };

export type EventHandler = (event: Event) => void | Promise<void>;

export interface ClientConfig {
  domain?: string;
  apiKey?: string;
  maxRetries?: number;
  fetch?: typeof globalThis.fetch;
}

export type FileEntryType = OpenString<'file' | 'dir' | 'symlink' | 'other'>;

export interface FileEntry {
  name: string;
  size: number;
  mode: number;
  mtime: string;
  is_dir: boolean;
  type: FileEntryType;
  uid: number;
  gid: number;
  owner?: string;
  group?: string;
  symlink_target?: string;
  [field: string]: unknown;
}
export interface FileList { entries: FileEntry[]; next_cursor?: string; [field: string]: unknown }
export interface ListFilesOptions extends RequestOptions { limit?: number; cursor?: string }
