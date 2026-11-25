package sandbox

type State string

const (
	StatePending State = "pending"
	StateRunning State = "running"
	StatePaused  State = "paused"
	StateKilled  State = "killed"
)

type KillReason string

const (
	KillRequested   KillReason = "requested"
	KillTimeout     KillReason = "timeout"
	KillIdleTimeout KillReason = "idle_timeout"
	KillEvicted     KillReason = "evicted"
)

type Action string

const (
	ActionKill  Action = "kill"
	ActionPause Action = "pause"
)

type Reason string

const (
	ReasonCanceled Reason = "canceled"
	ReasonTimeout  Reason = "timeout"
	ReasonOOM      Reason = "oom"
	ReasonEvicted  Reason = "evicted"
	ReasonExited   Reason = "exited"
	ReasonUnknown  Reason = "unknown"
)

type Sandbox struct {
	AgentMode               string      `json:"agent_mode"`
	Protected               bool        `json:"protected"`
	Runtime                 string      `json:"runtime,omitempty"`
	MaxConcurrentExecutions int         `json:"max_concurrent_executions"`
	ID                      string      `json:"id"`
	State                   State       `json:"state"`
	Image                   string      `json:"image"`
	MemoryMB                int         `json:"memory_mb"`
	PidsLimit               int         `json:"pids_limit"`
	NanoCPUs                int64       `json:"nano_cpus"`
	Network                 string      `json:"network"`
	TimeoutSeconds          int         `json:"timeout_seconds"`
	TimeoutAction           Action      `json:"timeout_action"`
	IdleTimeoutSeconds      int         `json:"idle_timeout_seconds"`
	IdleTimeoutAction       Action      `json:"idle_timeout_action"`
	CreatedAt               Time        `json:"created_at"`
	ExpiresAt               *Time       `json:"expires_at"`
	KillReason              *KillReason `json:"kill_reason"`

	Mounts []VolumeMount `json:"mounts,omitempty"`
}

type Execution struct {
	StdoutOffset int64 `json:"stdout_offset,omitzero"`
	StderrOffset int64 `json:"stderr_offset,omitzero"`

	ID                string   `json:"id"`
	PID               *int     `json:"pid,omitempty"`
	SandboxID         string   `json:"sandbox_id"`
	Command           []string `json:"command"`
	Cwd               *string  `json:"cwd"`
	State             string   `json:"state"`
	ExitCode          *int     `json:"exit_code"`
	TerminationReason *Reason  `json:"termination_reason"`
	Stdout            string   `json:"stdout"`
	Stderr            string   `json:"stderr"`
	OutputTruncated   bool     `json:"output_truncated"`
	StartedAt         Time     `json:"started_at"`
	FinishedAt        *Time    `json:"finished_at"`
}

const (
	NetworkFiltered = "filtered"
	NetworkBridge   = "bridge"
	NetworkNone     = "none"
)

type CreateSandboxRequest struct {
	Runtime                 string            `json:"runtime,omitempty"`
	MaxConcurrentExecutions *int              `json:"max_concurrent_executions,omitempty"`
	Image                   *string           `json:"image,omitempty"`
	MemoryMB                *int              `json:"memory_mb,omitempty"`
	PidsLimit               *int              `json:"pids_limit,omitempty"`
	NanoCPUs                *int64            `json:"nano_cpus,omitempty"`
	Network                 *string           `json:"network,omitempty"`
	TimeoutSeconds          *int              `json:"timeout_seconds,omitempty"`
	TimeoutAction           *Action           `json:"timeout_action,omitempty"`
	IdleTimeoutSeconds      *int              `json:"idle_timeout_seconds,omitempty"`
	IdleTimeoutAction       *Action           `json:"idle_timeout_action,omitempty"`
	Env                     map[string]string `json:"env,omitempty"`
	Mounts                  []VolumeMount     `json:"mounts,omitempty"`
}

type UpdateSandboxRequest struct {
	TimeoutSeconds *int `json:"timeout_seconds,omitempty"`
	ExtendSeconds  *int `json:"extend_seconds,omitempty"`
}

type ExecuteRequest struct {
	OutputTail     bool              `json:"output_tail,omitzero"`
	Command        []string          `json:"command"`
	IdempotencyKey string            `json:"idempotency_key,omitempty"`
	TimeoutSeconds *int              `json:"timeout_seconds,omitempty"`
	TimeoutMS      *int64            `json:"timeout_ms,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	Cwd            string            `json:"cwd,omitempty"`
	Stdin          string            `json:"stdin,omitempty"`
}

type EventType string

const (
	EventStdout    EventType = "stdout"
	EventStderr    EventType = "stderr"
	EventTruncated EventType = "truncated"
	EventComplete  EventType = "complete"
)

type Event struct {
	ID         uint64
	Type       EventType
	Data       string
	LimitBytes int
	Execution  *Execution
}

type sandboxList struct {
	Sandboxes []*Sandbox `json:"sandboxes"`
}

type executionList struct {
	Executions []*Execution `json:"executions"`
}
