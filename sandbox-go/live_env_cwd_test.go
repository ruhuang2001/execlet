//go:build live

package sandbox_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	sandbox "github.com/ruhuang2001/tiny-sandbox/sandbox-go"
)

func liveExec(t *testing.T, c *sandbox.Client, sandboxID string, req sandbox.ExecuteRequest) (*sandbox.Execution, error) {
	t.Helper()
	ctx := context.Background()
	started, err := c.StartExecution(ctx, sandboxID, req)
	if err != nil {
		return nil, err
	}
	return c.StreamExecution(ctx, sandboxID, started.ID, nil)
}

func TestLiveExecEnvReachesTheProcess(t *testing.T) {
	c := liveClient(t)
	sb := liveSandbox(t, c, "alpine:3.20", 64)

	ex, err := liveExec(t, c, sb.ID, sandbox.ExecuteRequest{
		Command: []string{"sh", "-c", `printf '%s' "$GREETING"`},
		Env:     map[string]string{"GREETING": "hello-from-env"},
	})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if got := strings.TrimSpace(ex.Stdout); got != "hello-from-env" {
		t.Fatalf("stdout = %q, want the requested GREETING", got)
	}

	again, err := liveExec(t, c, sb.ID, sandbox.ExecuteRequest{
		Command: []string{"sh", "-c", `printf '%s' "${GREETING-unset}"`},
	})
	if err != nil {
		t.Fatalf("second exec: %v", err)
	}
	if got := strings.TrimSpace(again.Stdout); got != "unset" {
		t.Fatalf("second execution saw %q; the environment leaked past its execution", got)
	}
}

func TestLiveExecCwdIsApplied(t *testing.T) {
	c := liveClient(t)
	sb := liveSandbox(t, c, "alpine:3.20", 64)

	if err := c.WriteFile(context.Background(), sb.ID, "/work/marker", []byte("x")); err != nil {
		t.Fatalf("write file: %v", err)
	}

	ex, err := liveExec(t, c, sb.ID, sandbox.ExecuteRequest{
		Command: []string{"pwd"},
		Cwd:     "/work",
	})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if got := strings.TrimSpace(ex.Stdout); got != "/work" {
		t.Fatalf("pwd = %q, want /work", got)
	}
	if ex.Cwd == nil || *ex.Cwd != "/work" {
		t.Fatalf("cwd field = %v, want /work echoed back", ex.Cwd)
	}
}

func TestLiveExecBadCwdCostsOneExecution(t *testing.T) {
	c := liveClient(t)
	sb := liveSandbox(t, c, "alpine:3.20", 64)

	ex, err := liveExec(t, c, sb.ID, sandbox.ExecuteRequest{
		Command: []string{"true"},
		Cwd:     "/does-not-exist",
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if ex.State != "completed" {
		t.Fatalf("state = %s, want completed", ex.State)
	}
	if ex.ExitCode == nil || *ex.ExitCode != 126 {
		t.Fatalf("exit code = %v, want 126", ex.ExitCode)
	}
	if ex.TerminationReason == nil || *ex.TerminationReason != sandbox.ReasonExited {
		t.Fatalf("reason = %v, want exited", ex.TerminationReason)
	}
	if !strings.Contains(ex.Stderr, "cwd is not a usable directory") {
		t.Fatalf("stderr = %q, want the cwd marker", ex.Stderr)
	}
	if _, err := c.Exec(context.Background(), sb.ID, []string{"sh", "-c", "exit 3"}); !errors.Is(err, sandbox.ErrNonZeroExit) {
		t.Errorf("Exec error = %v, want ErrNonZeroExit", err)
	}

	after, err := c.GetSandbox(context.Background(), sb.ID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if after.State != "running" {
		t.Fatalf("sandbox state = %s, want running", after.State)
	}
	if _, err := liveExec(t, c, sb.ID, sandbox.ExecuteRequest{Command: []string{"true"}}); err != nil {
		t.Fatalf("sandbox is unusable after a bad cwd: %v", err)
	}
}

func TestLiveExecCancelStillWorksWithABadCwd(t *testing.T) {
	c := liveClient(t)
	sb := liveSandbox(t, c, "alpine:3.20", 64)

	bad, err := liveExec(t, c, sb.ID, sandbox.ExecuteRequest{
		Command: []string{"true"},
		Cwd:     "/does-not-exist",
	})
	if err != nil {
		t.Fatalf("bad cwd execution: %v", err)
	}
	if bad.ExitCode == nil || *bad.ExitCode != 126 {
		t.Fatalf("bad cwd exit = %v, want 126", bad.ExitCode)
	}

	started, err := c.StartExecution(context.Background(), sb.ID, sandbox.ExecuteRequest{
		Command: []string{"sh", "-c", "sleep 30 & wait"},
		Cwd:     "/tmp",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForRunning(t, c, sb.ID, started.ID)

	canceled, err := c.CancelExecution(context.Background(), sb.ID, started.ID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if canceled.State != "completed" {
		t.Fatalf("state = %s, want completed after cancel", canceled.State)
	}
	if canceled.TerminationReason == nil || *canceled.TerminationReason != sandbox.ReasonCanceled {
		t.Fatalf("reason = %v, want canceled: the cancel did not end the process",
			canceled.TerminationReason)
	}

	after, err := c.GetSandbox(context.Background(), sb.ID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if after.State != "running" {
		t.Fatalf("sandbox state = %s; a cancel must leave the sandbox running", after.State)
	}
	if _, err := liveExec(t, c, sb.ID, sandbox.ExecuteRequest{Command: []string{"true"}}); err != nil {
		t.Fatalf("sandbox is unusable after a cancel: %v", err)
	}
}

func waitForRunning(t *testing.T, c *sandbox.Client, sandboxID, executionID string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		pollCtx, cancel := context.WithDeadline(context.Background(), deadline)
		ex, err := c.GetExecution(pollCtx, sandboxID, executionID)
		cancel()
		if err != nil {
			t.Fatalf("get execution: %v", err)
		}
		if ex.State == "running" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("execution never reported running")
}
