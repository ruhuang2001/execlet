package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path"
	"strings"
	"sync"
	"time"
)

type intentRecord struct {
	reason Reason
}

type execution struct {
	mu sync.Mutex

	id             string
	sandboxID      string
	command        []string
	envDigest      [32]byte
	envCount       int
	cwd            string
	key            string
	stdinDigest    [32]byte
	pid            int
	timeoutMS      *int64
	fingerprintKey [32]byte
	persist        func(Execution, []Event) error
	forget         func() error
	outputLost     bool

	startedAt time.Time
	finished  *time.Time

	state    ExecState
	exitCode *int
	reason   *Reason
	log      *eventLog
	logger   *slog.Logger

	intent *intentRecord

	deadline *time.Time
	timedOut bool

	proc        Process
	oomBaseline int64
	done        chan struct{}
}

func (ex *execution) setIntent(reason Reason) {
	ex.mu.Lock()
	defer ex.mu.Unlock()
	if ex.intent == nil && ex.state == ExecRunning {
		ex.intent = &intentRecord{reason: reason}
	}
}

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
	if ex.intent != nil {
		reason = ex.intent.reason
	}
	ex.reason = &reason

	terminal := Execution{
		ID:                ex.id,
		PID:               ex.pid,
		SandboxID:         ex.sandboxID,
		Command:           append([]string(nil), ex.command...),
		Cwd:               cwdPtr(ex.cwd),
		State:             ExecCompleted,
		ExitCode:          ex.exitCode,
		TerminationReason: ex.reason,
		StartedAt:         ex.startedAt,
		FinishedAt:        ex.finished,
	}
	ex.log.mu.Lock()
	terminal.StdoutOffset, terminal.StderrOffset = ex.log.stdoutOffset, ex.log.stderrOffset
	if ex.persist != nil {
		events := append(append([]Event(nil), ex.log.events...), Event{ID: ex.log.nextID + 1, Type: EventComplete, Execution: &terminal})
		terminal.OutputTruncated = ex.log.truncated
		if err := ex.persist(terminal, events); err != nil {
			ex.reason = new(ReasonUnknown)
			ex.exitCode = nil
			ex.log.truncated = true
			terminal.TerminationReason = ex.reason
			terminal.ExitCode = nil
			terminal.OutputTruncated = true
		}
	}
	ex.log.complete = true
	ex.log.appendLocked(Event{Type: EventComplete, Execution: &terminal})
	ex.log.mu.Unlock()
	close(ex.done)
	logger := ex.logger
	if logger == nil {
		logger = slog.Default()
	}
	var exitVal any
	if ex.exitCode != nil {
		exitVal = *ex.exitCode
	}
	logger.Info("execution completed", "sandbox_id", ex.sandboxID, "execution_id", ex.id, "exit_code", exitVal, "reason", *ex.reason)
	return true
}

func (ex *execution) retained() int {
	return ex.log.retainedBytes()
}

func (ex *execution) snapshot() Execution {
	ex.mu.Lock()
	defer ex.mu.Unlock()
	return ex.snapshotLocked()
}

func (ex *execution) snapshotLocked() Execution {
	stdout, stderr, truncated, stdoutOffset, stderrOffset := ex.log.output()
	return Execution{
		ID:                ex.id,
		PID:               ex.pid,
		SandboxID:         ex.sandboxID,
		Command:           append([]string(nil), ex.command...),
		Cwd:               cwdPtr(ex.cwd),
		State:             ex.state,
		ExitCode:          cloneInt(ex.exitCode),
		TerminationReason: cloneReason2(ex.reason),
		StdoutOffset:      stdoutOffset, StderrOffset: stderrOffset,
		Stdout:          stdout,
		Stderr:          stderr,
		OutputTruncated: truncated,
		StartedAt:       ex.startedAt,
		FinishedAt:      cloneTime(ex.finished),
	}
}

func (ex *execution) completed() bool {
	ex.mu.Lock()
	defer ex.mu.Unlock()
	return ex.state == ExecCompleted
}

func (ex *execution) summary() ExecutionSummary {
	ex.mu.Lock()
	defer ex.mu.Unlock()
	return ExecutionSummary{
		ID:                ex.id,
		PID:               ex.pid,
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

func (s *Sandboxes) StartExecution(ctx context.Context, sandboxID string, req ExecuteRequest) (Execution, bool, error) {
	start := time.Now()
	defer s.metrics.observe("tiny_sandbox_execution_start_seconds{operation=\"start\"}", start)
	if err := s.durableReady(); err != nil {
		return Execution{}, false, err
	}
	if len(req.Command) == 0 {
		return Execution{}, false, errorf(CodeInvalidRequest, "command must not be empty")
	}
	commandBytes := 0
	for _, arg := range req.Command {
		if len(arg) >= MaxCommandBytes-commandBytes {
			return Execution{}, false, errorf(CodePayloadTooLarge, "command exceeds %d bytes including argv separators", MaxCommandBytes)
		}
		commandBytes += len(arg) + 1
	}
	if req.IdempotencyKey != "" && len(req.IdempotencyKey) > MaxIdempotencyKey {
		return Execution{}, false, errorf(CodeInvalidRequest, "idempotency_key exceeds %d characters", MaxIdempotencyKey)
	}
	if req.TimeoutSeconds != nil && *req.TimeoutSeconds < 0 {
		return Execution{}, false, errorf(CodeInvalidRequest, "timeout_seconds must not be negative")
	}
	if req.TimeoutMS != nil {
		if req.TimeoutSeconds != nil || *req.TimeoutMS < 0 || *req.TimeoutMS > 9223372036854 || req.Stdin != "" {
			return Execution{}, false, errorf(CodeInvalidRequest, "timeout_ms must be within duration range, without timeout_seconds or stdin")
		}
		if !s.backgroundCommands() {
			return Execution{}, false, errorf(CodeUnavailable, "managed commands are unsupported by this runtime")
		}
	}
	if len(req.Stdin) > MaxStdinBytes {
		return Execution{}, false, errorf(CodePayloadTooLarge, "stdin exceeds %d bytes", MaxStdinBytes)
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
					"idempotency key was used for a different command, cwd, environment, or stdin")
			}
			out := prior.snapshot()
			sb.mu.Unlock()
			if prior.timeoutMS != nil && out.PID <= 0 {
				return Execution{}, false, errorf(CodeBackendError, "managed identity unconfirmed; retained execution %s; do not start with a new key", out.ID)
			}
			return out, false, nil
		}
	}

	if len(sb.running) >= sb.opts.maxConcurrentExecutions {
		sb.mu.Unlock()
		return Execution{}, false, errorf(CodeBusy, "sandbox %s has reached its execution limit", sandboxID)
	}

	baseline, oomErr := int64(-1), error(nil)
	if sb.opts.maxConcurrentExecutions == 1 {
		baseline, oomErr = s.readOOMBaseline(ctx, sb.container)
	}
	if oomErr != nil {
		contextLogger(ctx).WarnContext(ctx, "oom baseline unavailable", "sandbox_id", sandboxID)
	}

	ex := &execution{
		id: newID("exe_"), sandboxID: sandboxID, command: append([]string(nil), req.Command...),
		envDigest: keyedFingerprint(s.fingerprintKey, req.envPairs()), envCount: len(req.Env), cwd: req.Cwd, key: req.IdempotencyKey,
		stdinDigest: keyedFingerprint(s.fingerprintKey, []string{req.Stdin}), fingerprintKey: s.fingerprintKey,
		startedAt: s.now(), state: ExecRunning, log: newEventLog(), logger: contextLogger(ctx), oomBaseline: baseline, done: make(chan struct{}),
	}
	ex.log.keepTail = req.OutputTail
	if req.TimeoutMS != nil {
		ex.timeoutMS = new(*req.TimeoutMS)
	}
	ex.log.onFirstOutput = func() { s.metrics.observe("tiny_sandbox_first_output_seconds{operation=\"execution\"}", start) }
	if s.journal != nil {
		ex.persist = func(snapshot Execution, events []Event) error { return s.saveExecution(ex, snapshot, events) }
		ex.forget = func() error { return s.forgetExecution(sb, ex) }
		if err := ex.persist(ex.snapshotLocked(), nil); err != nil {
			sb.mu.Unlock()
			return Execution{}, false, errorf(CodeUnavailable, "cannot persist execution intent")
		}
		sb.executions = append(sb.executions, ex)
		if ex.key != "" {
			sb.idempotency[ex.key] = ex
		}
		if err := s.saveSandboxLocked(sb); err != nil {
			sb.mu.Unlock()
			return Execution{}, false, err
		}
	}
	startCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), BackendCallTimeout)
	proc, err := s.backend.Exec(startCtx, sb.container, ExecSpec{
		ExecutionID: ex.id, OutputTail: req.OutputTail,
		Command:   req.Command,
		Env:       mergedEnv(sb.opts.env, req.Env),
		Dir:       req.Cwd,
		Stdin:     req.Stdin,
		TimeoutMS: req.TimeoutMS,
	})
	cancel()
	startErr := err
	if err != nil && proc == nil && CodeOf(err) == CodeInvalidState && !errors.Is(err, ErrStartUnconfirmed) {
		if ex.forget != nil {
			if forgetErr := ex.forget(); forgetErr != nil {
				sb.mu.Unlock()
				return Execution{}, false, forgetErr
			}
			sb.executions = sb.executions[:len(sb.executions)-1]
			delete(sb.idempotency, ex.key)
		}
		sb.mu.Unlock()
		return Execution{}, false, err
	}
	if err != nil && proc == nil {
		if s.journal != nil || req.TimeoutMS != nil || errors.Is(err, ErrStartUnconfirmed) {
			if s.journal == nil {
				sb.executions = append(sb.executions, ex)
				if ex.key != "" {
					sb.idempotency[ex.key] = ex
				}
			}
			removeErr := s.terminateLocked(context.WithoutCancel(ctx), sb, KillEvicted)
			ex.log.mu.Lock()
			ex.log.truncateLocked()
			ex.log.mu.Unlock()
			ex.finish(s.now(), -1, ExecOutcome{Reason: ReasonUnknown})
			sb.pruneExecutionsLocked()
			sb.mu.Unlock()
			message := "start unconfirmed; sandbox retired; retained execution " + ex.id
			if removeErr != nil {
				message += "; cleanup unconfirmed: " + removeErr.Error()
			}
			return Execution{}, false, &Error{Code: CodeBackendError, Message: message, Err: err}
		}
		sb.mu.Unlock()
		return Execution{}, false, backendError(err)
	}

	ex.proc = proc
	if req.TimeoutSeconds != nil && *req.TimeoutSeconds > 0 {
		deadline := s.now().Add(time.Duration(*req.TimeoutSeconds) * time.Second)
		ex.deadline = &deadline
	}
	if req.TimeoutMS != nil {
		managed, ok := proc.(ManagedProcess)
		if !ok || (managed.PID() <= 0 && startErr == nil) {
			_ = s.terminateLocked(context.WithoutCancel(ctx), sb, KillRequested)
			ex.finish(s.now(), -1, ExecOutcome{Reason: ReasonUnknown})
			sb.mu.Unlock()
			return Execution{}, false, errorf(CodeBackendError, "runtime did not confirm managed identity")
		}
		ex.pid = managed.PID()
	}
	if s.journal == nil {
		sb.executions = append(sb.executions, ex)
	}
	sb.running[ex.id] = ex
	if ex.key != "" {
		sb.idempotency[ex.key] = ex
	}
	var persistErr error
	if ex.persist != nil {
		persistErr = ex.persist(ex.snapshotLocked(), nil)
	}
	sb.touchLocked(s.now())
	sb.pruneExecutionsLocked()
	out := ex.snapshotLocked()
	sb.mu.Unlock()

	contextLogger(ctx).InfoContext(ctx, "execution started", "sandbox_id", sandboxID, "execution_id", ex.id)
	go s.run(context.WithoutCancel(ctx), sb, ex)
	if persistErr != nil {
		return Execution{}, false, errorf(CodeUnavailable, "accepted execution %s could not persist its identity", ex.id)
	}
	if startErr != nil {
		return Execution{}, false, errorf(CodeBackendError, "managed identity unconfirmed; retained execution %s; do not start with a new key", ex.id)
	}
	return out, true, nil
}

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
	if managed, ok := ex.proc.(ManagedProcess); ok && managed.TimedOut() {
		ex.setIntent(ReasonTimeout)
	} else if ok && managed.Killed() {
		ex.setIntent(ReasonCanceled)
	}

	ex.mu.Lock()
	intent := ex.intent
	baseline := ex.oomBaseline
	ex.mu.Unlock()

	var outcome ExecOutcome
	if sb.opts.maxConcurrentExecutions == 1 {
		outcome = classify(context.WithoutCancel(context.Background()), s.backend, sb.container, baseline, exitCode, intent, s.sleep)
	} else {
		outcome = classifyWithAttribution(context.WithoutCancel(context.Background()), s.backend, sb.container, baseline, exitCode, intent, s.sleep, false)
	}
	if len(outcome.sampled) > 0 {
		contextLogger(ctx).InfoContext(ctx, "execution liveness settled", "sandbox_id", ex.sandboxID, "execution_id", ex.id, "samples", outcome.sampled, "duration_ms", outcome.elapsed.Milliseconds(), "reason", outcome.Reason)
	}
	if waitErr != nil && intent == nil && !outcome.SandboxLost {
		exitCode = -1
		if outcome.Reason == ReasonExited {
			outcome.Reason = ReasonUnknown
		}
	}

	if errors.Is(waitErr, ErrGuestLost) && !outcome.SandboxLost {
		exitCode = -1
		outcome = ExecOutcome{Reason: ReasonUnknown, SandboxLost: true}
		ex.log.mu.Lock()
		if !ex.log.truncated {
			ex.log.truncateLocked()
		}
		ex.log.mu.Unlock()
	}
	now := s.now()
	sb.mu.Lock()
	if ex.finish(now, exitCode, outcome) {
		delete(sb.running, ex.id)
		sb.lastActivity = now
		_ = s.saveSandboxLocked(sb)
	}
	if outcome.SandboxLost {
		_ = s.terminateLocked(context.WithoutCancel(context.Background()), sb, KillEvicted)
	}
	sb.pruneExecutionsLocked()
	sb.mu.Unlock()
}

func pump(log *eventLog, kind EventType, r io.Reader) {
	if r == nil {
		return
	}
	buf := make([]byte, MaxEventDataBytes)
	carry := 0
	for {
		n, err := r.Read(buf[carry:])
		if gap, ok := err.(interface{ ResumeOffset() int64 }); ok {
			carry = 0
			log.resumeOutput(kind, gap.ResumeOffset())
			continue
		}
		total := carry + n

		tail := 0
		if err == nil && total > 0 {
			tail = incompleteRuneTail(buf[:total])
		}
		if total-tail > 0 {
			log.appendOutput(kind, buf[:total-tail])
		}
		carry = tail
		if tail > 0 {
			copy(buf, buf[total-tail:total])
		}
		if err != nil {
			return
		}
	}
}

func (s *Sandboxes) CancelExecution(ctx context.Context, sandboxID, executionID string) (Execution, error) {
	sb, ex, err := s.execution(sandboxID, executionID)
	if err != nil {
		return Execution{}, err
	}
	if ex.completed() {
		return ex.snapshot(), nil
	}

	sb.mu.Lock()
	if sb.state == StatePaused {
		if err := s.unpauseLocked(ctx, sb); err != nil {
			sb.mu.Unlock()
			return Execution{}, err
		}
	}
	sb.mu.Unlock()

	cleanup := context.WithoutCancel(ctx)
	pending := &intentRecord{reason: ReasonCanceled}
	ex.mu.Lock()
	if ex.intent == nil && ex.state == ExecRunning {
		ex.intent = pending
	}
	ex.mu.Unlock()
	if err := ex.proc.Signal(SignalTerm); CodeOf(err) == CodeInvalidState {
		ex.mu.Lock()
		if ex.intent == pending {
			ex.intent = nil
		}
		ex.mu.Unlock()
		return Execution{}, err
	}
	if s.waitDone(cleanup, ex.done, s.cancelGrace) {
		return ex.snapshot(), nil
	}
	_ = ex.proc.Signal(SignalKill)
	if s.waitDone(cleanup, ex.done, s.cancelDeadline-s.cancelGrace) {
		return ex.snapshot(), nil
	}

	sb.mu.Lock()
	_ = s.terminateLocked(cleanup, sb, KillRequested)
	sb.mu.Unlock()
	s.waitDone(cleanup, ex.done, s.cancelGrace)
	return ex.snapshot(), nil
}

func (s *Sandboxes) timeoutExecution(ctx context.Context, sb *sandbox, ex *execution) {
	if s.waitDone(ctx, ex.done, 0) {
		return
	}

	sb.mu.Lock()
	if sb.state == StatePaused {
		callCtx, cancel := backendCtx(ctx)
		err := s.backend.Unpause(callCtx, sb.container)
		cancel()
		if err != nil {
			_ = s.terminateLocked(ctx, sb, KillTimeout)
			sb.mu.Unlock()
			return
		}
		sb.state = StateRunning
		sb.armLifetimeLocked(s.now())
		if saveErr := s.saveSandboxLocked(sb); saveErr != nil {
			contextLogger(ctx).WarnContext(ctx, "cannot persist timeout unpause", "sandbox_id", sb.id)
		}
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
	_ = s.terminateLocked(cleanup, sb, KillTimeout)
	sb.mu.Unlock()
}

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

func (s *Sandboxes) GetExecution(sandboxID, executionID string) (Execution, error) {
	_, ex, err := s.execution(sandboxID, executionID)
	if err != nil {
		return Execution{}, err
	}
	return ex.snapshot(), nil
}

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
		out = append(out, list[i].summary())
	}
	return out, nil
}

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

type Stream struct {
	log    *eventLog
	cursor uint64
	once   sync.Once
	close  func()
}

func (s *Sandboxes) Events(sandboxID, executionID string, lastEventID uint64) (*Stream, error) {
	_, ex, err := s.execution(sandboxID, executionID)
	if err != nil {
		return nil, err
	}
	if ex.outputLost && lastEventID != 0 {
		return nil, errorf(CodeInvalidState, "live output lost on restart; read execution outcome or replay from zero")
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

func (st *Stream) Next(ctx context.Context) (events []Event, done bool, err error) {
	events, done, err = st.log.wait(ctx, st.cursor)
	if len(events) > 0 {
		st.cursor = events[len(events)-1].ID
	}
	return events, done, err
}

func (st *Stream) Close() {
	st.once.Do(st.close)
}

func (s *Sandboxes) LastEventID(sandboxID, executionID string) (uint64, error) {
	_, ex, err := s.execution(sandboxID, executionID)
	if err != nil {
		return 0, err
	}
	return ex.log.lastEventID(), nil
}

func (s *Sandboxes) ReadFile(ctx context.Context, sandboxID, p string) ([]byte, error) {
	if err := validateRegularFilePath(p); err != nil {
		return nil, err
	}
	p = path.Clean(p)
	ctx, cancel := context.WithTimeout(ctx, FileOperationTimeout)
	defer cancel()
	sb, err := s.prepareFiles(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	sb.mu.Lock()
	container := sb.container
	sb.mu.Unlock()

	data, err := s.backend.ReadFile(ctx, container, p)
	if err != nil {
		return nil, backendError(err)
	}
	return data, nil
}

func (s *Sandboxes) WriteFile(ctx context.Context, sandboxID, p string, data []byte) (FileEntry, error) {
	if err := validateRegularFilePath(p); err != nil {
		return FileEntry{}, err
	}
	p = path.Clean(p)
	if len(data) > MaxFileBytes {
		return FileEntry{}, errorf(CodePayloadTooLarge, "file exceeds %d bytes", MaxFileBytes)
	}
	ctx, cancel := context.WithTimeout(ctx, FileOperationTimeout)
	defer cancel()
	sb, err := s.prepareFiles(ctx, sandboxID)
	if err != nil {
		return FileEntry{}, err
	}
	sb.mu.Lock()
	container := sb.container
	sb.mu.Unlock()

	entry, err := s.backend.WriteFile(ctx, container, p, data)
	if err != nil {
		return FileEntry{}, backendError(err)
	}
	return entry, nil
}

func validateRegularFilePath(p string) error {
	if err := ValidateFilePath(p); err != nil {
		return err
	}
	if path.Clean(p) == "/" {
		return errorf(CodeInvalidRequest, "path must identify a regular file")
	}
	return nil
}

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

func sameInvocation(ex *execution, req ExecuteRequest) bool {
	return ex.log.keepTail == req.OutputTail && equalStrings(ex.command, req.Command) &&
		((ex.timeoutMS == nil && req.TimeoutMS == nil) || (ex.timeoutMS != nil && req.TimeoutMS != nil && *ex.timeoutMS == *req.TimeoutMS)) &&
		ex.envCount == len(req.Env) &&
		ex.envDigest == keyedFingerprint(ex.fingerprintKey, req.envPairs()) &&
		ex.cwd == req.Cwd &&
		ex.stdinDigest == keyedFingerprint(ex.fingerprintKey, []string{req.Stdin})
}

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

func cwdPtr(cwd string) *string {
	if cwd == "" {
		return nil
	}
	return &cwd
}

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
