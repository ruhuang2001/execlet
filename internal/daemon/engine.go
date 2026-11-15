package daemon

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// intentRecord is the termination intent the daemon recorded before it acted.
// Recorded intents win over whatever exit status follows, which is why a 137
// from a cancel is never classified as oom or exited.
type intentRecord struct {
	reason Reason
}

// execution is one execution and its live engine state.
//
// Lock order: the sandbox lock may be held while taking an execution lock, but
// never the reverse. Code holding an execution lock therefore never touches
// its sandbox; finishing an execution hands the cleanup back to the runner.
type execution struct {
	mu sync.Mutex

	id        string
	sandboxID string
	command   []string
	// Env identity retains no raw names or values. Cwd remains diagnostic data.
	envDigest [32]byte
	envCount  int
	cwd       string
	key       string

	startedAt time.Time
	finished  *time.Time

	state    ExecState
	exitCode *int
	reason   *Reason
	log      *eventLog
	logger   *slog.Logger

	// intent is the recorded termination intent: canceled or timeout. It is
	// set before the daemon acts and never cleared, so it wins over whatever
	// exit status follows.
	intent *intentRecord

	// deadline is when this execution's own timeout fires, or nil when it has
	// none. It is fixed at start: the point of an execution timeout is that a
	// command cannot extend it by staying busy.
	deadline *time.Time
	// timedOut records that the reaper already started this execution's
	// timeout escalation, so a later beat does not start a second one.
	timedOut bool

	proc        Process
	oomBaseline int64
	done        chan struct{}
	// confirmedGone is closed once the process tree is known to be gone. A
	// cancel that cannot confirm this removes the container instead.
}

// setIntent records the first termination intent. Later intents do not
// overwrite it: the first reason the daemon acted on is the true one.
func (ex *execution) setIntent(reason Reason) {
	ex.mu.Lock()
	defer ex.mu.Unlock()
	if ex.intent == nil && ex.state == ExecRunning {
		ex.intent = &intentRecord{reason: reason}
	}
}

// finish completes the execution exactly once. It records the outcome, appends
// the terminal event, and closes done. It does not touch the sandbox.
func (ex *execution) finish(at time.Time, exitCode int, outcome ExecOutcome) bool {
	ex.mu.Lock()
	defer ex.mu.Unlock()
	if ex.state == ExecCompleted {
		return false
	}

	ex.state = ExecCompleted
	f := at.UTC()
	ex.finished = &f
	if exitCode >= 0 {
		code := exitCode
		ex.exitCode = &code
	} else {
		ex.exitCode = nil
	}
	reason := outcome.Reason
	ex.reason = &reason

	// The terminal record is stored without output: the log's events are the
	// only copy, and the delivery path assembles it for the client. Storing it
	// here as well would double the memory each execution keeps.
	ex.log.appendComplete(Execution{
		ID:                ex.id,
		SandboxID:         ex.sandboxID,
		Command:           append([]string(nil), ex.command...),
		Cwd:               cwdPtr(ex.cwd),
		State:             ExecCompleted,
		ExitCode:          ex.exitCode,
		TerminationReason: ex.reason,
		StartedAt:         ex.startedAt,
		FinishedAt:        ex.finished,
	})
	close(ex.done)
	logger := ex.logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Info("execution completed", "sandbox_id", ex.sandboxID, "execution_id", ex.id, "exit_code", ex.exitCode, "reason", *ex.reason)
	return true
}

// retained reports how many bytes of output the execution keeps.
func (ex *execution) retained() int {
	return ex.log.retainedBytes()
}

// snapshot returns the contract view with retained output.
func (ex *execution) snapshot() Execution {
	ex.mu.Lock()
	defer ex.mu.Unlock()
	return ex.snapshotLocked()
}

func (ex *execution) snapshotLocked() Execution {
	stdout, stderr, truncated := ex.log.output()
	return Execution{
		ID:                ex.id,
		SandboxID:         ex.sandboxID,
		Command:           append([]string(nil), ex.command...),
		Cwd:               cwdPtr(ex.cwd),
		State:             ex.state,
		ExitCode:          cloneInt(ex.exitCode),
		TerminationReason: cloneReason2(ex.reason),
		Stdout:            string(stdout),
		Stderr:            string(stderr),
		OutputTruncated:   truncated,
		StartedAt:         ex.startedAt,
		FinishedAt:        cloneTime(ex.finished),
	}
}

func (ex *execution) completed() bool {
	ex.mu.Lock()
	defer ex.mu.Unlock()
	return ex.state == ExecCompleted
}

// summary returns the contract view without copying retained output. List
// endpoints use it so a sandbox with a long history does not copy every
// execution's stdout and stderr only to discard them.
func (ex *execution) summary() ExecutionSummary {
	ex.mu.Lock()
	defer ex.mu.Unlock()
	return ExecutionSummary{
		ID:                ex.id,
		SandboxID:         ex.sandboxID,
		Command:           append([]string(nil), ex.command...),
		Cwd:               cwdPtr(ex.cwd),
		State:             ex.state,
		ExitCode:          cloneInt(ex.exitCode),
		TerminationReason: cloneReason2(ex.reason),
		OutputTruncated:   ex.log.truncatedOutput(),
		StartedAt:         ex.startedAt,
		FinishedAt:        cloneTime(ex.finished),
	}
}

// ---------------------------------------------------------------- start

// StartExecution starts command in the sandbox.
//
// The rules come straight from api.md: the sandbox's state decides whether a
// start is allowed; a paused sandbox resumes first; one execution runs at a
// time, so an overlapping start is busy; and a reused idempotency key returns
// the existing execution instead of starting a second process.
func (s *Sandboxes) StartExecution(ctx context.Context, sandboxID string, req ExecuteRequest) (Execution, bool, error) {
	if len(req.Command) == 0 {
		return Execution{}, false, errorf(CodeInvalidRequest, "command must not be empty")
	}
	if req.IdempotencyKey != "" && len(req.IdempotencyKey) > MaxIdempotencyKey {
		return Execution{}, false, errorf(CodeInvalidRequest, "idempotency_key exceeds %d characters", MaxIdempotencyKey)
	}
	if req.TimeoutSeconds != nil && *req.TimeoutSeconds < 0 {
		return Execution{}, false, errorf(CodeInvalidRequest, "timeout_seconds must not be negative")
	}
	if err := validateExecEnv(req.Env); err != nil {
		return Execution{}, false, err
	}
	if err := validateCwd(req.Cwd); err != nil {
		return Execution{}, false, err
	}

	sb, err := s.lookup(sandboxID)
	if err != nil {
		return Execution{}, false, err
	}
	sb.mu.Lock()

	// State comes first: a killed or pending sandbox rejects any start, even a
	// replay of an idempotency key it accepted earlier. Idempotency answers
	// "did this request already run", not "is this sandbox usable".
	switch sb.state {
	case StateRunning:
	case StatePaused:
		if err := s.unpauseLocked(ctx, sb); err != nil {
			sb.mu.Unlock()
			return Execution{}, false, err
		}
	default:
		err := sb.invalidState()
		sb.mu.Unlock()
		return Execution{}, false, err
	}

	if key := req.IdempotencyKey; key != "" {
		if prior := sb.idempotency[key]; prior != nil {
			prior.mu.Lock()
			same := sameInvocation(prior, req)
			prior.mu.Unlock()
			if !same {
				sb.mu.Unlock()
				return Execution{}, false, errorf(CodeIdempotencyConflict,
					"idempotency key was used for a different command, cwd, or environment")
			}
			out := prior.snapshot()
			sb.mu.Unlock()
			return out, false, nil
		}
	}

	if sb.running != nil {
		runningID := sb.running.id
		sb.mu.Unlock()
		return Execution{}, false, errorf(CodeBusy, "sandbox %s is already running execution %s", sandboxID, runningID)
	}

	// The counter read happens while the request is still alive, so the
	// request's context is fine there. The process itself outlives the
	// request that started it: net/http cancels the request context when the
	// handler returns, and a command must keep running after that.
	//
	// One failed read would otherwise disable OOM attribution for the whole
	// execution (baseline -1 makes the classifier skip the delta check), so
	// the read is retried once. A second failure is rare and is reported in
	// the log, because OOM detection is the one thing a 137 cannot recover
	// from afterwards.
	baseline, oomErr := s.readOOMBaseline(ctx, sb.container)
	if oomErr != nil {
		contextLogger(ctx).WarnContext(ctx, "oom baseline unavailable", "sandbox_id", sandboxID)
	}

	// The start call is bounded so a hung runtime cannot hold sb.mu forever:
	// the reaper skips a busy sandbox, so a permanent lock would mean its
	// timeouts never fire again. The bound covers the start handshake only;
	// the process itself outlives this context, which the backend detaches
	// from the request.
	startCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), BackendCallTimeout)
	proc, err := s.backend.Exec(startCtx, sb.container, ExecSpec{
		Command: req.Command,
		Env:     req.envPairs(),
		Dir:     req.Cwd,
	})
	cancel()
	if err != nil {
		sb.mu.Unlock()
		return Execution{}, false, backendError(err)
	}

	ex := &execution{
		id:          newID("exe_"),
		sandboxID:   sandboxID,
		command:     append([]string(nil), req.Command...),
		envDigest:   envFingerprint(req),
		envCount:    len(req.Env),
		cwd:         req.Cwd,
		key:         req.IdempotencyKey,
		startedAt:   s.now(),
		state:       ExecRunning,
		log:         newEventLog(),
		logger:      contextLogger(ctx),
		proc:        proc,
		oomBaseline: baseline,
		done:        make(chan struct{}),
	}
	if req.TimeoutSeconds != nil && *req.TimeoutSeconds > 0 {
		deadline := s.now().Add(time.Duration(*req.TimeoutSeconds) * time.Second)
		ex.deadline = &deadline
	}
	sb.executions = append(sb.executions, ex)
	sb.running = ex
	if ex.key != "" {
		sb.idempotency[ex.key] = ex
	}
	sb.lastActivity = s.now()
	sb.pruneExecutionsLocked()
	// The contract reports the execution as running with empty output; the
	// pumps below deliver the rest through the event log.
	out := ex.snapshotLocked()
	sb.mu.Unlock()

	contextLogger(ctx).InfoContext(ctx, "execution started", "sandbox_id", sandboxID, "execution_id", ex.id)
	go s.run(context.WithoutCancel(ctx), sb, ex)
	return out, true, nil
}

// readOOMBaseline returns the cgroup oom_kill counter, retrying once. A
// failed read is reported to the caller, which records -1: the classifier then
// skips the delta check rather than guess.
//
// Both attempts share one bounded context, so a runtime that never answers
// cannot hold the sandbox lock for longer than BackendCallTimeout.
func (s *Sandboxes) readOOMBaseline(ctx context.Context, containerID string) (int64, error) {
	callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), BackendCallTimeout)
	defer cancel()
	baseline, err := s.backend.OOMKillCount(callCtx, containerID)
	if err == nil {
		return baseline, nil
	}
	baseline, retryErr := s.backend.OOMKillCount(callCtx, containerID)
	if retryErr != nil {
		return -1, retryErr
	}
	return baseline, nil
}

// run pumps output, waits for the process, classifies the exit, and finishes
// the execution. It is the only writer of the terminal state.
func (s *Sandboxes) run(ctx context.Context, sb *sandbox, ex *execution) {
	var pumps sync.WaitGroup
	pumps.Go(func() {
		pump(ex.log, EventStdout, ex.proc.Stdout())
	})
	pumps.Go(func() {
		pump(ex.log, EventStderr, ex.proc.Stderr())
	})

	exitCode, waitErr := ex.proc.Wait()
	pumps.Wait()

	ex.mu.Lock()
	intent := ex.intent
	baseline := ex.oomBaseline
	ex.mu.Unlock()

	outcome := classify(context.WithoutCancel(context.Background()), s.backend, sb.container, baseline, exitCode, intent, s.sleep)
	if len(outcome.sampled) > 0 {
		// One line per ambiguous 137: the observed liveness sequence and how
		// long it took to settle. This is the evidence a machine we cannot
		// reproduce on leaves behind for a misclassification.
		contextLogger(ctx).InfoContext(ctx, "execution liveness settled", "sandbox_id", ex.sandboxID, "execution_id", ex.id, "samples", outcome.sampled, "duration_ms", outcome.elapsed.Milliseconds(), "reason", outcome.Reason)
	}
	if waitErr != nil && intent == nil && !outcome.SandboxLost {
		// The runtime failed to report an exit status. Keep the reason the
		// classification gave, but the exit code stays unknown.
		exitCode = -1
	}

	// Completion and the sandbox's release happen under one sandbox lock.
	// finish makes the execution visible as completed, and a client that
	// sees that may act at once: start the next command, or read the
	// sandbox. If running were cleared only afterwards, that start would find
	// the sandbox busy; if a lost sandbox were marked killed only afterwards,
	// the client would see it running and start a command against a
	// container that is gone. The lock order (sandbox, then execution) allows
	// finish to run inside it.
	now := s.now()
	sb.mu.Lock()
	if ex.finish(now, exitCode, outcome) {
		if sb.running == ex {
			sb.running = nil
		}
		sb.lastActivity = now
	}
	if outcome.SandboxLost {
		s.terminateLocked(context.WithoutCancel(context.Background()), sb, KillEvicted)
	}
	sb.mu.Unlock()
}

// pump copies r into the event log in chunks no larger than the per-event
// limit. A read can end inside a UTF-8 sequence, and an event whose payload is
// half a rune would be turned into U+FFFD by the JSON encoder on the wire, so
// the incomplete tail is carried into the next read instead of being
// delivered. At most utf8.UTFMax-1 bytes are ever pending. A read error ends
// the pump; the process wait is the authority on the outcome.
func pump(log *eventLog, kind EventType, r io.Reader) {
	if r == nil {
		return
	}
	buf := make([]byte, MaxEventDataBytes)
	carry := 0
	for {
		// Read at most MaxEventDataBytes in total, so a large write produces
		// full events rather than a full one plus a small remainder.
		n, err := r.Read(buf[carry:])
		total := carry + n

		// Hold back a partial rune; the next read completes it. Nothing is
		// held back at EOF: a truncated sequence has no completion coming,
		// and the encoder will substitute U+FFFD for it.
		tail := 0
		if err == nil && total > 0 {
			tail = incompleteRuneTail(buf[:total])
		}
		if total-tail > 0 {
			log.appendOutput(kind, buf[:total-tail])
		}
		carry = tail
		if tail > 0 {
			// Move the pending bytes to the front only after the data before
			// them has been sent: copying first would overwrite that data.
			copy(buf, buf[total-tail:total])
		}
		if err != nil {
			return
		}
	}
}

// ---------------------------------------------------------------- cancel

// CancelExecution terminates an execution's whole process group: SIGTERM,
// then SIGKILL after the grace period. It answers only once the execution is
// completed. Canceling a completed execution returns it unchanged.
//
// If the process tree cannot be confirmed gone within the cancel deadline, the
// container is removed so nothing keeps running, and the sandbox ends killed
// with reason requested.
func (s *Sandboxes) CancelExecution(ctx context.Context, sandboxID, executionID string) (Execution, error) {
	sb, ex, err := s.execution(sandboxID, executionID)
	if err != nil {
		return Execution{}, err
	}
	if ex.completed() {
		return ex.snapshot(), nil
	}

	// A paused container delivers no signals, and the contract says a cancel
	// in a paused sandbox resumes it first.
	sb.mu.Lock()
	if sb.state == StatePaused {
		if err := s.backend.Unpause(ctx, sb.container); err != nil {
			sb.mu.Unlock()
			return Execution{}, s.backendFailure(ctx, sb, err)
		}
		sb.state = StateRunning
		sb.armLifetimeLocked(s.now())
	}
	sb.mu.Unlock()

	// Record the intent before acting: it explains whatever exit code follows.
	ex.setIntent(ReasonCanceled)

	// The escalation waits below must not inherit the caller's request
	// context. A client that gives up on the cancel request (a timeout, a
	// closed terminal, a dropped proxy connection) would otherwise cut both
	// waits short and move straight to removing the container, turning a
	// slow signal into sandbox destruction. The cancel grace and deadline
	// bound the action; the connection does not.
	cleanup := context.WithoutCancel(ctx)
	_ = ex.proc.Signal(SignalTerm)
	if s.waitDone(cleanup, ex.done, s.cancelGrace) {
		return ex.snapshot(), nil
	}
	_ = ex.proc.Signal(SignalKill)
	if s.waitDone(cleanup, ex.done, s.cancelDeadline-s.cancelGrace) {
		return ex.snapshot(), nil
	}

	// The process tree could not be confirmed gone. Removing the container is
	// the only way to guarantee nothing keeps running.
	sb.mu.Lock()
	s.terminateLocked(cleanup, sb, KillRequested)
	sb.mu.Unlock()
	// Removing the container ends the process, so the runner finishes the
	// execution. Give it a moment, then report what is known.
	s.waitDone(cleanup, ex.done, s.cancelGrace)
	return ex.snapshot(), nil
}

// timeoutExecution ends one execution whose own deadline is due. It runs on
// its own goroutine because the reaper is a single goroutine and the SIGTERM
// grace alone outlasts a beat.
//
// The escalation is the cancel escalation with a different reason: SIGTERM,
// then SIGKILL after the grace period, and container removal only when the
// process tree cannot be confirmed gone. Unlike a cancel it normally leaves
// the sandbox running, because the command timed out, not the sandbox. The
// container-removal fallback is the exception: it ends the sandbox with
// kill_reason timeout, the same reason a lifetime kill records.
func (s *Sandboxes) timeoutExecution(ctx context.Context, sb *sandbox, ex *execution) {
	if s.waitDone(ctx, ex.done, 0) {
		return
	}

	// A paused container delivers no signals. A timeout that reaches the
	// execution while the sandbox is paused resumes it, so the signal can
	// land; the deadline was already due, so this does not extend anything.
	sb.mu.Lock()
	if sb.state == StatePaused {
		if err := s.backend.Unpause(ctx, sb.container); err != nil {
			// The process outlives its deadline by one beat and timedOut is
			// already set, so the deadline is not reclaimed. Removing the
			// container is the only guarantee left.
			s.terminateLocked(ctx, sb, KillTimeout)
			sb.mu.Unlock()
			return
		}
		sb.state = StateRunning
		sb.armLifetimeLocked(s.now())
	}
	sb.mu.Unlock()

	cleanup := context.WithoutCancel(ctx)
	_ = ex.proc.Signal(SignalTerm)
	if s.waitDone(cleanup, ex.done, s.cancelGrace) {
		return
	}
	_ = ex.proc.Signal(SignalKill)
	if s.waitDone(cleanup, ex.done, s.cancelDeadline-s.cancelGrace) {
		return
	}

	sb.mu.Lock()
	s.terminateLocked(cleanup, sb, KillTimeout)
	sb.mu.Unlock()
}

// waitDone waits for done or ctx or the timeout, reporting whether done
// closed.
func (s *Sandboxes) waitDone(ctx context.Context, done <-chan struct{}, timeout time.Duration) bool {
	if timeout <= 0 {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	case <-t.C:
		return false
	}
}

// ---------------------------------------------------------------- reads

// GetExecution returns one execution with retained output.
func (s *Sandboxes) GetExecution(sandboxID, executionID string) (Execution, error) {
	_, ex, err := s.execution(sandboxID, executionID)
	if err != nil {
		return Execution{}, err
	}
	return ex.snapshot(), nil
}

// ListExecutions returns every execution of the sandbox, newest first, without
// output.
func (s *Sandboxes) ListExecutions(sandboxID string) ([]ExecutionSummary, error) {
	sb, err := s.lookup(sandboxID)
	if err != nil {
		return nil, err
	}
	sb.mu.Lock()
	list := append([]*execution(nil), sb.executions...)
	sb.mu.Unlock()

	out := make([]ExecutionSummary, 0, len(list))
	for i := len(list) - 1; i >= 0; i-- {
		// summary() reads the fields without copying retained output, so a
		// long history costs one slice allocation, not one output copy per
		// execution.
		out = append(out, list[i].summary())
	}
	return out, nil
}

// execution looks up one execution inside a sandbox.
func (s *Sandboxes) execution(sandboxID, executionID string) (*sandbox, *execution, error) {
	sb, err := s.lookup(sandboxID)
	if err != nil {
		return nil, nil, err
	}
	sb.mu.Lock()
	var found *execution
	for _, ex := range sb.executions {
		if ex.id == executionID {
			found = ex
			break
		}
	}
	sb.mu.Unlock()
	if found == nil {
		return nil, nil, errorf(CodeNotFound, "execution %s does not exist in sandbox %s", executionID, sandboxID)
	}
	return sb, found, nil
}

// ---------------------------------------------------------------- events

// Stream delivers one execution's events. It keeps its own cursor, so calling
// Next repeatedly never re-delivers an event: that is what makes a reconnect
// with Last-Event-ID lossless. Closing a stream never affects the execution.
type Stream struct {
	log    *eventLog
	cursor uint64
	once   sync.Once
	close  func()
}

// Events returns a Stream positioned after lastEventID. A lastEventID of 0
// replays from the first event, which reproduces the retained stdout and
// stderr exactly. The stream must be closed; an open stream counts as
// activity for the idle timeout.
//
// A cursor beyond the newest event is rejected: no event will ever have the
// requested ID, and a running execution would otherwise leave the caller
// waiting for a replay that cannot arrive.
func (s *Sandboxes) Events(sandboxID, executionID string, lastEventID uint64) (*Stream, error) {
	_, ex, err := s.execution(sandboxID, executionID)
	if err != nil {
		return nil, err
	}
	if newest := ex.log.lastEventID(); lastEventID > newest {
		return nil, errorf(CodeInvalidRequest,
			"Last-Event-ID %d is beyond the newest event %d", lastEventID, newest)
	}
	closeStream, err := s.OpenStream(sandboxID)
	if err != nil {
		return nil, err
	}
	return &Stream{log: ex.log, cursor: lastEventID, close: closeStream}, nil
}

// Next returns every event after the cursor, blocking until at least one is
// available, the log completes, or ctx is done. done is true once the complete
// event has been delivered.
func (st *Stream) Next(ctx context.Context) (events []Event, done bool, err error) {
	events, done, err = st.log.wait(ctx, st.cursor)
	if len(events) > 0 {
		st.cursor = events[len(events)-1].ID
	}
	return events, done, err
}

// Close releases the stream.
func (st *Stream) Close() {
	st.once.Do(st.close)
}

// LastEventID returns the newest event ID of an execution, for diagnostics
// and for a client that wants to resume without replaying.
func (s *Sandboxes) LastEventID(sandboxID, executionID string) (uint64, error) {
	_, ex, err := s.execution(sandboxID, executionID)
	if err != nil {
		return 0, err
	}
	return ex.log.lastEventID(), nil
}

// ---------------------------------------------------------------- files

// ReadFile returns a file inside the sandbox. A paused sandbox resumes first,
// as the transition table says.
func (s *Sandboxes) ReadFile(ctx context.Context, sandboxID, path string) ([]byte, error) {
	if !isAbsPath(path) {
		return nil, errorf(CodeInvalidRequest, "path must be absolute")
	}
	sb, err := s.prepareFiles(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	sb.mu.Lock()
	container := sb.container
	sb.mu.Unlock()

	data, err := s.backend.ReadFile(ctx, container, path)
	if err != nil {
		return nil, backendError(err)
	}
	return data, nil
}

// WriteFile writes a file inside the sandbox, creating parent directories.
func (s *Sandboxes) WriteFile(ctx context.Context, sandboxID, path string, data []byte) error {
	if !isAbsPath(path) {
		return errorf(CodeInvalidRequest, "path must be absolute")
	}
	if len(data) > MaxFileBytes {
		return errorf(CodePayloadTooLarge, "file exceeds %d bytes", MaxFileBytes)
	}
	sb, err := s.prepareFiles(ctx, sandboxID)
	if err != nil {
		return err
	}
	sb.mu.Lock()
	container := sb.container
	sb.mu.Unlock()

	if err := s.backend.WriteFile(ctx, container, path, data); err != nil {
		return backendError(err)
	}
	return nil
}

// prepareFiles applies the state rules for file access: allowed while running,
// a resume first while paused, invalid while pending or killed.
func (s *Sandboxes) prepareFiles(ctx context.Context, sandboxID string) (*sandbox, error) {
	sb, err := s.lookup(sandboxID)
	if err != nil {
		return nil, err
	}
	sb.mu.Lock()
	defer sb.mu.Unlock()
	switch sb.state {
	case StateRunning:
		return sb, nil
	case StatePaused:
		if err := s.unpauseLocked(ctx, sb); err != nil {
			return nil, err
		}
		return sb, nil
	default:
		return nil, sb.invalidState()
	}
}

func isAbsPath(path string) bool {
	return len(path) > 0 && path[0] == '/'
}

// validateExecEnv enforces the environment limits from docs/api.md. Every bound
// is checked before the sandbox lookup, so a malformed request costs nothing
// and answers the same way whatever the sandbox's state is.
func validateExecEnv(env map[string]string) error {
	if len(env) == 0 {
		return nil
	}
	if len(env) > MaxEnvEntries {
		return errorf(CodeInvalidRequest, "env has %d entries; the limit is %d", len(env), MaxEnvEntries)
	}
	total := 0
	for name, value := range env {
		switch {
		case name == "":
			return errorf(CodeInvalidRequest, "env has an empty variable name")
		case len(name) > MaxEnvKeyBytes:
			return errorf(CodeInvalidRequest, "env name %q exceeds %d bytes", name, MaxEnvKeyBytes)
		case strings.ContainsAny(name, "=\x00\n"):
			return errorf(CodeInvalidRequest, "env name %q must not contain '=', a NUL byte, or a newline", name)
		case len(value) > MaxEnvValueBytes:
			return errorf(CodeInvalidRequest, "env value for %q exceeds %d bytes", name, MaxEnvValueBytes)
		case strings.IndexByte(value, 0) >= 0:
			return errorf(CodeInvalidRequest, "env value for %q must not contain a NUL byte", name)
		}
		total += len(name) + 1 + len(value)
		if total > MaxEnvTotalBytes {
			return errorf(CodeInvalidRequest, "env exceeds %d bytes in total", MaxEnvTotalBytes)
		}
	}
	return nil
}

// validateCwd checks only what the daemon can decide without asking the
// runtime: the path is absolute and bounded. Whether the directory exists is
// deliberately not checked here. A stat would cost a backend round trip on
// every start and could race with the command that creates the directory; the
// wrapper reports an unusable directory as exit 126 instead, which the
// execution record then carries.
func validateCwd(cwd string) error {
	switch {
	case cwd == "":
		return nil
	case len(cwd) > MaxCwdBytes:
		return errorf(CodeInvalidRequest, "cwd exceeds %d bytes", MaxCwdBytes)
	case strings.IndexByte(cwd, 0) >= 0:
		return errorf(CodeInvalidRequest, "cwd must not contain a NUL byte")
	case !isAbsPath(cwd):
		return errorf(CodeInvalidRequest, "cwd must be an absolute path")
	}
	return nil
}

// sameInvocation reports whether an existing execution was started with this
// request. It is the idempotency comparison: a key identifies one invocation,
// and a replay with a different environment would otherwise return an
// execution that ran with the old one.
func sameInvocation(ex *execution, req ExecuteRequest) bool {
	return equalStrings(ex.command, req.Command) &&
		ex.envCount == len(req.Env) &&
		ex.envDigest == envFingerprint(req) &&
		ex.cwd == req.Cwd
}

// ---------------------------------------------------------------- small helpers

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func cloneInt(v *int) *int {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}

// cwdPtr returns a pointer to cwd for the snapshot, or nil when the execution
// used the image's working directory. The field is nullable on the wire so a
// client can tell "no cwd requested" from a directory it happens to share with
// the image's default.
func cwdPtr(cwd string) *string {
	if cwd == "" {
		return nil
	}
	return &cwd
}

// cloneString copies a *string so a caller cannot mutate a snapshot through the
// pointer it was handed.
func cloneString(v *string) *string {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}

func cloneReason2(r *Reason) *Reason {
	if r == nil {
		return nil
	}
	c := *r
	return &c
}
