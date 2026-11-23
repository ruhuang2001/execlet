package daemon

import (
	"context"
	"errors"
	"time"
)

func backendCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, BackendCallTimeout)
}

func (sb *sandbox) dueTimeout(now time.Time) (reason KillReason, ok bool) {
	if sb.opts.timeoutSeconds > 0 && sb.deadlined(now) {
		return KillTimeout, true
	}
	if idle := sb.idleFor(now); idle >= 0 && idle >= time.Duration(sb.opts.idleTimeoutSeconds)*time.Second {
		return KillIdleTimeout, true
	}
	return "", false
}

func (sb *sandbox) dueExecutionTimeout(now time.Time) (*execution, bool) {
	for _, ex := range sb.running {
		ex.mu.Lock()
		due := ex.state == ExecRunning && ex.deadline != nil && !ex.timedOut && !now.Before(*ex.deadline)
		if due {
			ex.timedOut = true
		}
		ex.mu.Unlock()
		if due {
			return ex, true
		}
	}
	return nil, false
}

func (s *Sandboxes) Reap(ctx context.Context) {
	s.mu.Lock()
	order := append([]*sandbox(nil), s.order...)
	s.mu.Unlock()

	now := s.now()
	for _, sb := range order {
		if !sb.mu.TryLock() {
			continue
		}
		s.reapOne(ctx, sb, now)
		if s.journal != nil && sb.dirty {
			_ = s.saveSandboxLocked(sb)
		}
		sb.mu.Unlock()
	}
}

func (s *Sandboxes) reapOne(ctx context.Context, sb *sandbox, now time.Time) {
	if sb.state != StateRunning && sb.state != StatePaused {
		return
	}

	reason, due := sb.dueTimeout(now)
	if !due {
		if sb.state == StateRunning {
			for {
				ex, ok := sb.dueExecutionTimeout(now)
				if !ok {
					break
				}
				ex.setIntent(ReasonTimeout)
				go s.timeoutExecution(context.WithoutCancel(ctx), sb, ex)
			}
		}
		return
	}

	action := sb.opts.timeoutAction
	if reason == KillIdleTimeout {
		action = sb.opts.idleTimeoutAction
	}

	switch action {
	case ActionPause:
		if sb.state != StateRunning {
			return
		}
		callCtx, cancel := backendCtx(ctx)
		err := s.backend.Pause(callCtx, sb.container)
		cancel()
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				s.killLocked(sb, KillEvicted)
				s.confirmRemoval(sb.id, sb.container)
				return
			}
			if liveness, livenessErr := s.liveness(ctx, sb.container); livenessErr == nil &&
				liveness == LivenessPaused {
				sb.state = StatePaused
				sb.expiresAt = nil
				sb.dirty = true
			}
			return
		}
		sb.state = StatePaused
		sb.expiresAt = nil
		sb.dirty = true
	case ActionKill:
		_ = s.terminateLocked(ctx, sb, reason)
	}
}

func (s *Sandboxes) liveness(ctx context.Context, containerID string) (Liveness, error) {
	callCtx, cancel := backendCtx(ctx)
	defer cancel()
	return s.backend.Liveness(callCtx, containerID)
}

func (s *Sandboxes) retryRemovals(ctx context.Context) {
	if !s.tryStartRemovalPass() {
		return
	}
	passCtx, cancel := context.WithTimeout(ctx, removalPassBudget)
	go func() {
		defer cancel()
		defer s.finishRemovalPass()
		s.runRemovalPass(passCtx)
	}()
}

func (s *Sandboxes) runRemovalPass(ctx context.Context) {
	for _, id := range s.pendingRemovals() {
		if ctx.Err() != nil {
			return
		}
		err := s.removeBounded(ctx, id)
		if err == nil || errors.Is(err, ErrNotFound) {
			s.confirmRemoval("", id)
		} else {
			s.metrics.cleanupFailed()
		}
	}
}

func (s *Sandboxes) pendingRemovals() []string {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	ids := make([]string, 0, len(s.retryIDs))
	for id := range s.retryIDs {
		ids = append(ids, id)
	}
	return ids
}

func (s *Sandboxes) removeBounded(ctx context.Context, containerID string) error {
	callCtx, cancel := context.WithTimeout(ctx, BackendCallTimeout)
	defer cancel()
	return s.backend.Remove(callCtx, containerID)
}

func (s *Sandboxes) confirmRemoval(sandboxID, containerID string) {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	if sandboxID == "" {
		sandboxID = s.retryIDs[containerID]
	}
	delete(s.reservationLive, sandboxID)
	delete(s.retryIDs, containerID)
}

func (s *Sandboxes) tryStartRemovalPass() bool {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	if s.removalPassRunning {
		return false
	}
	s.removalPassRunning = true
	return true
}

func (s *Sandboxes) finishRemovalPass() {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	s.removalPassRunning = false
}

func (s *Sandboxes) queueStandbyRemoval(containerID string) {
	s.metrics.cleanupFailed()
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	s.retryIDs[containerID] = ""
}

func (s *Sandboxes) removeOrQueue(ctx context.Context, sandboxID, containerID string) error {
	if err := s.removeBounded(ctx, containerID); err == nil || errors.Is(err, ErrNotFound) {
		s.confirmRemoval(sandboxID, containerID)
		return nil
	}
	s.metrics.cleanupFailed()
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	if _, held := s.reservationLive[sandboxID]; !held {
		return nil
	}
	s.retryIDs[containerID] = sandboxID
	return s.unconfirmedRemoval(containerID)
}

type BatchLivenessBackend interface {
	OwnedLiveness(ctx context.Context, owner string) (map[string]Liveness, error)
}

func (s *Sandboxes) EvictSweep(ctx context.Context) {
	s.mu.Lock()
	order := append([]*sandbox(nil), s.order...)
	s.mu.Unlock()

	if s.owner != "" {
		if batch, ok := s.backend.(BatchLivenessBackend); ok {
			type candidate struct {
				sb        *sandbox
				container string
			}
			var candidates []candidate
			for _, sb := range order {
				if !sb.mu.TryLock() {
					continue
				}
				if (sb.state == StateRunning || sb.state == StatePaused) && sb.container != "" {
					candidates = append(candidates, candidate{sb: sb, container: sb.container})
				}
				sb.mu.Unlock()
			}
			if len(candidates) == 0 {
				return
			}
			callCtx, cancel := backendCtx(ctx)
			states, err := batch.OwnedLiveness(callCtx, s.owner)
			cancel()
			if errors.Is(err, errors.ErrUnsupported) {
				s.sweepIndividually(ctx, order)
				return
			}
			if err != nil {
				return
			}
			for _, c := range candidates {
				if state, present := states[c.container]; present && state != LivenessStopped && state != LivenessMissing {
					continue
				}
				if !c.sb.mu.TryLock() {
					continue
				}
				if (c.sb.state == StateRunning || c.sb.state == StatePaused) && c.sb.container == c.container {
					_ = s.terminateLocked(ctx, c.sb, KillEvicted)
				}
				c.sb.mu.Unlock()
			}
			return
		}
	}
	s.sweepIndividually(ctx, order)
}

func (s *Sandboxes) sweepIndividually(ctx context.Context, order []*sandbox) {
	for _, sb := range order {
		if !sb.mu.TryLock() {
			continue
		}
		if sb.state == StateRunning || sb.state == StatePaused {
			callCtx, cancel := backendCtx(ctx)
			liveness, err := s.backend.Liveness(callCtx, sb.container)
			cancel()
			switch {
			case err != nil:
			case liveness == LivenessStopped || liveness == LivenessMissing:
				_ = s.terminateLocked(ctx, sb, KillEvicted)
			}
		}
		sb.mu.Unlock()
	}
}

func (s *Sandboxes) Run(ctx context.Context) {
	if b, ok := s.backend.(*PolicyBackend); ok && len(b.targets) > 0 {
		done := make(chan struct{})
		go func() { defer close(done); b.maintainPrewarm(ctx, s.owner, s.queueStandbyRemoval) }()
		defer func() { <-done }()
	}
	tick := time.NewTicker(s.cleanupInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.Reap(ctx)
			s.EvictSweep(ctx)
			s.retryRemovals(ctx)
			s.prune()
		}
	}
}
