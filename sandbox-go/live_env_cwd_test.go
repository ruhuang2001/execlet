//go:build live

// Live tests for the execution environment and working directory. They need the
// real Docker backend, because the behaviour under test is the wrapper shell
// and Docker's exec API, neither of which a fake reproduces:
//
//	make build && ./bin/sandboxd -addr 127.0.0.1:18080 &
//	SANDBOX_DOMAIN=http://127.0.0.1:18080 go test -tags live ./sandbox-go/ -run TestLiveExec -v
package sandbox_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	sandbox "github.com/ruhuang2001/tiny-sandbox/sandbox-go"
)

// liveExec runs command to completion under a request, through the two-step
// API the contract documents for env and cwd.
func liveExec(t *testing.T, c *sandbox.Client, sandboxID string, req sandbox.ExecuteRequest) (*sandbox.Execution, error) {
	t.Helper()
	ctx := context.Background()
	started, err := c.StartExecution(ctx, sandboxID, req)
	if err != nil {
		return nil, err
	}
	return c.StreamExecution(ctx, sandboxID, started.ID, nil)
}

// TestLiveExecEnvReachesTheProcess proves the environment is applied rather
// than only accepted by the API.
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

	// A later execution must not inherit it: the environment belongs to one
	// execution, not to the container.
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

// TestLiveExecCwdIsApplied proves the directory is used, and echoed back.
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

// TestLiveExecBadCwdStaysCancelable is the reason the wrapper chdirs instead of
// using Docker's exec WorkingDir.
//
// With WorkingDir, the runtime fails before the wrapper writes its PID file,
// so a cancel has no process to signal and removes the whole container. This
// asserts the opposite: a bad directory is one failed execution with exit 126,
// and the sandbox keeps working.
func TestLiveExecBadCwdStaysCancelable(t *testing.T) {
	c := liveClient(t)
	sb := liveSandbox(t, c, "alpine:3.20", 64)

	ex, err := liveExec(t, c, sb.ID, sandbox.ExecuteRequest{
		Command: []string{"true"},
		Cwd:     "/does-not-exist",
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	// A non-zero exit is a completed execution, not a stream failure: the SDK
	// hands the outcome back and leaves the judgement to the caller.
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
	// The step that turns an outcome into an error, so the sentinel the
	// contract promises for a non-zero exit is exercised too.
	if _, err := c.Exec(context.Background(), sb.ID, []string{"sh", "-c", "exit 3"}); !errors.Is(err, sandbox.ErrNonZeroExit) {
		t.Errorf("Exec error = %v, want ErrNonZeroExit", err)
	}

	// The sandbox must survive: a typo should cost one execution, not the
	// container.
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

// TestLiveExecCancelWorksWithBadCwd is the sharpest form of the regression: a
// long-running command under an unusable directory must still be cancelable.
//
// With Docker's WorkingDir the process never starts, so there is nothing to
// signal and the cancel path removes the container. With the wrapper the
// process starts, fails the chdir, and exits 126 immediately, so this asserts
// the execution is terminal without the container being destroyed.
func TestLiveExecCancelWorksWithBadCwd(t *testing.T) {
	c := liveClient(t)
	sb := liveSandbox(t, c, "alpine:3.20", 64)

	started, err := c.StartExecution(context.Background(), sb.ID, sandbox.ExecuteRequest{
		Command: []string{"sleep", "30"},
		Cwd:     "/does-not-exist",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	canceled, err := c.CancelExecution(context.Background(), sb.ID, started.ID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if canceled.State != "completed" {
		t.Fatalf("state = %s, want completed after cancel", canceled.State)
	}

	after, err := c.GetSandbox(context.Background(), sb.ID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if after.State != "running" {
		t.Fatalf("sandbox state = %s; a cancel must leave the sandbox running", after.State)
	}
}
