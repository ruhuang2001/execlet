package daemon

import (
	"context"
	"time"
)

func (s *Sandboxes) ContainerIDForTest(sandboxID string) string {
	sb, err := s.lookup(sandboxID)
	if err != nil {
		return ""
	}
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.container
}

func (s *Sandboxes) RetryRemovalsForTest(ctx context.Context) {
	s.retryRemovals(ctx)
	s.waitRemovalPassForTest()
}

func (s *Sandboxes) waitRemovalPassForTest() {
	for {
		s.retryMu.Lock()
		running := s.removalPassRunning
		s.retryMu.Unlock()
		if !running {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

func (s *Sandboxes) ScheduleRemovalCountForTest() int {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	return len(s.retryIDs)
}

func (s *Sandboxes) RetentionForTest() {
	s.prune()
}

func (s *Sandboxes) RetainedBytesForTest() int {
	total := 0
	for _, sb := range s.sandboxesInCreationOrder() {
		if sb.mu.TryLock() {
			total += sb.retainedBytesLocked()
			sb.mu.Unlock()
		}
	}
	return total
}
