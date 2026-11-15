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

// These scenarios exercise the installed daemon and real Docker, not a fake.
func TestLiveStdin(t *testing.T) {
	c := liveClient(t)
	sb := liveSandbox(t, c, "alpine:3.20", 64)
	input := "hello 世界\n\x00'$HOME;$(touch /tmp/input-injected);`printf X`\n"
	assertClean := func(label string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		check, err := c.Exec(ctx, sb.ID, []string{"sh", "-c", "test ! -e /tmp/input-injected && find /tmp -name 'tiny-sandbox-stdin.*' -print"})
		if err != nil || check.Stdout != "" {
			t.Fatalf("%s left staged input or failed cleanup check", label)
		}
	}
	req := sandbox.ExecuteRequest{Command: []string{"cat"}, Stdin: input, Cwd: "/tmp", Env: map[string]string{"A": "B"}, IdempotencyKey: "stdin-replay"}
	ex, err := liveExec(t, c, sb.ID, req)
	if err != nil || ex.Stdout != input {
		t.Fatal("exact stdin did not match or execution failed")
	}
	replay, err := c.StartExecution(t.Context(), sb.ID, req)
	if err != nil || replay.ID != ex.ID {
		t.Fatal("stdin replay failed or created a different execution")
	}
	assertClean("normal exit")
	req.Stdin += "different"
	_, err = c.StartExecution(t.Context(), sb.ID, req)
	conflict, ok := errors.AsType[*sandbox.Error](err)
	if !ok || conflict.Code != "idempotency_conflict" {
		t.Fatalf("changed input replay: %v", err)
	}
	for _, value := range []string{"", strings.Repeat("\x00", 1<<20)} {
		ex, err := liveExec(t, c, sb.ID, sandbox.ExecuteRequest{Command: []string{"wc", "-c"}, Stdin: value})
		if err != nil {
			t.Fatal(err)
		}
		want := "0"
		if value != "" {
			want = "1048576"
		}
		if strings.TrimSpace(ex.Stdout) != want {
			t.Fatalf("byte count = %q, want %s", ex.Stdout, want)
		}
	}
	_, err = c.StartExecution(t.Context(), sb.ID, sandbox.ExecuteRequest{Command: []string{"true"}, Stdin: strings.Repeat("x", (1<<20)+1)})
	apiErr, ok := errors.AsType[*sandbox.Error](err)
	if !ok || apiErr.Code != "payload_too_large" {
		t.Fatalf("oversized stdin: %v", err)
	}
	for _, tc := range []struct {
		name   string
		req    sandbox.ExecuteRequest
		reason sandbox.Reason
		code   int
	}{
		{"missing command", sandbox.ExecuteRequest{Command: []string{"missing-stdin-command"}, Stdin: "private"}, sandbox.ReasonExited, 127},
		{"unusable cwd", sandbox.ExecuteRequest{Command: []string{"cat"}, Cwd: "/missing-stdin-dir", Stdin: "private"}, sandbox.ReasonExited, 126},
		{"timeout", sandbox.ExecuteRequest{Command: []string{"sleep", "30"}, TimeoutSeconds: new(1), Stdin: "private"}, sandbox.ReasonTimeout, -1},
	} {
		ex, _ := liveExec(t, c, sb.ID, tc.req)
		if ex == nil || ex.TerminationReason == nil || *ex.TerminationReason != tc.reason || ex.State != "completed" {
			t.Fatalf("%s returned wrong terminal outcome", tc.name)
		}
		if tc.code >= 0 && (ex.ExitCode == nil || *ex.ExitCode != tc.code) {
			t.Fatalf("%s returned wrong exit code", tc.name)
		}
		assertClean(tc.name)
	}
	started, err := c.StartExecution(t.Context(), sb.ID, sandbox.ExecuteRequest{Command: []string{"sleep", "30"}, Stdin: "cancel-private"})
	if err != nil {
		t.Fatal(err)
	}
	cancelCtx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err := c.CancelExecution(cancelCtx, sb.ID, started.ID); err != nil {
		t.Fatal("cancel request failed")
	}
	for {
		finished, err := c.GetExecution(cancelCtx, sb.ID, started.ID)
		if err != nil {
			t.Fatal("cancel outcome lookup failed")
		}
		if finished.State == "completed" {
			if finished.TerminationReason == nil || *finished.TerminationReason != sandbox.ReasonCanceled {
				t.Fatal("cancel returned wrong reason")
			}
			break
		}
		select {
		case <-cancelCtx.Done():
			t.Fatal("cancel did not complete within bound")
		case <-time.After(20 * time.Millisecond):
		}
	}
	assertClean("canceled")
	t.Logf("stdin exact bytes=%d; max=1048576; replay=%s; per-case cleanup=0", len(input), replay.ID)
}
