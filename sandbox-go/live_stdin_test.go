//go:build live

package sandbox_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	sandbox "github.com/ruhuang2001/tiny-sandbox/sandbox-go"
)

// These scenarios exercise the installed daemon and real Docker, not a fake.
func TestLiveStdin(t *testing.T) {
	c := liveClient(t)
	sb := liveSandbox(t, c, "alpine:3.20", 64)
	input := "hello 世界\n\x00'$HOME;$(touch /tmp/input-injected);`printf X`\n"
	secret, err := liveExec(t, c, sb.ID, sandbox.ExecuteRequest{Command: []string{"true"}, Stdin: "private-stdin-marker"})
	if err != nil {
		t.Fatal(err)
	}
	view, err := json.Marshal(secret)
	if err != nil || strings.Contains(string(view), "private-stdin-marker") || strings.Contains(string(view), "\"stdin\"") {
		t.Fatal("stdin leaked into execution response")
	}
	req := sandbox.ExecuteRequest{Command: []string{"cat"}, Stdin: input, Cwd: "/tmp", Env: map[string]string{"A": "B"}, IdempotencyKey: "stdin-replay"}
	ex, err := liveExec(t, c, sb.ID, req)
	if err != nil || ex.Stdout != input {
		t.Fatalf("exact stdin: execution=%+v err=%v", ex, err)
	}
	replay, err := c.StartExecution(t.Context(), sb.ID, req)
	if err != nil || replay.ID != ex.ID {
		t.Fatalf("replay: %+v %v", replay, err)
	}
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
	for _, req := range []sandbox.ExecuteRequest{
		{Command: []string{"missing-stdin-command"}, Stdin: "private"},
		{Command: []string{"cat"}, Cwd: "/missing-stdin-dir", Stdin: "private"},
		{Command: []string{"sleep", "30"}, TimeoutSeconds: new(1), Stdin: "private"},
	} {
		if ex, err := liveExec(t, c, sb.ID, req); ex == nil || ex.ExitCode == nil || *ex.ExitCode == 0 {
			t.Fatalf("expected terminal failure: %+v %v", ex, err)
		}
	}
	started, err := c.StartExecution(t.Context(), sb.ID, sandbox.ExecuteRequest{Command: []string{"sleep", "30"}, Stdin: "cancel-private"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CancelExecution(t.Context(), sb.ID, started.ID); err != nil {
		t.Fatal(err)
	}
	check, err := c.Exec(t.Context(), sb.ID, []string{"sh", "-c", "test ! -e /tmp/input-injected && find /tmp -name 'tiny-sandbox-stdin.*' -print"})
	if err != nil || check.Stdout != "" {
		t.Fatalf("staging cleanup/injection: %+v %v", check, err)
	}
	t.Logf("stdin exact bytes=%d; max=1048576; replay=%s; cleanup=0", len(input), replay.ID)
}
