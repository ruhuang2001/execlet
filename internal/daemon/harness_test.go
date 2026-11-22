package daemon_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
	"github.com/ruhuang2001/tiny-sandbox/internal/daemon/membackend"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func noSleep(context.Context, time.Duration) bool { return true }

type harness struct {
	t     *testing.T
	clock *fakeClock
	be    *membackend.Backend
	s     *daemon.Sandboxes
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	clock := newFakeClock()
	be := membackend.New()
	s := daemon.New(be,
		daemon.WithClock(clock),
		daemon.WithClassifierSleep(noSleep),
		daemon.WithCancelGrace(10*time.Millisecond),
		daemon.WithCancelDeadline(100*time.Millisecond),
	)
	return &harness{t: t, clock: clock, be: be, s: s}
}

func (h *harness) create(req daemon.CreateRequest) daemon.Sandbox {
	h.t.Helper()
	sb, err := h.s.Create(context.Background(), req)
	if err != nil {
		h.t.Fatalf("create: %v", err)
	}
	return sb
}

func (h *harness) containerOf(id string) string {
	h.t.Helper()
	container := h.s.ContainerIDForTest(id)
	if container == "" {
		h.t.Fatalf("sandbox %s has no container", id)
	}
	return container
}

func wantCode(t *testing.T, err error, want daemon.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("want error with code %s, got nil", want)
	}
	if got := daemon.CodeOf(err); got != want {
		t.Fatalf("code = %s (%v), want %s", got, err, want)
	}
}

func wantNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func ptr[T any](v T) *T { return &v }

func (h *harness) waitCompleted(sandboxID, executionID string) daemon.Execution {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ex, err := h.s.GetExecution(sandboxID, executionID)
		if err != nil {
			h.t.Fatalf("get execution: %v", err)
		}
		if ex.State == daemon.ExecCompleted {
			return ex
		}
		time.Sleep(time.Millisecond)
	}
	h.t.Fatal("execution did not complete")
	return daemon.Execution{}
}

func (h *harness) spinCompleted(sandboxID, executionID string) daemon.Execution {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ex, err := h.s.GetExecution(sandboxID, executionID)
		if err != nil {
			h.t.Fatalf("get execution: %v", err)
		}
		if ex.State == daemon.ExecCompleted {
			return ex
		}
	}
	h.t.Fatal("execution did not complete")
	return daemon.Execution{}
}

func memEcho(_ context.Context, args []string) (string, string, int) {
	out := ""
	for _, a := range args {
		out += a + "\n"
	}
	return out, "", 0
}

func memBlock(ctx context.Context, _ []string) (string, string, int) {
	t := time.NewTimer(50 * time.Millisecond)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return "", "", 137
	case <-t.C:
		return "", "", 0
	}
}

func memExit(_ context.Context, args []string) (string, string, int) {
	code := 0
	for _, a := range args {
		for _, ch := range a {
			code = code*10 + int(ch-'0')
		}
	}
	return "", "", code
}

func memLongBlock(ctx context.Context, _ []string) (string, string, int) {
	<-ctx.Done()
	return "", "", 137
}

type flaky struct {
	*membackend.Backend
	pauseErr       error
	livenessErr    error
	pauseThenErr   error
	removeFailures int

	mu       sync.Mutex
	removals int
}

func (f *flaky) Pause(ctx context.Context, id string) error {
	if f.pauseThenErr != nil {
		if err := f.Backend.Pause(ctx, id); err != nil {
			return err
		}
		return f.pauseThenErr
	}
	if f.pauseErr != nil {
		return f.pauseErr
	}
	return f.Backend.Pause(ctx, id)
}

func (f *flaky) Liveness(ctx context.Context, id string) (daemon.Liveness, error) {
	if f.livenessErr != nil {
		return daemon.LivenessRunning, f.livenessErr
	}
	return f.Backend.Liveness(ctx, id)
}

func (f *flaky) Remove(ctx context.Context, id string) error {
	f.mu.Lock()
	f.removals++
	failing := f.removals <= f.removeFailures
	f.mu.Unlock()
	if failing {
		return errBoom
	}
	return f.Backend.Remove(ctx, id)
}
