//go:build live

package sandbox_test

import (
	"context"
	"errors"
	"testing"
	"time"

	sandbox "github.com/ruhuang2001/tiny-sandbox/sandbox-go"
)

func TestLiveConcurrency(t *testing.T) {
	c := liveClient(t)
	sb, err := c.CreateSandbox(t.Context(), sandbox.CreateSandboxRequest{MaxConcurrentExecutions: new(2)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = c.KillSandbox(ctx, sb.ID)
	})
	if sb.MaxConcurrentExecutions != 2 {
		t.Fatalf("limit = %d", sb.MaxConcurrentExecutions)
	}
	a, err := c.StartExecution(t.Context(), sb.ID, sandbox.ExecuteRequest{Command: []string{"sleep", "30"}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.StartExecution(t.Context(), sb.ID, sandbox.ExecuteRequest{Command: []string{"sleep", "30"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.StartExecution(t.Context(), sb.ID, sandbox.ExecuteRequest{Command: []string{"true"}}); !errors.Is(err, sandbox.ErrBusy) {
		t.Fatalf("at capacity: %v", err)
	}
	if _, err := c.CancelExecution(t.Context(), sb.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	sibling, err := c.GetExecution(t.Context(), sb.ID, b.ID)
	if err != nil || sibling.State != "running" {
		t.Fatalf("sibling %+v: %v", sibling, err)
	}
	unknown, err := c.Exec(t.Context(), sb.ID, []string{"sh", "-c", "exit 137"})
	if !errors.Is(err, sandbox.ErrUnknown) || unknown == nil || unknown.ExitCode == nil || *unknown.ExitCode != 137 {
		t.Fatalf("unknown %+v: %v", unknown, err)
	}
	if _, err := c.CancelExecution(t.Context(), sb.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	t.Logf("overlapped %s/%s, canceled independently, unknown137=%s", a.ID, b.ID, unknown.ID)
}
