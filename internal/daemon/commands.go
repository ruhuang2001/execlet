package daemon

import (
	"context"
	"net/http"
	"strconv"
	"time"
)

type ManagedProcess interface {
	Process
	PID() int
	KillManaged() (bool, error)
	TimedOut() bool
	Killed() bool
}

type CommandAttachment struct {
	Execution   Execution `json:"execution"`
	LastEventID uint64    `json:"last_event_id"`
}

type CommandKill struct {
	Execution Execution `json:"execution"`
	Signaled  bool      `json:"signaled"`
}

func (s *Sandboxes) backgroundCommands() bool {
	backend, ok := s.backend.(interface{ BackgroundCommands() bool })
	return ok && backend.BackgroundCommands()
}

func (h *Handler) capabilities(w http.ResponseWriter, _ *http.Request) {
	flags := []string{"execution.env-cwd.v1", "files.archive.v1", "files.list.v1", "execution.stdin.v1", "execution.concurrent.v1", "sandbox.runtime.v1", "execution.output-tail.v1", "sandbox.purge.v1"}
	if h.store.backgroundCommands() {
		flags = append(flags, "e2b.background.v1")
	}
	writeJSON(w, http.StatusOK, map[string]any{"capabilities": flags})
}

func (s *Sandboxes) AttachCommand(sandboxID string, pid int) (CommandAttachment, error) {
	if pid <= 0 {
		return CommandAttachment{}, errorf(CodeInvalidRequest, "pid must be positive")
	}
	sb, err := s.lookup(sandboxID)
	if err != nil {
		return CommandAttachment{}, err
	}
	sb.mu.Lock()
	defer sb.mu.Unlock()
	if sb.state != StateRunning {
		return CommandAttachment{}, sb.invalidState()
	}
	for i := len(sb.executions) - 1; i >= 0; i-- {
		ex := sb.executions[i]
		if ex.pid != pid {
			continue
		}
		ex.mu.Lock()
		snapshot := ex.snapshotLocked()
		ex.log.mu.Lock()
		snapshot.Stdout, snapshot.Stderr, snapshot.OutputTruncated = ex.log.outputLocked()
		attached := CommandAttachment{snapshot, ex.log.nextID}
		ex.log.mu.Unlock()
		ex.mu.Unlock()
		return attached, nil
	}
	return CommandAttachment{}, errorf(CodeNotFound, "managed command does not exist")
}

func (s *Sandboxes) KillExecution(ctx context.Context, sandboxID, executionID string) (CommandKill, error) {
	sb, ex, err := s.execution(sandboxID, executionID)
	if err != nil {
		return CommandKill{}, err
	}
	if ex.completed() {
		return CommandKill{Execution: ex.snapshot()}, nil
	}
	return s.killManaged(ctx, sb, ex)
}

func (s *Sandboxes) killManaged(ctx context.Context, sb *sandbox, ex *execution) (CommandKill, error) {
	managed, ok := ex.proc.(ManagedProcess)
	if !ok || ex.timeoutMS == nil {
		return CommandKill{}, errorf(CodeInvalidRequest, "execution has no lifetime-bound signal support")
	}
	sb.mu.Lock()
	if sb.state != StateRunning {
		err := sb.invalidState()
		sb.mu.Unlock()
		return CommandKill{}, err
	}
	sb.mu.Unlock()
	signaled, signalErr := managed.KillManaged()
	if CodeOf(signalErr) == CodeInvalidState {
		return CommandKill{}, signalErr
	}
	cleanup := context.WithoutCancel(ctx)
	if signalErr == nil && s.waitDone(cleanup, ex.done, s.cancelDeadline) {
		return CommandKill{Execution: ex.snapshot(), Signaled: signaled}, nil
	}
	sb.mu.Lock()
	removalErr := s.terminateLocked(cleanup, sb, KillRequested)
	sb.mu.Unlock()
	s.waitDone(cleanup, ex.done, time.Second)
	if removalErr != nil {
		return CommandKill{}, errorf(CodeBackendError, "signal and sandbox removal unconfirmed for %s", ex.id)
	}
	return CommandKill{}, errorf(CodeBackendError, "command kill required sandbox removal; siblings terminated for %s", ex.id)
}

func (h *Handler) attachCommand(w http.ResponseWriter, r *http.Request) {
	pid, err := strconv.Atoi(r.PathValue("pid"))
	if err != nil || pid <= 0 {
		writeDaemonError(r.Context(), w, errorf(CodeInvalidRequest, "invalid command PID"))
		return
	}
	result, err := h.store.AttachCommand(r.PathValue("sandbox_id"), pid)
	if err != nil {
		writeDaemonError(r.Context(), w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) killExecution(w http.ResponseWriter, r *http.Request) {
	result, err := h.store.KillExecution(r.Context(), r.PathValue("sandbox_id"), r.PathValue("execution_id"))
	if err != nil {
		writeDaemonError(r.Context(), w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
