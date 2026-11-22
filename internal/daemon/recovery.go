package daemon

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

// RecoveryBackend must enumerate exact owned IDs, not names or PID prefixes.
type RecoveryBackend interface {
	Owned(context.Context, string) ([]string, error)
}

// Journal is an opt-in, single-writer local control store. It is not a process
// checkpoint: uncertain commands are never restarted or silently reattached.
type Journal struct {
	mu      sync.Mutex
	dir     string
	owner   string
	key     [32]byte
	lock    *os.File
	failure error
}

type storedSandbox struct {
	Version      int               `json:"version"`
	Sandbox      Sandbox           `json:"sandbox"`
	Container    string            `json:"container"`
	Env          map[string]string `json:"env"`
	LastActivity time.Time         `json:"last_activity"`
	Executions   []string          `json:"executions"`
}
type storedExecution struct {
	Version     int       `json:"version"`
	Execution   Execution `json:"execution"`
	Key         string    `json:"key"`
	EnvDigest   [32]byte  `json:"env_digest"`
	EnvCount    int       `json:"env_count"`
	StdinDigest [32]byte  `json:"stdin_digest"`
	TimeoutMS   *int64    `json:"timeout_ms"`
	Events      []Event   `json:"events"`
	OutputLost  bool      `json:"output_lost"`
}
type journalHeader struct {
	Version int      `json:"version"`
	Owner   string   `json:"owner"`
	Key     [32]byte `json:"fingerprint_key"`
}

// OpenJournal validates ownership and holds a kernel lock until Close. A corrupt
// store or a second daemon fails before Docker is touched. Use local storage.
func OpenJournal(dir, owner string) (*Journal, error) {
	if owner == "" {
		return nil, errors.New("recovery requires a stable owner")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("state directory must be a private directory (0700)")
	}
	lock, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	lockInfo, err := lock.Stat()
	if err != nil || !lockInfo.Mode().IsRegular() || lockInfo.Mode().Perm()&0o077 != 0 {
		_ = lock.Close()
		return nil, errors.New("state lock must be a private regular file")
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("state directory already in use: %w", err)
	}
	j := &Journal{dir: dir, owner: owner, lock: lock, key: invocationFingerprintKey}
	header := journalHeader{Version: 1, Owner: owner, Key: j.key}
	err = j.read("metadata.json", &header)
	if errors.Is(err, os.ErrNotExist) {
		entries, readErr := os.ReadDir(dir)
		err = readErr
		for _, entry := range entries {
			if entry.Name() != "lock" {
				err = errors.New("metadata missing from nonempty state directory")
			}
		}
		if err == nil {
			err = j.write("metadata.json", header)
		}
	} else if err == nil && (header.Version != 1 || header.Owner != owner || header.Key == [32]byte{}) {
		err = errors.New("state directory version or owner mismatch")
	}
	if err != nil {
		_ = j.Close()
		return nil, err
	}
	j.key = header.Key
	return j, nil
}

// Close prevents late workers from writing after releasing the single-writer lock.
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.failure = errors.New("state directory is closed")
	return j.lock.Close()
}

func (j *Journal) health() error { j.mu.Lock(); defer j.mu.Unlock(); return j.failure }
func (j *Journal) read(name string, value any) error {
	info, err := os.Lstat(filepath.Join(j.dir, name))
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return errors.New("recovery records must be private regular files")
	}
	f, err := os.Open(filepath.Join(j.dir, name))
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	// A single retained execution is bounded; refuse unbounded or corrupt files.
	data, err := io.ReadAll(io.LimitReader(f, 8*MaxOutputBytes+1))
	if err != nil {
		return err
	}
	if len(data) > 8*MaxOutputBytes {
		return errors.New("oversized recovery record")
	}
	return json.Unmarshal(data, value, json.RejectUnknownMembers(true))
}
func (j *Journal) write(name string, value any) (err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.failure != nil {
		return j.failure
	}
	defer func() {
		if err != nil {
			j.failure = err
		}
	}()
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(j.dir, ".write-*")
	if err != nil {
		return err
	}
	defer func() { _ = f.Close(); _ = os.Remove(f.Name()) }()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), filepath.Join(j.dir, name)); err != nil {
		return err
	}
	return syncDirectory(j.dir)
}
func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return f.Sync()
}
func (j *Journal) remove(name string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.failure != nil {
		return
	}
	err := os.Remove(filepath.Join(j.dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err == nil {
		err = syncDirectory(j.dir)
	}
	if err != nil {
		j.failure = err
	}
}
func (s *Sandboxes) durableReady() error {
	if s.journal != nil && s.journal.health() != nil {
		return errorf(CodeUnavailable, "durable state unavailable; restart after repairing storage")
	}
	return nil
}
func (s *Sandboxes) saveSandboxLocked(sb *sandbox) error {
	return s.saveSandboxExceptLocked(sb, "")
}

func (s *Sandboxes) saveSandboxExceptLocked(sb *sandbox, omit string) error {
	if s.journal == nil || sb.forgotten {
		return nil
	}
	ids := make([]string, 0, len(sb.executions))
	for _, ex := range sb.executions {
		if ex.id != omit {
			ids = append(ids, ex.id)
		}
	}
	err := s.journal.write(sb.id+".json", storedSandbox{1, sb.snapshot(), sb.container, sb.opts.env, sb.lastActivity, ids})
	if err != nil {
		return errorf(CodeUnavailable, "cannot persist sandbox state")
	}
	sb.dirty = false
	return nil
}

// Remove the manifest entry first. A crash can retain extra history, never
// silently forget a live sandbox's acknowledged idempotency record.
func (s *Sandboxes) forgetExecution(sb *sandbox, ex *execution) error {
	if err := s.saveSandboxExceptLocked(sb, ex.id); err != nil {
		return err
	}
	s.journal.remove(sb.id + "-" + ex.id + ".json")
	return s.journal.health()
}
func (s *Sandboxes) saveExecution(ex *execution, snapshot Execution, events []Event) error {
	if s.journal == nil {
		return nil
	}
	snapshot.Stdout, snapshot.Stderr = "", ""
	return s.journal.write(ex.sandboxID+"-"+ex.id+".json", storedExecution{1, snapshot, ex.key, ex.envDigest, ex.envCount, ex.stdinDigest, ex.timeoutMS, events, ex.outputLost})
}

// Recover validates every record before reconciling Docker. It preserves idle
// containers, completes interrupted executions as unknown, and removes their
// containers and owned orphans before admitting traffic. It never executes code.
func (s *Sandboxes) Recover(ctx context.Context, j *Journal, backend RecoveryBackend) error {
	entries, err := os.ReadDir(j.dir)
	if err != nil {
		return err
	}
	var sandboxes []storedSandbox
	executions := map[string][]storedExecution{}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "sbx_") || !strings.HasSuffix(name, ".json") {
			continue
		}
		if strings.Contains(name, "-exe_") {
			var value storedExecution
			if err := j.read(name, &value); err != nil {
				return fmt.Errorf("read execution record: %w", err)
			}
			ex := value.Execution
			if value.Version != 1 || name != ex.SandboxID+"-"+ex.ID+".json" || !validStateID(ex.ID, "exe_") || !validStateID(ex.SandboxID, "sbx_") || (ex.State != ExecRunning && ex.State != ExecCompleted) {
				return errors.New("invalid execution record")
			}
			if len(value.Events) > MaxOutputEvents+2 {
				return errors.New("excess recovery events")
			}
			for i, ev := range value.Events {
				if ev.ID != uint64(i+1) || len(ev.Data) > MaxEventDataBytes {
					return errors.New("invalid recovery cursor")
				}
			}
			executions[ex.SandboxID] = append(executions[ex.SandboxID], value)
		} else {
			var value storedSandbox
			if err := j.read(name, &value); err != nil {
				return fmt.Errorf("read sandbox record: %w", err)
			}
			if value.Version != 1 || !validStateID(value.Sandbox.ID, "sbx_") || name != value.Sandbox.ID+".json" {
				return errors.New("invalid sandbox record")
			}
			sandboxes = append(sandboxes, value)
		}
	}
	// Validate the complete graph before even enumerating Docker. Corrupt
	// history must not turn recovery into partial adoption or deletion.
	seen := map[string]bool{}
	containers := map[string]bool{}
	for _, value := range sandboxes {
		v := value.Sandbox
		if _, err := recoveredOptions(value); err != nil {
			return err
		}
		if seen[v.ID] || (value.Container != "" && containers[value.Container]) || v.CreatedAt.IsZero() || value.LastActivity.IsZero() || value.Executions == nil {
			return errors.New("invalid sandbox recovery identity or manifest")
		}
		if v.State != StatePending && v.State != StateRunning && v.State != StatePaused && v.State != StateKilled {
			return errors.New("invalid recovered sandbox state")
		}
		seen[v.ID], containers[value.Container] = true, true
		ids, keys := map[string]bool{}, map[string]bool{}
		for _, item := range executions[v.ID] {
			if err := validateStoredExecution(item); err != nil {
				return err
			}
			if ids[item.Execution.ID] || (item.Key != "" && keys[item.Key]) {
				return errors.New("duplicate recovered execution or idempotency key")
			}
			ids[item.Execution.ID], keys[item.Key] = true, true
		}
		for _, id := range value.Executions {
			if !ids[id] {
				return errors.New("missing execution record for live sandbox")
			}
		}
	}
	for id := range executions {
		if !seen[id] {
			return errors.New("execution record has no sandbox manifest")
		}
	}
	owned, err := backend.Owned(ctx, j.owner)
	if err != nil {
		return err
	}
	ownedSet := map[string]bool{}
	for _, id := range owned {
		ownedSet[id] = true
	}
	s.journal = j
	s.fingerprintKey = j.key
	slices.SortFunc(sandboxes, func(a, b storedSandbox) int { return a.Sandbox.CreatedAt.Compare(b.Sandbox.CreatedAt) })
	for _, value := range sandboxes {
		v := value.Sandbox
		opts, err := recoveredOptions(value)
		if err != nil {
			return errors.New("invalid recovered sandbox options")
		}
		sb := &sandbox{id: v.ID, container: value.Container, opts: opts, createdAt: v.CreatedAt, state: v.State, killReason: v.KillReason, expiresAt: v.ExpiresAt, lastActivity: value.LastActivity, running: map[string]*execution{}, idempotency: map[string]*execution{}}
		unsafe := sb.state == StatePending || sb.state == StateKilled
		items := executions[sb.id]
		slices.SortFunc(items, func(a, b storedExecution) int { return a.Execution.StartedAt.Compare(b.Execution.StartedAt) })
		for _, item := range items {
			v := item.Execution
			ex := &execution{id: v.ID, sandboxID: sb.id, command: v.Command, key: item.Key, envDigest: item.EnvDigest, envCount: item.EnvCount, stdinDigest: item.StdinDigest, timeoutMS: item.TimeoutMS, fingerprintKey: j.key, pid: v.PID, startedAt: v.StartedAt, finished: v.FinishedAt, state: v.State, exitCode: v.ExitCode, reason: v.TerminationReason, log: newEventLog(), done: make(chan struct{})}
			if v.Cwd != nil {
				ex.cwd = *v.Cwd
			}
			ex.persist = func(snapshot Execution, events []Event) error { return s.saveExecution(ex, snapshot, events) }
			ex.forget = func() error { return s.forgetExecution(sb, ex) }
			ex.outputLost = item.OutputLost || v.State == ExecRunning
			ex.log.truncated = v.OutputTruncated
			for _, ev := range item.Events {
				ex.log.events = append(ex.log.events, ev)
				ex.log.nextID = ev.ID
				if ev.Type == EventStdout || ev.Type == EventStderr {
					ex.log.retained += len(ev.Data)
					ex.log.outputEvents++
				}
				if ev.Type == EventTruncated {
					ex.log.truncated = true
				}
				if ev.Type == EventComplete {
					ex.log.complete = true
				}
			}
			if ex.log.retained > MaxOutputBytes {
				return errors.New("excess recovery output")
			}
			if v.State == ExecRunning {
				unsafe = true
				// Live output was not durably committed. Do not replay an old cursor
				// as a complete log or infer success from the surviving container.
				ex.log.truncated = true
			} else {
				if !ex.log.complete || v.FinishedAt == nil || v.TerminationReason == nil {
					return errors.New("incomplete terminal recovery record")
				}
				close(ex.done)
			}
			sb.executions = append(sb.executions, ex)
			if ex.key != "" {
				sb.idempotency[ex.key] = ex
			}
		}
		if sb.container == "" || !ownedSet[sb.container] {
			unsafe = true
			sb.container = ""
		}
		if !unsafe {
			live, err := s.backend.Liveness(ctx, sb.container)
			if err != nil {
				return err
			}
			unsafe = !live.Alive()
			if !unsafe {
				if live == LivenessPaused {
					sb.state = StatePaused
				} else {
					sb.state = StateRunning
				}
				_, unsafe = sb.dueTimeout(s.now())
			}
		}
		if !unsafe {
			if adopter, ok := s.backend.(interface {
				Adopt(context.Context, string, ContainerSpec) error
			}); ok {
				// Paused containers cannot execute the kernel verification probe.
				unsafe = sb.state == StatePaused || adopter.Adopt(ctx, sb.container, ContainerSpec{Image: opts.image, MemoryMB: opts.memoryMB, PidsLimit: opts.pidsLimit, NanoCPUs: opts.nanoCPUs, Network: opts.network}) != nil
			}
		}
		if unsafe {
			// Commit the tombstone before completing uncertain records. A crash
			// here must never make a still-live container look safe to adopt.
			sb.state = StateKilled
			sb.released = true
			sb.expiresAt = nil
			if sb.killReason == nil {
				sb.killReason = new(KillEvicted)
			}
			if err := s.saveSandboxLocked(sb); err != nil {
				return err
			}
			if sb.container != "" {
				if err := s.backend.Remove(ctx, sb.container); err != nil && !errors.Is(err, ErrNotFound) {
					return err
				}
			}
		} else {
			s.live++
		}
		for _, ex := range sb.executions {
			if ex.state == ExecRunning {
				ex.finish(s.now(), -1, ExecOutcome{Reason: ReasonUnknown})
			}
		}
		delete(ownedSet, sb.container)
		s.byID[sb.id] = sb
		s.order = append(s.order, sb)
		if err := s.saveSandboxLocked(sb); err != nil {
			return err
		}
	}
	for id := range ownedSet {
		if err := s.backend.Remove(ctx, id); err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
	}
	s.prune()
	return s.durableReady()
}

// DrainPersistent preserves idle sandboxes but ends active work on graceful
// shutdown. Crash recovery remains conservative even if this drain fails.
func (s *Sandboxes) DrainPersistent(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, shutdownBudget)
	defer cancel()
	var workers sync.WaitGroup
	defer workers.Wait()
	slots := make(chan struct{}, shutdownWorkers)
	for _, sb := range s.sandboxesInCreationOrder() {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			return // Remaining live intents are reconciled on the next startup.
		}
		workers.Go(func() {
			defer func() { <-slots }()
			for !sb.mu.TryLock() {
				if !s.sleep(ctx, 10*time.Millisecond) {
					return
				}
			}
			var active []*execution
			for _, ex := range sb.running {
				active = append(active, ex)
			}
			if len(active) > 0 {
				s.killLocked(sb, KillRequested)
			}
			_ = s.saveSandboxLocked(sb)
			container := sb.container
			sb.mu.Unlock()
			if len(active) > 0 && container != "" {
				if err := s.removeBounded(ctx, container); err != nil && !errors.Is(err, ErrNotFound) {
					s.scheduleRemoval(container)
				}
			}
			for _, ex := range active {
				s.waitDone(ctx, ex.done, BackendCallTimeout)
			}
		})
	}
}

func validStateID(id, prefix string) bool {
	if len(id) != len(prefix)+16 || !strings.HasPrefix(id, prefix) {
		return false
	}
	for _, c := range id[len(prefix):] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func recoveredOptions(value storedSandbox) (sandboxOptions, error) {
	v := value.Sandbox
	return (CreateRequest{Image: &v.Image, MemoryMB: &v.MemoryMB, PidsLimit: &v.PidsLimit, NanoCPUs: &v.NanoCPUs, Network: &v.Network, TimeoutSeconds: &v.TimeoutSeconds, TimeoutAction: &v.TimeoutAction, IdleTimeoutSeconds: &v.IdleTimeoutSeconds, IdleTimeoutAction: &v.IdleTimeoutAction, MaxConcurrentExecutions: &v.MaxConcurrentExecutions, Env: value.Env}).resolve()
}

func validateStoredExecution(value storedExecution) error {
	v := value.Execution
	if len(v.Command) == 0 || v.StartedAt.IsZero() || v.PID < 0 || len(value.Key) > MaxIdempotencyKey || value.EnvCount < 0 || value.EnvCount > MaxEnvEntries {
		return errors.New("invalid recovered invocation")
	}
	bytes, completed := 0, false
	for i, ev := range value.Events {
		if completed || ev.ID != uint64(i+1) {
			return errors.New("invalid recovered event order")
		}
		switch ev.Type {
		case EventStdout, EventStderr:
			bytes += len(ev.Data)
		case EventTruncated:
		case EventComplete:
			completed = true
			if ev.Execution == nil || !reflect.DeepEqual(*ev.Execution, v) {
				return errors.New("invalid recovered terminal event")
			}
		default:
			return errors.New("unknown recovered event")
		}
	}
	if bytes > MaxOutputBytes || (v.State == ExecCompleted) != completed {
		return errors.New("inconsistent recovered execution output")
	}
	if completed && (v.FinishedAt == nil || v.TerminationReason == nil) {
		return errors.New("missing recovered terminal outcome")
	}
	return nil
}
