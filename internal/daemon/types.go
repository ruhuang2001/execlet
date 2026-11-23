package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"sort"
	"time"
)

const (
	DefaultImage          = "alpine:3.20"
	DefaultMemoryMB       = 512
	MinMemoryMB           = 16
	DefaultTimeoutSeconds = 300

	DefaultPidsLimit = 1024
	MinPidsLimit     = 64
	DefaultNetwork   = NetworkBridge

	MaxOutputBytes               = 1 << 20
	MaxOutputEvents              = 4096
	MaxEventDataBytes            = 32 << 10
	MaxIdempotencyKey            = 128
	MaxEnvEntries                = 64
	MaxEnvKeyBytes               = 64
	MaxEnvValueBytes             = 8 << 10
	MaxEnvTotalBytes             = 32 << 10
	MaxCwdBytes                  = 4096
	MaxFileBytes                 = 10 << 20
	MaxRequestBodyBytes          = 1 << 20
	MaxExecutionRequestBodyBytes = 8 << 20
	MaxStdinBytes                = 1 << 20
	MaxCommandBytes              = 1 << 20
	MaxExecutionsPerSandbox      = 200
	MaxSandboxes                 = 64
	MaxRetainedSandboxes         = 64
	MaxRetainedOutputBytes       = 64 << 20
	MaxStreamsPerSandbox         = 32
	BackendCallTimeout           = 5 * time.Second
)

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

type NetworkMode string

const (
	NetworkBridge   NetworkMode = "bridge"
	NetworkNone     NetworkMode = "none"
	NetworkFiltered NetworkMode = "filtered"
)

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

type ExecState string

const (
	ExecRunning   ExecState = "running"
	ExecCompleted ExecState = "completed"
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
	Network                 NetworkMode `json:"network"`
	TimeoutSeconds          int         `json:"timeout_seconds"`
	TimeoutAction           Action      `json:"timeout_action"`
	IdleTimeoutSeconds      int         `json:"idle_timeout_seconds"`
	IdleTimeoutAction       Action      `json:"idle_timeout_action"`
	CreatedAt               time.Time   `json:"created_at"`
	ExpiresAt               *time.Time  `json:"expires_at"`
	KillReason              *KillReason `json:"kill_reason"`
}

type Execution struct {
	StdoutOffset int64 `json:"stdout_offset,omitzero"`
	StderrOffset int64 `json:"stderr_offset,omitzero"`

	ID                string     `json:"id"`
	PID               int        `json:"pid,omitzero"`
	SandboxID         string     `json:"sandbox_id"`
	Command           []string   `json:"command"`
	Cwd               *string    `json:"cwd"`
	State             ExecState  `json:"state"`
	ExitCode          *int       `json:"exit_code"`
	TerminationReason *Reason    `json:"termination_reason"`
	Stdout            string     `json:"stdout"`
	Stderr            string     `json:"stderr"`
	OutputTruncated   bool       `json:"output_truncated"`
	StartedAt         time.Time  `json:"started_at"`
	FinishedAt        *time.Time `json:"finished_at"`
}

type ExecutionSummary struct {
	ID                string     `json:"id"`
	PID               int        `json:"pid,omitzero"`
	SandboxID         string     `json:"sandbox_id"`
	Command           []string   `json:"command"`
	Cwd               *string    `json:"cwd"`
	State             ExecState  `json:"state"`
	ExitCode          *int       `json:"exit_code"`
	TerminationReason *Reason    `json:"termination_reason"`
	OutputTruncated   bool       `json:"output_truncated"`
	StartedAt         time.Time  `json:"started_at"`
	FinishedAt        *time.Time `json:"finished_at"`
}

func (e Execution) Summary() ExecutionSummary {
	return ExecutionSummary{
		ID:                e.ID,
		PID:               e.PID,
		SandboxID:         e.SandboxID,
		Command:           e.Command,
		Cwd:               cloneString(e.Cwd),
		State:             e.State,
		ExitCode:          e.ExitCode,
		TerminationReason: e.TerminationReason,
		OutputTruncated:   e.OutputTruncated,
		StartedAt:         e.StartedAt,
		FinishedAt:        e.FinishedAt,
	}
}

type CreateRequest struct {
	Runtime                 string            `json:"runtime,omitempty"`
	MaxConcurrentExecutions *int              `json:"max_concurrent_executions,omitempty"`
	Image                   *string           `json:"image,omitempty"`
	MemoryMB                *int              `json:"memory_mb,omitempty"`
	PidsLimit               *int              `json:"pids_limit,omitempty"`
	NanoCPUs                *int64            `json:"nano_cpus,omitempty"`
	Network                 *NetworkMode      `json:"network,omitempty"`
	TimeoutSeconds          *int              `json:"timeout_seconds,omitempty"`
	TimeoutAction           *Action           `json:"timeout_action,omitempty"`
	IdleTimeoutSeconds      *int              `json:"idle_timeout_seconds,omitempty"`
	IdleTimeoutAction       *Action           `json:"idle_timeout_action,omitempty"`
	Env                     map[string]string `json:"env,omitempty"`
}

type UpdateRequest struct {
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

func (r ExecuteRequest) envPairs() []string {
	if len(r.Env) == 0 {
		return nil
	}
	pairs := make([]string, 0, len(r.Env))
	for name, value := range r.Env {
		pairs = append(pairs, name+"="+value)
	}
	sort.Strings(pairs)
	return pairs
}

func mergedEnv(base, override map[string]string) []string {
	if len(base) == 0 && len(override) == 0 {
		return nil
	}
	merged := make(map[string]string, len(base)+len(override))
	for name, value := range base {
		merged[name] = value
	}
	for name, value := range override {
		merged[name] = value
	}
	return ExecuteRequest{Env: merged}.envPairs()
}

type sandboxOptions struct {
	runtime                 string
	maxConcurrentExecutions int
	image                   string
	memoryMB                int
	pidsLimit               int
	nanoCPUs                int64
	network                 NetworkMode
	timeoutSeconds          int
	timeoutAction           Action
	idleTimeoutSeconds      int
	idleTimeoutAction       Action
	env                     map[string]string
}

func (r CreateRequest) resolve() (sandboxOptions, error) {
	opts := sandboxOptions{
		runtime:                 r.Runtime,
		maxConcurrentExecutions: 1,
		image:                   DefaultImage,
		memoryMB:                DefaultMemoryMB,
		pidsLimit:               DefaultPidsLimit,
		nanoCPUs:                0,
		network:                 DefaultNetwork,
		timeoutSeconds:          DefaultTimeoutSeconds,
		timeoutAction:           ActionKill,
		idleTimeoutSeconds:      0,
		idleTimeoutAction:       ActionKill,
	}
	if r.MaxConcurrentExecutions != nil {
		if *r.MaxConcurrentExecutions < 1 || *r.MaxConcurrentExecutions > 8 {
			return sandboxOptions{}, errorf(CodeInvalidRequest, "max_concurrent_executions must be between 1 and 8")
		}
		opts.maxConcurrentExecutions = *r.MaxConcurrentExecutions
	}
	if r.Image != nil {
		if *r.Image == "" {
			return sandboxOptions{}, errorf(CodeInvalidRequest, "image must not be empty")
		}
		opts.image = *r.Image
	}
	if r.PidsLimit != nil {
		if *r.PidsLimit < MinPidsLimit {
			return sandboxOptions{}, errorf(CodeInvalidRequest, "pids_limit must be at least %d", MinPidsLimit)
		}
		opts.pidsLimit = *r.PidsLimit
	}
	if r.NanoCPUs != nil {
		if *r.NanoCPUs < 0 {
			return sandboxOptions{}, errorf(CodeInvalidRequest, "nano_cpus must not be negative; use 0 for no limit")
		}
		opts.nanoCPUs = *r.NanoCPUs
	}
	if r.Network != nil {
		switch *r.Network {
		case NetworkBridge, NetworkNone, NetworkFiltered:
			opts.network = *r.Network
		default:
			return sandboxOptions{}, errorf(CodeInvalidRequest, "network must be bridge, none or filtered")
		}
	}
	if r.MemoryMB != nil {
		if *r.MemoryMB < MinMemoryMB {
			return sandboxOptions{}, errorf(CodeInvalidRequest, "memory_mb must be at least %d", MinMemoryMB)
		}
		opts.memoryMB = *r.MemoryMB
	}
	if r.TimeoutSeconds != nil {
		if *r.TimeoutSeconds < 0 {
			return sandboxOptions{}, errorf(CodeInvalidRequest, "timeout_seconds must not be negative")
		}
		opts.timeoutSeconds = *r.TimeoutSeconds
	}
	if r.TimeoutAction != nil {
		if err := validateAction("timeout_action", *r.TimeoutAction); err != nil {
			return sandboxOptions{}, err
		}
		opts.timeoutAction = *r.TimeoutAction
	}
	if r.IdleTimeoutSeconds != nil {
		if *r.IdleTimeoutSeconds < 0 {
			return sandboxOptions{}, errorf(CodeInvalidRequest, "idle_timeout_seconds must not be negative")
		}
		opts.idleTimeoutSeconds = *r.IdleTimeoutSeconds
	}
	if r.IdleTimeoutAction != nil {
		if err := validateAction("idle_timeout_action", *r.IdleTimeoutAction); err != nil {
			return sandboxOptions{}, err
		}
		opts.idleTimeoutAction = *r.IdleTimeoutAction
	}
	if len(r.Env) > 0 {
		if err := validateExecEnv(r.Env); err != nil {
			return sandboxOptions{}, err
		}
		opts.env = cloneEnv(r.Env)
	}
	return opts, nil
}

func validateAction(field string, action Action) error {
	switch action {
	case ActionKill, ActionPause:
		return nil
	default:
		return errorf(CodeInvalidRequest, "%s must be %q or %q", field, ActionKill, ActionPause)
	}
}

func newID(prefix string) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("daemon: generate id: " + err.Error())
	}
	return prefix + hex.EncodeToString(b[:])
}

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}

func cloneReason(r *KillReason) *KillReason {
	if r == nil {
		return nil
	}
	c := *r
	return &c
}

func cloneEnv(env map[string]string) map[string]string {
	if env == nil {
		return nil
	}
	c := make(map[string]string, len(env))
	for name, value := range env {
		c[name] = value
	}
	return c
}
