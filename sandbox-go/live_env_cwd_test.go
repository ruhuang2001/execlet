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
	"time"

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

// TestLiveExecBadCwdCostsOneExecution pins the failure model a bad directory
// must have: exit 126 with our own marker on stderr, nothing on stdout, and a
// sandbox that still works afterwards.
//
// 126 is the wrapper's code, which is the point of chdirring in the wrapper
// instead of using Docker's exec WorkingDir: that runtime failure reports 127
// on stdout, indistinguishable from a missing command. Cancellation is not
// what this asserts, because the execution is already terminal; the test below
// covers cancel after such a failure.
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

// TestLiveExecCancelStillWorksWithABadCwd proves a bad cwd does not break the
// cancel path for a following execution.
//
// A bad cwd fails on its own, so canceling it proves nothing: the execution is
// already terminal and CancelExecution returns the record unchanged. What the
// wrapper's PID-first order protects is the next execution, so this cancels a
// long-running command with a valid directory and asserts the cancel actually
// ended the process group. TestLiveCancel covers the same path without a cwd;
// this one runs it right after the failure, in the same sandbox.
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

	// A cwd that exists, so the command really runs and cancel has a process
	// to signal. Requiring output first rules out cancelling a terminal
	// record: without it, the assertion below would hold even if nothing ran.
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

// waitForRunning blocks until the execution reports running, so a cancel
// cannot pass by returning a record that never started.
func waitForRunning(t *testing.T, c *sandbox.Client, sandboxID, executionID string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		ex, err := c.GetExecution(context.Background(), sandboxID, executionID)
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
