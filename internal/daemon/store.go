package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

type SleepFunc func(ctx context.Context, d time.Duration) bool

type Option func(*Sandboxes)

func WithClock(c Clock) Option { return func(s *Sandboxes) { s.clock = c } }

func WithCleanupInterval(d time.Duration) Option {
	return func(s *Sandboxes) { s.cleanupInterval = d }
}

func WithClassifierSleep(f SleepFunc) Option { return func(s *Sandboxes) { s.sleep = f } }

func WithOwnerLabel(owner string) Option { return func(s *Sandboxes) { s.owner = owner } }

func WithCancelGrace(d time.Duration) Option { return func(s *Sandboxes) { s.cancelGrace = d } }

func WithCancelDeadline(d time.Duration) Option { return func(s *Sandboxes) { s.cancelDeadline = d } }

func WithMaxSandboxes(n int) Option { return func(s *Sandboxes) { s.maxSandboxes = n } }

func WithMaxRetainedSandboxes(n int) Option {
	return func(s *Sandboxes) { s.maxRetainedSandboxes = n }
}

func WithMaxRetainedOutputBytes(n int) Option {
	return func(s *Sandboxes) { s.maxRetainedOutput = n }
}

const OwnerLabelKey = "tiny-sandbox.owner"
const SandboxIDLabelKey = "tiny-sandbox.id"

const (
	ambiguousWindow     = 2 * time.Second
	ambiguousInterval   = 50 * time.Millisecond
	ambiguousMinSamples = 3
	ambiguousSamples    = int(ambiguousWindow / ambiguousInterval)
)

type Sandboxes struct {
	backend        Backend
	metrics        metrics
	clock          Clock
	journal        *Journal
	fingerprintKey [32]byte
	sleep          SleepFunc

	cleanupInterval time.Duration
	owner           string
	cancelGrace     time.Duration
	cancelDeadline  time.Duration

	maxSandboxes         int
	maxRetainedSandboxes int
	maxRetainedOutput    int

	mu    sync.Mutex
	byID  map[string]*sandbox
	order []*sandbox

	retryMu            sync.Mutex
	reservationLive    map[string]bool
	retryIDs           map[string]string
	removalPassRunning bool
}

const removalPassBudget = 2 * time.Second

func New(backend Backend, opts ...Option) *Sandboxes {
	s := &Sandboxes{
		backend:              backend,
		fingerprintKey:       invocationFingerprintKey,
		clock:                realClock{},
		cleanupInterval:      time.Second,
		cancelGrace:          2 * time.Second,
		cancelDeadline:       10 * time.Second,
		maxSandboxes:         MaxSandboxes,
		maxRetainedSandboxes: MaxRetainedSandboxes,
		maxRetainedOutput:    MaxRetainedOutputBytes,
		sleep:                sleepContext,
		byID:                 make(map[string]*sandbox),
		reservationLive:      make(map[string]bool),
		retryIDs:             make(map[string]string),
	}
	for _, opt := range opts {
		opt(s)
	}
	if s.cleanupInterval <= 0 || s.cleanupInterval > time.Second {
		s.cleanupInterval = time.Second
	}
	if s.cancelGrace <= 0 {
		s.cancelGrace = 2 * time.Second
	}
	if s.cancelDeadline <= s.cancelGrace {
		s.cancelDeadline = 10 * time.Second
	}
	if s.maxSandboxes <= 0 {
		s.maxSandboxes = MaxSandboxes
	}
	if s.maxRetainedSandboxes <= 0 {
		s.maxRetainedSandboxes = MaxRetainedSandboxes
	}
	if s.maxRetainedOutput <= 0 {
		s.maxRetainedOutput = MaxRetainedOutputBytes
	}
	return s
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (s *Sandboxes) now() time.Time { return s.clock.Now().UTC() }

type sandbox struct {
	agentMode string
	protected bool
	mu        sync.Mutex

	id        string
	container string
	opts      sandboxOptions
	createdAt time.Time

	state        State
	forgotten    bool
	dirty        bool
	killReason   *KillReason
	expiresAt    *time.Time
	lastActivity time.Time

	executions  []*execution
	running     map[string]*execution
	idempotency map[string]*execution

	terminals map[string]bool
	streams   int
}

func (sb *sandbox) pruneExecutionsLocked() {
	for len(sb.executions) > MaxExecutionsPerSandbox {
		if _, ok := sb.dropOldestLocked(); !ok {
			return
		}
	}
}

func (s *Sandboxes) Create(ctx context.Context, req CreateRequest) (result Sandbox, retErr error) {
	start := time.Now()
	defer func() {
		outcome := "success"
		if retErr != nil {
			outcome = string(CodeOf(retErr))
		}
		s.metrics.observe("tiny_sandbox_create_seconds{outcome="+fmt.Sprintf("%q", outcome)+"}", start)
	}()
	if err := s.durableReady(); err != nil {
		return Sandbox{}, err
	}
	if defaults, ok := s.backend.(interface {
		DefaultNetwork() NetworkMode
		DefaultNanoCPUs() int64
	}); ok {
		if req.Network == nil {
			req.Network = new(defaults.DefaultNetwork())
		}
		if req.NanoCPUs == nil {
			req.NanoCPUs = new(defaults.DefaultNanoCPUs())
		}
	}
	if defaults, ok := s.backend.(interface{ DefaultRuntime() string }); ok && req.Runtime == "" {
		req.Runtime = defaults.DefaultRuntime()
	}
	if req.Runtime != "" {
		if _, ok := s.backend.(interface{ DefaultRuntime() string }); !ok {
			return Sandbox{}, errorf(CodeInvalidRequest, "runtime selection requires a deployment policy")
		}
	}
	opts, err := req.resolve()
	if err != nil {
		return Sandbox{}, err
	}
	spec := ContainerSpec{Image: opts.image, Runtime: opts.runtime, MemoryMB: opts.memoryMB, PidsLimit: opts.pidsLimit, NanoCPUs: opts.nanoCPUs, Network: opts.network}
	id := newID("sbx_")
	spec.Name = "tiny-sandbox-" + id
	spec.Labels = map[string]string{OwnerLabelKey: s.owner, SandboxIDLabelKey: id}
	if err := s.reserve(id); err != nil {
		return Sandbox{}, err
	}

	now := s.now()
	sb := &sandbox{
		id:           id,
		opts:         opts,
		createdAt:    now,
		state:        StatePending,
		lastActivity: now,
		idempotency:  make(map[string]*execution),
		running:      make(map[string]*execution),
	}

	s.mu.Lock()
	s.byID[id] = sb
	s.order = append(s.order, sb)
	s.mu.Unlock()

	sb.mu.Lock()
	err = s.saveSandboxLocked(sb)
	sb.mu.Unlock()
	if err != nil {
		s.confirmRemoval(id, "")
		s.forget(sb)
		return Sandbox{}, err
	}
	containerID, err := s.backend.Create(ctx, spec)
	sb.mu.Lock()
	sb.container = containerID
	if err != nil || sb.state != StatePending {
		s.killLocked(sb, KillRequested)
		var cleanupErr error
		if containerID != "" {
			cleanupErr = s.removeOrQueue(context.WithoutCancel(ctx), id, containerID)
		} else {
			s.confirmRemoval(id, "")
		}
		out := sb.snapshot()
		sb.mu.Unlock()
		if err != nil {
			s.forget(sb)
			return Sandbox{}, backendError(err)
		}
		return out, cleanupErr
	}
	sb.agentMode, sb.protected = agentInfo(s.backend, containerID)
	sb.state = StateRunning
	sb.armLifetimeLocked(s.now())
	err = s.saveSandboxLocked(sb)
	if err != nil {
		_ = s.terminateLocked(ctx, sb, KillEvicted)
		sb.mu.Unlock()
		return Sandbox{}, err
	}
	out := sb.snapshot()
	sb.mu.Unlock()
	return out, nil
}

func (s *Sandboxes) reserve(id string) error {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	if len(s.reservationLive) >= s.maxSandboxes {
		return errorf(CodeRateLimited,
			"all %d sandbox slots are reserved for creation, use or cleanup", s.maxSandboxes)
	}
	s.reservationLive[id] = true
	return nil
}

func (s *Sandboxes) forget(sb *sandbox) {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	if s.journal != nil {
		history := sb.executions
		sb.executions = nil
		err := s.saveSandboxLocked(sb)
		sb.executions = history
		if err != nil {
			return
		}
		for _, ex := range sb.executions {
			s.journal.remove(sb.id + "-" + ex.id + ".json")
		}
		if s.journal.health() != nil {
			return
		}
		s.journal.remove(sb.id + ".json")
		if s.journal.health() != nil {
			return
		}
	}
	sb.forgotten = true
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byID, sb.id)
	for i, other := range s.order {
		if other == sb {
			s.order = append(s.order[:i], s.order[i+1:]...)
			return
		}
	}
}

func (s *Sandboxes) Get(id string) (Sandbox, error) {
	sb, err := s.lookup(id)
	if err != nil {
		return Sandbox{}, err
	}
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.snapshot(), nil
}

func (s *Sandboxes) List() []Sandbox {
	s.mu.Lock()
	order := append([]*sandbox(nil), s.order...)
	s.mu.Unlock()

	out := make([]Sandbox, 0, len(order))
	for i := len(order) - 1; i >= 0; i-- {
		order[i].mu.Lock()
		out = append(out, order[i].snapshot())
		order[i].mu.Unlock()
	}
	return out
}

func (s *Sandboxes) Touch(id string) {
	sb, err := s.lookup(id)
	if err != nil {
		return
	}
	sb.mu.Lock()
	if sb.state == StateRunning || sb.state == StatePaused {
		sb.touchLocked(s.now())
	}
	sb.mu.Unlock()
}

func (sb *sandbox) touchLocked(now time.Time) {
	if sb.opts.idleTimeoutSeconds > 0 && !now.Equal(sb.lastActivity) {
		sb.dirty = true
	}
	sb.lastActivity = now
}

func (s *Sandboxes) OpenStream(id string) (closeStream func(), err error) {
	sb, err := s.lookup(id)
	if err != nil {
		return nil, err
	}
	sb.mu.Lock()
	if sb.streams >= MaxStreamsPerSandbox {
		sb.mu.Unlock()
		return nil, errorf(CodeRateLimited, "sandbox %s has reached its open event stream limit", id)
	}
	sb.streams++
	if sb.state == StateRunning || sb.state == StatePaused {
		sb.touchLocked(s.now())
	}
	sb.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			sb.mu.Lock()
			sb.streams--
			if sb.state == StateRunning || sb.state == StatePaused {
				sb.touchLocked(s.now())
			}
			sb.mu.Unlock()
		})
	}, nil
}

func (s *Sandboxes) Update(_ context.Context, id string, req UpdateRequest) (result Sandbox, retErr error) {
	if req.TimeoutSeconds != nil && req.ExtendSeconds != nil {
		return Sandbox{}, errorf(CodeInvalidRequest, "timeout_seconds and extend_seconds are mutually exclusive")
	}
	if req.TimeoutSeconds == nil && req.ExtendSeconds == nil {
		return Sandbox{}, errorf(CodeInvalidRequest, "one of timeout_seconds or extend_seconds is required")
	}
	if req.TimeoutSeconds != nil && *req.TimeoutSeconds < 0 {
		return Sandbox{}, errorf(CodeInvalidRequest, "timeout_seconds must not be negative")
	}
	if req.ExtendSeconds != nil && *req.ExtendSeconds < 0 {
		return Sandbox{}, errorf(CodeInvalidRequest, "extend_seconds must not be negative")
	}

	sb, err := s.lookup(id)
	if err != nil {
		return Sandbox{}, err
	}
	sb.mu.Lock()
	defer sb.mu.Unlock()
	if sb.state != StateRunning && sb.state != StatePaused {
		return Sandbox{}, sb.invalidState()
	}
	now := s.now()
	switch {
	case req.TimeoutSeconds != nil:
		sb.opts.timeoutSeconds = *req.TimeoutSeconds
		if *req.TimeoutSeconds == 0 {
			sb.expiresAt = nil
			if err := s.saveSandboxLocked(sb); err != nil {
				return Sandbox{}, err
			}
			return sb.snapshot(), nil
		}
		deadline := now.Add(time.Duration(*req.TimeoutSeconds) * time.Second)
		sb.expiresAt = &deadline
	case *req.ExtendSeconds == 0:
		return sb.snapshot(), nil
	default:
		deadline := now.Add(time.Duration(*req.ExtendSeconds) * time.Second)
		if sb.expiresAt == nil || deadline.After(*sb.expiresAt) {
			if *req.ExtendSeconds > sb.opts.timeoutSeconds {
				sb.opts.timeoutSeconds = *req.ExtendSeconds
			}
			sb.expiresAt = &deadline
		} else {
			return sb.snapshot(), nil
		}
	}
	if err := s.saveSandboxLocked(sb); err != nil {
		return Sandbox{}, err
	}
	return sb.snapshot(), nil
}

func (s *Sandboxes) Pause(ctx context.Context, id string) (result Sandbox, retErr error) {
	sb, err := s.lookup(id)
	if err != nil {
		return Sandbox{}, err
	}
	sb.mu.Lock()
	defer sb.mu.Unlock()

	switch sb.state {
	case StatePaused:
		return sb.snapshot(), nil
	case StateRunning:
	default:
		return Sandbox{}, sb.invalidState()
	}

	callCtx, cancel := backendCtx(ctx)
	err = s.backend.Pause(callCtx, sb.container)
	cancel()
	if err != nil {
		return Sandbox{}, s.backendFailure(ctx, sb, err)
	}
	sb.state = StatePaused
	if sb.opts.timeoutAction == ActionPause {
		sb.expiresAt = nil
	}
	if err := s.saveSandboxLocked(sb); err != nil {
		return Sandbox{}, err
	}
	return sb.snapshot(), nil
}

func (s *Sandboxes) Resume(ctx context.Context, id string) (Sandbox, error) {
	sb, err := s.lookup(id)
	if err != nil {
		return Sandbox{}, err
	}
	sb.mu.Lock()
	defer sb.mu.Unlock()

	switch sb.state {
	case StateRunning:
		return sb.snapshot(), nil
	case StatePaused:
	default:
		return Sandbox{}, sb.invalidState()
	}
	if err := s.unpauseLocked(ctx, sb); err != nil {
		return Sandbox{}, err
	}
	return sb.snapshot(), nil
}

func (s *Sandboxes) unpauseLocked(ctx context.Context, sb *sandbox) error {
	callCtx, cancel := backendCtx(ctx)
	err := s.backend.Unpause(callCtx, sb.container)
	cancel()
	if err != nil {
		return s.backendFailure(ctx, sb, err)
	}
	sb.state = StateRunning
	sb.armLifetimeLocked(s.now())
	return s.saveSandboxLocked(sb)
}

func (s *Sandboxes) Delete(ctx context.Context, id string) (Sandbox, error) {
	sb, err := s.lookup(id)
	if err != nil {
		return Sandbox{}, err
	}
	sb.mu.Lock()
	defer sb.mu.Unlock()

	if sb.state == StateKilled {
		if sb.container != "" && s.removalPending(sb.container) {
			return sb.snapshot(), s.unconfirmedRemoval(sb.container)
		}
		return sb.snapshot(), nil
	}
	if err := s.terminateLocked(ctx, sb, KillRequested); err != nil {
		return sb.snapshot(), err
	}
	return sb.snapshot(), nil
}

func (s *Sandboxes) Destroy(ctx context.Context, id string) (Sandbox, error) {
	sb, err := s.lookup(id)
	if err != nil {
		return Sandbox{}, err
	}
	sb.mu.Lock()
	pending := sb.state == StatePending
	sb.mu.Unlock()
	if pending {
		return Sandbox{}, errorf(CodeInvalidState, "sandbox creation is pending")
	}
	result, err := s.Delete(ctx, id)
	if err != nil {
		return result, err
	}
	sb.mu.Lock()
	pendingWork := make([]<-chan struct{}, 0, len(sb.running))
	for _, ex := range sb.running {
		pendingWork = append(pendingWork, ex.done)
	}
	sb.mu.Unlock()
	for _, done := range pendingWork {
		select {
		case <-done:
		case <-ctx.Done():
			return result, errorf(CodeUnavailable, "sandbox cleanup is pending")
		}
	}
	s.forget(sb)
	return result, s.durableReady()
}

func (s *Sandboxes) unconfirmedRemoval(containerID string) error {
	return &Error{
		Code:    CodeBackendError,
		Message: "unconfirmed container removal: " + containerID + "; a retry is scheduled",
	}
}

func (s *Sandboxes) removalPending(containerID string) bool {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	_, ok := s.retryIDs[containerID]
	return ok
}

func (s *Sandboxes) killLocked(sb *sandbox, reason KillReason) {
	if sb.state == StateKilled {
		return
	}
	sb.state = StateKilled
	r := reason
	sb.killReason = &r
	sb.expiresAt = nil
	sb.dirty = true
	s.retryMu.Lock()
	if _, held := s.reservationLive[sb.id]; held {
		s.reservationLive[sb.id] = false
	}
	s.retryMu.Unlock()

	for id, ex := range sb.running {
		switch reason {
		case KillRequested:
			ex.setIntent(ReasonCanceled)
		case KillTimeout, KillIdleTimeout:
			ex.setIntent(ReasonTimeout)
		case KillEvicted:
			outcome := ExecOutcome{Reason: ReasonEvicted, ExitCode: -1, SandboxLost: true}
			ex.mu.Lock()
			if ex.intent != nil {
				outcome.Reason = ex.intent.reason
			}
			ex.mu.Unlock()
			ex.finish(s.now(), -1, outcome)
			delete(sb.running, id)
		}
	}
	sb.pruneExecutionsLocked()
}

func (s *Sandboxes) terminateLocked(ctx context.Context, sb *sandbox, reason KillReason) error {
	s.killLocked(sb, reason)
	persistErr := s.saveSandboxLocked(sb)
	if sb.container == "" {
		return persistErr
	}
	if err := s.removeOrQueue(context.WithoutCancel(ctx), sb.id, sb.container); err != nil {
		return err
	}
	return persistErr
}

func (s *Sandboxes) Shutdown(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownBudget)
	defer cancel()

	var wg sync.WaitGroup
	sem := make(chan struct{}, shutdownWorkers)
	for _, sb := range s.sandboxesInCreationOrder() {
		sb.mu.Lock()
		s.killLocked(sb, KillRequested)
		container := sb.container
		sb.mu.Unlock()
		if container == "" {
			continue
		}
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			_ = s.removeOrQueue(ctx, sb.id, container)
		})
	}
	wg.Wait()
	s.runRemovalPass(ctx)

	s.mu.Lock()
	s.byID = make(map[string]*sandbox)
	s.order = nil
	s.mu.Unlock()
}

const (
	shutdownBudget  = 10 * time.Second
	shutdownWorkers = 8
)

func (s *Sandboxes) lookup(id string) (*sandbox, error) {
	s.mu.Lock()
	sb := s.byID[id]
	s.mu.Unlock()
	if sb == nil {
		return nil, errorf(CodeNotFound, "sandbox %s does not exist", id)
	}
	return sb, nil
}

func (s *Sandboxes) backendFailure(_ context.Context, sb *sandbox, err error) error {
	if errors.Is(err, ErrNotFound) {
		s.killLocked(sb, KillEvicted)
		s.confirmRemoval(sb.id, sb.container)
		if saveErr := s.saveSandboxLocked(sb); saveErr != nil {
			return saveErr
		}
		return errorf(CodeInvalidState, "sandbox %s was evicted", sb.id)
	}
	return backendError(err)
}

func (sb *sandbox) armLifetimeLocked(now time.Time) {
	if sb.opts.timeoutSeconds == 0 || sb.state == StateKilled || sb.state == StatePending {
		sb.expiresAt = nil
		return
	}
	limit := time.Duration(sb.opts.timeoutSeconds) * time.Second
	base := sb.createdAt
	if sb.opts.timeoutAction == ActionPause {
		if sb.state != StateRunning {
			sb.expiresAt = nil
			return
		}
		base = now
	}
	deadline := base.Add(limit)
	sb.expiresAt = &deadline
}

func (sb *sandbox) deadlined(now time.Time) bool {
	return sb.expiresAt != nil && !now.Before(*sb.expiresAt)
}

func (sb *sandbox) idleFor(now time.Time) time.Duration {
	if sb.state != StateRunning || len(sb.running) != 0 || sb.streams > 0 ||
		sb.opts.idleTimeoutSeconds <= 0 {
		return -1
	}
	return now.Sub(sb.lastActivity)
}

func (sb *sandbox) invalidState() error {
	return errorf(CodeInvalidState, "sandbox %s is %s", sb.id, sb.state)
}

func (sb *sandbox) snapshot() Sandbox {
	return Sandbox{
		AgentMode: cmp.Or(sb.agentMode, "legacy"), Protected: sb.protected,
		Runtime:                 sb.opts.runtime,
		MaxConcurrentExecutions: sb.opts.maxConcurrentExecutions,
		ID:                      sb.id,
		State:                   sb.state,
		Image:                   sb.opts.image,
		MemoryMB:                sb.opts.memoryMB,
		PidsLimit:               sb.opts.pidsLimit,
		NanoCPUs:                sb.opts.nanoCPUs,
		Network:                 sb.opts.network,
		TimeoutSeconds:          sb.opts.timeoutSeconds,
		TimeoutAction:           sb.opts.timeoutAction,
		IdleTimeoutSeconds:      sb.opts.idleTimeoutSeconds,
		IdleTimeoutAction:       sb.opts.idleTimeoutAction,
		CreatedAt:               sb.createdAt,
		ExpiresAt:               cloneTime(sb.expiresAt),
		KillReason:              cloneReason(sb.killReason),
	}
}
