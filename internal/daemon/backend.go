package daemon

import (
	"context"
	"io"

	"github.com/ruhuang2001/tiny-sandbox/internal/filearchive"
)

type Signal int

const (
	SignalTerm Signal = iota + 1
	SignalKill
)

func (s Signal) String() string {
	switch s {
	case SignalTerm:
		return "SIGTERM"
	case SignalKill:
		return "SIGKILL"
	default:
		return "signal"
	}
}

type Liveness int

const (
	LivenessRunning Liveness = iota
	LivenessPaused
	LivenessStopped
	LivenessMissing
)

func (l Liveness) Alive() bool {
	return l == LivenessRunning || l == LivenessPaused
}

func (l Liveness) String() string {
	switch l {
	case LivenessRunning:
		return "running"
	case LivenessPaused:
		return "paused"
	case LivenessStopped:
		return "stopped"
	case LivenessMissing:
		return "missing"
	default:
		return "unknown"
	}
}

type ContainerSpec struct {
	Runtime   string
	Image     string
	MemoryMB  int
	PidsLimit int
	NanoCPUs  int64
	Network   NetworkMode
	Name      string
	Labels    map[string]string
}

type ExecSpec struct {
	OutputTail  bool
	ExecutionID string
	Command     []string
	Env         []string
	Dir         string
	Stdin       string
	TimeoutMS   *int64
}

type Process interface {
	Stdout() io.Reader
	Stderr() io.Reader
	Wait() (exitCode int, err error)
	Signal(sig Signal) error
}

type Backend interface {
	Create(ctx context.Context, spec ContainerSpec) (containerID string, err error)
	Remove(ctx context.Context, containerID string) error
	Pause(ctx context.Context, containerID string) error
	Unpause(ctx context.Context, containerID string) error
	Exec(ctx context.Context, containerID string, spec ExecSpec) (Process, error)
	Liveness(ctx context.Context, containerID string) (Liveness, error)
	OOMKillCount(ctx context.Context, containerID string) (int64, error)
	ReadArchive(ctx context.Context, containerID, path string) ([]byte, error)
	WriteArchive(ctx context.Context, containerID, path string, archive *filearchive.Archive) error
	ListFiles(ctx context.Context, containerID, path, after string, limit int) ([]FileEntry, error)
	ReadFile(ctx context.Context, containerID, path string) ([]byte, error)
	WriteFile(ctx context.Context, containerID, path string, data []byte) (FileEntry, error)
}
