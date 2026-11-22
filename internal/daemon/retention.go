package daemon

import "time"

func (s *Sandboxes) prune() {
	s.pruneKilled()
	s.pruneOutput()
}

func (s *Sandboxes) pruneKilled() {
	order := s.sandboxesInCreationOrder()

	killed := 0
	for _, sb := range order {
		if sb.isKilled() {
			killed++
		}
	}
	drop := killed - s.maxRetainedSandboxes
	if drop <= 0 {
		return
	}
	for _, sb := range order {
		if drop == 0 {
			return
		}
		if sb.isKilled() {
			s.forget(sb)
			drop--
		}
	}
}

func (s *Sandboxes) pruneOutput() {
	order := s.sandboxesInCreationOrder()

	total := 0
	for _, sb := range order {
		if sb.mu.TryLock() {
			total += sb.retainedBytesLocked()
			sb.mu.Unlock()
		}
	}

	for _, sb := range order {
		if total <= s.maxRetainedOutput {
			return
		}
		if !sb.mu.TryLock() {
			continue
		}
		for total > s.maxRetainedOutput {
			size, ok := sb.dropOldestLocked()
			if !ok {
				break
			}
			total -= size
		}
		sb.mu.Unlock()
	}
}

func (s *Sandboxes) sandboxesInCreationOrder() []*sandbox {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*sandbox(nil), s.order...)
}

func (sb *sandbox) retainedBytesLocked() int {
	total := 0
	for _, ex := range sb.executions {
		total += ex.retained()
	}
	return total
}

func (sb *sandbox) oldestDroppable() *execution {
	var newestCompleted *execution
	var newestFinish time.Time
	for _, ex := range sb.executions {
		ex.mu.Lock()
		if ex.finished != nil && (newestCompleted == nil || !ex.finished.Before(newestFinish)) {
			newestCompleted = ex
			newestFinish = *ex.finished
		}
		ex.mu.Unlock()
	}

	for _, ex := range sb.executions {
		if ex != newestCompleted && sb.running[ex.id] == nil && ex.completed() {
			return ex
		}
	}
	return nil
}

func (sb *sandbox) dropOldestLocked() (int, bool) {
	ex := sb.oldestDroppable()
	if ex == nil {
		return 0, false
	}
	if ex.forget != nil && ex.forget() != nil {
		return 0, false
	}
	if ex.key != "" && sb.idempotency[ex.key] == ex {
		delete(sb.idempotency, ex.key)
	}
	freed := ex.retained()
	for i, candidate := range sb.executions {
		if candidate == ex {
			copy(sb.executions[i:], sb.executions[i+1:])
			sb.executions[len(sb.executions)-1] = nil
			sb.executions = sb.executions[:len(sb.executions)-1]
			break
		}
	}
	return freed, true
}

func (sb *sandbox) isKilled() bool {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.state == StateKilled && len(sb.running) == 0
}
