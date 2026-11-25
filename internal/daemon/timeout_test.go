package daemon_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/ruhuang2001/execlet/internal/daemon"
	"github.com/ruhuang2001/execlet/internal/daemon/membackend"
)

func TestLifetimeTimeout(t *testing.T) {
	ctx := context.Background()

	t.Run("kill", func(t *testing.T) {
		h := newHarness(t)
		sb := h.create(daemon.CreateRequest{TimeoutSeconds: ptr(10)})
		h.clock.Advance(9 * time.Second)
		h.s.Reap(ctx)
		got, err := h.s.Get(sb.ID)
		wantNoError(t, err)
		if got.State != daemon.StateRunning {
			t.Fatalf("state before the deadline = %s, want running", got.State)
		}

		h.clock.Advance(time.Second)
		h.s.Reap(ctx)
		got, err = h.s.Get(sb.ID)
		wantNoError(t, err)
		if got.State != daemon.StateKilled || got.KillReason == nil || *got.KillReason != daemon.KillTimeout {
			t.Fatalf("state = %s, kill_reason = %v; want killed/timeout", got.State, got.KillReason)
		}
		if got.ExpiresAt != nil {
			t.Fatalf("expires_at = %v, want null", got.ExpiresAt)
		}
	})

	t.Run("kill keeps counting while paused", func(t *testing.T) {
		h := newHarness(t)
		sb := h.create(daemon.CreateRequest{TimeoutSeconds: ptr(10)})
		if _, err := h.s.Pause(ctx, sb.ID); err != nil {
			t.Fatalf("pause: %v", err)
		}
		got, err := h.s.Get(sb.ID)
		wantNoError(t, err)
		if got.ExpiresAt == nil {
			t.Fatal("expires_at is null while paused under a kill action; the deadline keeps counting")
		}

		h.clock.Advance(10 * time.Second)
		h.s.Reap(ctx)
		got, err = h.s.Get(sb.ID)
		wantNoError(t, err)
		if got.State != daemon.StateKilled || *got.KillReason != daemon.KillTimeout {
			t.Fatalf("state = %s, kill_reason = %v; want killed/timeout from paused", got.State, got.KillReason)
		}
	})

	t.Run("pause does not count while paused", func(t *testing.T) {
		h := newHarness(t)
		sb := h.create(daemon.CreateRequest{
			TimeoutSeconds: ptr(10),
			TimeoutAction:  ptr(daemon.ActionPause),
		})
		if _, err := h.s.Pause(ctx, sb.ID); err != nil {
			t.Fatalf("pause: %v", err)
		}
		got, err := h.s.Get(sb.ID)
		wantNoError(t, err)
		if got.ExpiresAt != nil {
			t.Fatalf("expires_at = %v while paused under a pause action, want null", got.ExpiresAt)
		}

		h.clock.Advance(time.Hour)
		h.s.Reap(ctx)
		got, err = h.s.Get(sb.ID)
		wantNoError(t, err)
		if got.State != daemon.StatePaused {
			t.Fatalf("state = %s, want paused", got.State)
		}
	})

	t.Run("pause fires at the deadline", func(t *testing.T) {
		h := newHarness(t)
		sb := h.create(daemon.CreateRequest{
			TimeoutSeconds: ptr(10),
			TimeoutAction:  ptr(daemon.ActionPause),
		})
		h.clock.Advance(10 * time.Second)
		h.s.Reap(ctx)
		got, err := h.s.Get(sb.ID)
		wantNoError(t, err)
		if got.State != daemon.StatePaused {
			t.Fatalf("state = %s, want paused", got.State)
		}
		if got.ExpiresAt != nil {
			t.Fatalf("expires_at = %v, want null after a timeout pause", got.ExpiresAt)
		}
	})

	t.Run("resume after a timeout pause starts a fresh interval", func(t *testing.T) {
		h := newHarness(t)
		sb := h.create(daemon.CreateRequest{
			TimeoutSeconds: ptr(10),
			TimeoutAction:  ptr(daemon.ActionPause),
		})
		h.clock.Advance(10 * time.Second)
		h.s.Reap(ctx)

		h.clock.Advance(5 * time.Second)
		got, err := h.s.Resume(ctx, sb.ID)
		wantNoError(t, err)
		if got.State != daemon.StateRunning {
			t.Fatalf("state = %s, want running", got.State)
		}
		wantDeadline := h.clock.Now().Add(10 * time.Second)
		if got.ExpiresAt == nil || !got.ExpiresAt.Equal(wantDeadline) {
			t.Fatalf("expires_at = %v, want %v (now + timeout)", got.ExpiresAt, wantDeadline)
		}

		if _, err := h.s.Pause(ctx, sb.ID); err != nil {
			t.Fatalf("pause: %v", err)
		}
		h.clock.Advance(time.Hour)
		ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"true"}})
		wantNoError(t, err)
		after, err := h.s.Get(sb.ID)
		wantNoError(t, err)
		if after.ExpiresAt == nil || !after.ExpiresAt.After(h.clock.Now()) {
			t.Fatalf("expires_at = %v after an automatic resume, want a future deadline", after.ExpiresAt)
		}
		h.waitCompleted(sb.ID, ex.ID)
	})

	t.Run("zero disables the deadline", func(t *testing.T) {
		h := newHarness(t)
		sb := h.create(daemon.CreateRequest{TimeoutSeconds: ptr(0)})
		h.clock.Advance(1000 * time.Hour)
		h.s.Reap(ctx)
		got, err := h.s.Get(sb.ID)
		wantNoError(t, err)
		if got.State != daemon.StateRunning {
			t.Fatalf("state = %s, want running with the timeout disabled", got.State)
		}
	})

	t.Run("a running execution times out with the sandbox", func(t *testing.T) {
		h := newHarness(t)
		h.be.Programs["block"] = memBlock
		sb := h.create(daemon.CreateRequest{TimeoutSeconds: ptr(10)})
		ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"block"}})
		wantNoError(t, err)

		h.clock.Advance(10 * time.Second)
		h.s.Reap(ctx)

		done := h.waitCompleted(sb.ID, ex.ID)
		if *done.TerminationReason != daemon.ReasonTimeout {
			t.Fatalf("reason = %s, want timeout", *done.TerminationReason)
		}
		sandbox, err := h.s.Get(sb.ID)
		wantNoError(t, err)
		if sandbox.State != daemon.StateKilled || *sandbox.KillReason != daemon.KillTimeout {
			t.Fatalf("sandbox = %+v, want killed/timeout", sandbox)
		}
	})

	t.Run("reads never change state", func(t *testing.T) {
		h := newHarness(t)
		sb := h.create(daemon.CreateRequest{TimeoutSeconds: ptr(10)})
		h.clock.Advance(time.Hour)
		for range 5 {
			got, err := h.s.Get(sb.ID)
			wantNoError(t, err)
			if got.State != daemon.StateRunning {
				t.Fatalf("a read changed the state to %s", got.State)
			}
		}
	})
}

func TestExecutionTimeout(t *testing.T) {
	ctx := context.Background()

	t.Run("kills the execution and leaves the sandbox running", func(t *testing.T) {
		h := newHarness(t)
		h.be.Programs["block"] = memLongBlock
		sb := h.create(daemon.CreateRequest{TimeoutSeconds: ptr(0)})
		ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{
			Command:        []string{"block"},
			TimeoutSeconds: ptr(5),
		})
		wantNoError(t, err)

		h.clock.Advance(4 * time.Second)
		h.s.Reap(ctx)
		if got, err := h.s.GetExecution(sb.ID, ex.ID); err != nil || got.State != daemon.ExecRunning {
			t.Fatalf("execution before the deadline = %+v, want running", got)
		}

		h.clock.Advance(time.Second)
		h.s.Reap(ctx)
		done := h.waitCompleted(sb.ID, ex.ID)
		if done.TerminationReason == nil || *done.TerminationReason != daemon.ReasonTimeout {
			t.Fatalf("reason = %v, want timeout", done.TerminationReason)
		}

		sandbox, err := h.s.Get(sb.ID)
		wantNoError(t, err)
		if sandbox.State != daemon.StateRunning {
			t.Fatalf("sandbox state = %s, want running after an execution timeout", sandbox.State)
		}
		if sandbox.KillReason != nil {
			t.Fatalf("kill_reason = %v, want null", *sandbox.KillReason)
		}
	})

	t.Run("no deadline, no timeout", func(t *testing.T) {
		h := newHarness(t)
		h.be.Programs["block"] = memBlock
		sb := h.create(daemon.CreateRequest{TimeoutSeconds: ptr(0)})
		ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"block"}})
		wantNoError(t, err)

		h.clock.Advance(time.Hour)
		h.s.Reap(ctx)
		if got, err := h.s.GetExecution(sb.ID, ex.ID); err != nil || got.State != daemon.ExecRunning {
			t.Fatalf("execution = %+v, want still running: an unset deadline must not fire", got)
		}
	})

	t.Run("zero is the same as unset", func(t *testing.T) {
		h := newHarness(t)
		h.be.Programs["block"] = memBlock
		sb := h.create(daemon.CreateRequest{TimeoutSeconds: ptr(0)})
		ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{
			Command:        []string{"block"},
			TimeoutSeconds: ptr(0),
		})
		wantNoError(t, err)
		h.clock.Advance(time.Hour)
		h.s.Reap(ctx)
		if got, err := h.s.GetExecution(sb.ID, ex.ID); err != nil || got.State != daemon.ExecRunning {
			t.Fatalf("execution = %+v, want still running", got)
		}
	})

	t.Run("negative is invalid", func(t *testing.T) {
		h := newHarness(t)
		sb := h.create(daemon.CreateRequest{})
		_, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{
			Command:        []string{"true"},
			TimeoutSeconds: ptr(-1),
		})
		wantCode(t, err, daemon.CodeInvalidRequest)
	})

	t.Run("a paused sandbox does not extend the deadline", func(t *testing.T) {
		h := newHarness(t)
		h.be.Programs["block"] = memLongBlock
		sb := h.create(daemon.CreateRequest{TimeoutSeconds: ptr(0)})
		ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{
			Command:        []string{"block"},
			TimeoutSeconds: ptr(10),
		})
		wantNoError(t, err)
		if _, err := h.s.Pause(ctx, sb.ID); err != nil {
			t.Fatalf("pause: %v", err)
		}

		h.clock.Advance(time.Hour)
		h.s.Reap(ctx)
		if got, err := h.s.GetExecution(sb.ID, ex.ID); err != nil || got.State != daemon.ExecRunning {
			t.Fatalf("execution while paused = %+v, want running", got)
		}

		if _, err := h.s.Resume(ctx, sb.ID); err != nil {
			t.Fatalf("resume: %v", err)
		}
		h.s.Reap(ctx)
		done := h.waitCompleted(sb.ID, ex.ID)
		if done.TerminationReason == nil || *done.TerminationReason != daemon.ReasonTimeout {
			t.Fatalf("reason = %v, want timeout after resume", done.TerminationReason)
		}
	})

	t.Run("the earlier deadline wins", func(t *testing.T) {
		h := newHarness(t)
		h.be.Programs["block"] = memLongBlock
		sb := h.create(daemon.CreateRequest{TimeoutSeconds: ptr(5)})
		ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{
			Command:        []string{"block"},
			TimeoutSeconds: ptr(600),
		})
		wantNoError(t, err)

		h.clock.Advance(5 * time.Second)
		h.s.Reap(ctx)
		done := h.waitCompleted(sb.ID, ex.ID)
		if done.TerminationReason == nil || *done.TerminationReason != daemon.ReasonTimeout {
			t.Fatalf("reason = %v, want timeout", done.TerminationReason)
		}
		sandbox, err := h.s.Get(sb.ID)
		wantNoError(t, err)
		if sandbox.State != daemon.StateKilled {
			t.Fatalf("sandbox state = %s, want killed: the lifetime deadline came first", sandbox.State)
		}
	})
}

func TestIdleTimeout(t *testing.T) {
	ctx := context.Background()

	t.Run("kill", func(t *testing.T) {
		h := newHarness(t)
		sb := h.create(daemon.CreateRequest{IdleTimeoutSeconds: ptr(10)})
		h.clock.Advance(9 * time.Second)
		h.s.Reap(ctx)
		got, err := h.s.Get(sb.ID)
		wantNoError(t, err)
		if got.State != daemon.StateRunning {
			t.Fatalf("state = %s before the idle deadline, want running", got.State)
		}

		h.clock.Advance(time.Second)
		h.s.Reap(ctx)
		got, err = h.s.Get(sb.ID)
		wantNoError(t, err)
		if got.State != daemon.StateKilled || *got.KillReason != daemon.KillIdleTimeout {
			t.Fatalf("state = %s, kill_reason = %v; want killed/idle_timeout", got.State, got.KillReason)
		}
	})

	t.Run("pause", func(t *testing.T) {
		h := newHarness(t)
		sb := h.create(daemon.CreateRequest{
			IdleTimeoutSeconds: ptr(10),
			IdleTimeoutAction:  ptr(daemon.ActionPause),
		})
		h.clock.Advance(10 * time.Second)
		h.s.Reap(ctx)
		got, err := h.s.Get(sb.ID)
		wantNoError(t, err)
		if got.State != daemon.StatePaused {
			t.Fatalf("state = %s, want paused", got.State)
		}
	})

	t.Run("requests reset the idle clock", func(t *testing.T) {
		h := newHarness(t)
		sb := h.create(daemon.CreateRequest{IdleTimeoutSeconds: ptr(10)})
		h.clock.Advance(9 * time.Second)
		h.s.Touch(sb.ID)
		h.clock.Advance(9 * time.Second)
		h.s.Reap(ctx)
		got, err := h.s.Get(sb.ID)
		wantNoError(t, err)
		if got.State != daemon.StateRunning {
			t.Fatalf("state = %s after activity, want running", got.State)
		}
	})

	t.Run("an open stream is activity", func(t *testing.T) {
		h := newHarness(t)
		sb := h.create(daemon.CreateRequest{IdleTimeoutSeconds: ptr(10), TimeoutSeconds: ptr(0)})
		closeStream, err := h.s.OpenStream(sb.ID)
		wantNoError(t, err)
		h.clock.Advance(time.Hour)
		h.s.Reap(ctx)
		got, err := h.s.Get(sb.ID)
		wantNoError(t, err)
		if got.State != daemon.StateRunning {
			t.Fatalf("state = %s with an open stream, want running", got.State)
		}

		closeStream()
		h.clock.Advance(10 * time.Second)
		h.s.Reap(ctx)
		got, err = h.s.Get(sb.ID)
		wantNoError(t, err)
		if got.State != daemon.StateKilled {
			t.Fatalf("state = %s after the stream closed and idled, want killed", got.State)
		}
	})

	t.Run("a running execution is not idle", func(t *testing.T) {
		h := newHarness(t)
		h.be.Programs["block"] = memLongBlock
		sb := h.create(daemon.CreateRequest{IdleTimeoutSeconds: ptr(10), TimeoutSeconds: ptr(0)})
		ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"block"}})
		wantNoError(t, err)

		h.clock.Advance(time.Hour)
		h.s.Reap(ctx)
		got, err := h.s.Get(sb.ID)
		wantNoError(t, err)
		if got.State != daemon.StateRunning {
			t.Fatalf("state = %s while an execution runs, want running", got.State)
		}
		if _, err := h.s.CancelExecution(ctx, sb.ID, ex.ID); err != nil {
			t.Fatalf("cancel: %v", err)
		}
	})

	t.Run("idle time runs from the end of the last execution", func(t *testing.T) {
		h := newHarness(t)
		h.be.Programs["exit"] = memExit
		sb := h.create(daemon.CreateRequest{IdleTimeoutSeconds: ptr(10)})
		ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"exit", "0"}})
		wantNoError(t, err)
		h.waitCompleted(sb.ID, ex.ID)

		h.clock.Advance(9 * time.Second)
		h.s.Reap(ctx)
		got, err := h.s.Get(sb.ID)
		wantNoError(t, err)
		if got.State != daemon.StateRunning {
			t.Fatalf("state = %s, want running", got.State)
		}
		h.clock.Advance(2 * time.Second)
		h.s.Reap(ctx)
		got, err = h.s.Get(sb.ID)
		wantNoError(t, err)
		if got.State != daemon.StateKilled {
			t.Fatalf("state = %s, want killed", got.State)
		}
	})

	t.Run("disabled by default", func(t *testing.T) {
		h := newHarness(t)
		sb := h.create(daemon.CreateRequest{TimeoutSeconds: ptr(0)})
		h.clock.Advance(1000 * time.Hour)
		h.s.Reap(ctx)
		got, err := h.s.Get(sb.ID)
		wantNoError(t, err)
		if got.State != daemon.StateRunning {
			t.Fatalf("state = %s, want running with idle_timeout disabled", got.State)
		}
	})
}

func TestEvictSweep(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	sb := h.create(daemon.CreateRequest{})
	h.be.Kill(h.containerOf(sb.ID))

	h.s.EvictSweep(ctx)

	got, err := h.s.Get(sb.ID)
	wantNoError(t, err)
	if got.State != daemon.StateKilled || got.KillReason == nil || *got.KillReason != daemon.KillEvicted {
		t.Fatalf("sandbox = %+v, want killed/evicted", got)
	}
}

func TestReaperRun(t *testing.T) {
	h := newHarness(t)
	sb := h.create(daemon.CreateRequest{TimeoutSeconds: ptr(1)})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.s.Run(ctx)

	h.clock.Advance(time.Second)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got, err := h.s.Get(sb.ID)
		wantNoError(t, err)
		if got.State == daemon.StateKilled {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the reaper did not apply the lifetime timeout")
}

type deadlineCheckingBackend struct {
	*membackend.Backend
}

func (b *deadlineCheckingBackend) Owned(context.Context, string) ([]string, error) {
	return b.IDs(), nil
}

func (b *deadlineCheckingBackend) Pause(ctx context.Context, id string) error {
	if _, ok := ctx.Deadline(); !ok {
		return daemon.ErrStartUnconfirmed
	}
	return b.Backend.Pause(ctx, id)
}

func (b *deadlineCheckingBackend) Unpause(ctx context.Context, id string) error {
	if _, ok := ctx.Deadline(); !ok {
		return daemon.ErrStartUnconfirmed
	}
	return b.Backend.Unpause(ctx, id)
}

func TestPauseAndUnpauseApplyBackendCallTimeout(t *testing.T) {
	be := &deadlineCheckingBackend{Backend: membackend.New()}
	be.Programs["block"] = memLongBlock
	s := daemon.New(be, daemon.WithClassifierSleep(noSleep), daemon.WithCancelGrace(10*time.Millisecond), daemon.WithCancelDeadline(100*time.Millisecond))
	ctx := context.Background()

	sb, err := s.Create(ctx, daemon.CreateRequest{})
	wantNoError(t, err)
	if _, err := s.Pause(ctx, sb.ID); err != nil {
		t.Fatalf("Pause without caller deadline failed: %v", err)
	}
	if _, err := s.Resume(ctx, sb.ID); err != nil {
		t.Fatalf("Resume without caller deadline failed: %v", err)
	}

	ex, _, err := s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"block"}})
	wantNoError(t, err)
	if _, err := s.Pause(ctx, sb.ID); err != nil {
		t.Fatalf("Pause before cancel failed: %v", err)
	}
	if _, err := s.CancelExecution(ctx, sb.ID, ex.ID); err != nil {
		t.Fatalf("CancelExecution on paused sandbox without caller deadline failed: %v", err)
	}
}

func TestDurableJournalSkipsIdleReapWritesAndPersistsTransitions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	j, err := daemon.OpenJournal(dir, "test-owner")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })

	clock := newFakeClock()
	be := &deadlineCheckingBackend{Backend: membackend.New()}
	s := daemon.New(be, daemon.WithOwnerLabel("test-owner"), daemon.WithClock(clock), daemon.WithClassifierSleep(noSleep))
	ctx := context.Background()
	wantNoError(t, s.Recover(ctx, j, be))

	live, err := s.Create(ctx, daemon.CreateRequest{IdleTimeoutSeconds: ptr(60)})
	wantNoError(t, err)
	noIdle, err := s.Create(ctx, daemon.CreateRequest{})
	wantNoError(t, err)
	killed, err := s.Create(ctx, daemon.CreateRequest{})
	wantNoError(t, err)
	_, err = s.Delete(ctx, killed.ID)
	wantNoError(t, err)

	inodeOf := func(id string) uint64 {
		t.Helper()
		info, statErr := os.Stat(filepath.Join(dir, id+".json"))
		if statErr != nil {
			t.Fatal(statErr)
		}
		return info.Sys().(*syscall.Stat_t).Ino
	}

	liveIno := inodeOf(live.ID)
	noIdleIno := inodeOf(noIdle.ID)
	killedIno := inodeOf(killed.ID)

	s.Reap(ctx)
	if got := inodeOf(live.ID); got != liveIno {
		t.Fatalf("unchanged live sandbox was rewritten on Reap: ino %d -> %d", liveIno, got)
	}
	if got := inodeOf(killed.ID); got != killedIno {
		t.Fatalf("killed tombstone was rewritten on Reap: ino %d -> %d", killedIno, got)
	}

	clock.Advance(5 * time.Second)
	s.Touch(live.ID)
	s.Touch(noIdle.ID)
	s.Reap(ctx)
	if got := inodeOf(noIdle.ID); got != noIdleIno {
		t.Fatal("activity without an idle deadline rewrote the manifest")
	}
	if got := inodeOf(live.ID); got == liveIno {
		t.Fatal("dirty live sandbox activity was not checkpointed on Reap")
	}

	_, err = s.Pause(ctx, live.ID)
	wantNoError(t, err)
	be.Disappear(s.ContainerIDForTest(live.ID))
	_, err = s.Resume(ctx, live.ID)
	wantCode(t, err, daemon.CodeInvalidState)

	_ = j.Close()
	j2, err := daemon.OpenJournal(dir, "test-owner")
	wantNoError(t, err)
	defer func() { _ = j2.Close() }()
	s2 := daemon.New(be, daemon.WithOwnerLabel("test-owner"), daemon.WithClock(clock), daemon.WithClassifierSleep(noSleep))
	wantNoError(t, s2.Recover(ctx, j2, be))
	recovered, err := s2.Get(live.ID)
	wantNoError(t, err)
	if recovered.State != daemon.StateKilled || recovered.KillReason == nil || *recovered.KillReason != daemon.KillEvicted {
		t.Fatalf("recovered sandbox = %+v, want killed/evicted persisted before restart", recovered)
	}
}

type batchSweepBackend struct {
	*membackend.Backend
	batchCalls  atomic.Int32
	singleCalls atomic.Int32
	failBatch   bool
	onBatch     func()
}

func (b *batchSweepBackend) Liveness(ctx context.Context, id string) (daemon.Liveness, error) {
	b.singleCalls.Add(1)
	return b.Backend.Liveness(ctx, id)
}

func (b *batchSweepBackend) OwnedLiveness(ctx context.Context, _ string) (map[string]daemon.Liveness, error) {
	b.batchCalls.Add(1)
	if b.failBatch {
		return nil, errors.New("runtime unavailable")
	}
	states := make(map[string]daemon.Liveness)
	for _, id := range b.IDs() {
		l, err := b.Backend.Liveness(ctx, id)
		if err == nil && l != daemon.LivenessMissing {
			states[id] = l
		}
	}
	if b.onBatch != nil {
		b.onBatch()
	}
	return states, nil
}

func TestBatchEvictSweepUsesSingleListAndProtectsConcurrentCreate(t *testing.T) {
	ctx := context.Background()
	be := &batchSweepBackend{Backend: membackend.New()}
	s := daemon.New(be, daemon.WithOwnerLabel("sweep-owner"))

	var sandboxes []daemon.Sandbox
	for range 5 {
		sb, err := s.Create(ctx, daemon.CreateRequest{})
		wantNoError(t, err)
		sandboxes = append(sandboxes, sb)
	}
	be.Kill(s.ContainerIDForTest(sandboxes[0].ID))

	var concurrent daemon.Sandbox
	be.onBatch = func() {
		var err error
		concurrent, err = s.Create(ctx, daemon.CreateRequest{})
		wantNoError(t, err)
	}
	s.EvictSweep(ctx)
	be.onBatch = nil

	if got := be.batchCalls.Load(); got != 1 {
		t.Fatalf("batchCalls = %d, want 1", got)
	}
	if got := be.singleCalls.Load(); got != 0 {
		t.Fatalf("singleCalls = %d, want 0", got)
	}
	evicted, err := s.Get(sandboxes[0].ID)
	wantNoError(t, err)
	if evicted.State != daemon.StateKilled || evicted.KillReason == nil || *evicted.KillReason != daemon.KillEvicted {
		t.Fatalf("evicted sandbox = %+v, want killed/evicted", evicted)
	}
	fresh, err := s.Get(concurrent.ID)
	wantNoError(t, err)
	if fresh.State != daemon.StateRunning {
		t.Fatalf("concurrently created sandbox was falsely evicted: %+v", fresh)
	}

	be.failBatch = true
	s.EvictSweep(ctx)
	surviving, err := s.Get(sandboxes[1].ID)
	wantNoError(t, err)
	if surviving.State != daemon.StateRunning {
		t.Fatalf("batch error falsely evicted sandbox: %+v", surviving)
	}
}
