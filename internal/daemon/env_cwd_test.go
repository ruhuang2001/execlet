package daemon_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
)

// TestExecEnvironmentLimits pins every bound from docs/api.md. Each case is one
// over the limit, because a bound checked as "at most N" is only proven by
// N+1: an implementation that accepted one extra entry would pass a test that
// stayed under N.
func TestExecEnvironmentLimits(t *testing.T) {
	ctx := context.Background()

	tooMany := make(map[string]string, daemon.MaxEnvEntries+1)
	for i := 0; i <= daemon.MaxEnvEntries; i++ {
		tooMany["K"+strings.Repeat("0", 3)+string(rune('a'+i%26))+strings.Repeat("x", i/26)] = "v"
	}

	tooBigTotal := make(map[string]string)
	// Each value is under the per-value limit, so only the total can reject it.
	for i := 0; len(tooBigTotal) < 6; i++ {
		tooBigTotal["T"+strings.Repeat("0", i+1)] = strings.Repeat("v", daemon.MaxEnvValueBytes-64)
	}

	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"entry count", tooMany, "entries"},
		{"empty name", map[string]string{"": "v"}, "empty variable name"},
		{"name too long", map[string]string{strings.Repeat("n", daemon.MaxEnvKeyBytes+1): "v"}, "exceeds"},
		{"name with equals", map[string]string{"A=B": "v"}, "must not contain"},
		{"name with newline", map[string]string{"A\nB": "v"}, "must not contain"},
		{"value too long", map[string]string{"V": strings.Repeat("v", daemon.MaxEnvValueBytes+1)}, "exceeds"},
		{"value with NUL", map[string]string{"V": "a\x00b"}, "must not contain a NUL byte"},
		{"total too large", tooBigTotal, "in total"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			sb := h.create(daemon.CreateRequest{})
			_, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{
				Command: []string{"true"},
				Env:     tc.env,
			})
			wantCode(t, err, daemon.CodeInvalidRequest)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestExecEnvironmentAtTheLimit proves the bounds are inclusive. Each value is
// exactly at its limit, so rejecting any of them would break a request the
// contract allows.
func TestExecEnvironmentAtTheLimit(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.be.Programs["true"] = func(context.Context, []string) (string, string, int) { return "", "", 0 }
	sb := h.create(daemon.CreateRequest{})

	env := map[string]string{
		strings.Repeat("n", daemon.MaxEnvKeyBytes): strings.Repeat("v", daemon.MaxEnvValueBytes),
	}
	ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{
		Command: []string{"true"},
		Env:     env,
	})
	wantNoError(t, err)
	h.waitCompleted(sb.ID, ex.ID)
}

// TestCwdValidation covers what the daemon decides without the runtime.
func TestCwdValidation(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		cwd   string
		valid bool
	}{
		{"absolute", "/work", true},
		{"empty means default", "", true},
		{"relative", "work", false},
		{"dot relative", "./work", false},
		{"too long", "/" + strings.Repeat("a", daemon.MaxCwdBytes), false},
		{"NUL byte", "/work\x00x", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.be.Programs["true"] = func(context.Context, []string) (string, string, int) { return "", "", 0 }
			sb := h.create(daemon.CreateRequest{})
			_, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{
				Command: []string{"true"},
				Cwd:     tc.cwd,
			})
			if tc.valid {
				wantNoError(t, err)
				return
			}
			wantCode(t, err, daemon.CodeInvalidRequest)
		})
	}
}

// TestCwdIsEchoedAndEnvIsNot checks the response shape: cwd is a diagnostic a
// client can read back, and the environment is deliberately absent because it
// may carry credentials.
func TestCwdIsEchoedAndEnvIsNot(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.be.Programs["true"] = func(context.Context, []string) (string, string, int) { return "", "", 0 }
	sb := h.create(daemon.CreateRequest{})

	started, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{
		Command: []string{"true"},
		Cwd:     "/work",
		Env:     map[string]string{"SECRET": "hunter2"},
	})
	wantNoError(t, err)
	if started.Cwd == nil || *started.Cwd != "/work" {
		t.Fatalf("start cwd = %v, want /work", started.Cwd)
	}

	done := h.waitCompleted(sb.ID, started.ID)
	if done.Cwd == nil || *done.Cwd != "/work" {
		t.Errorf("completed cwd = %v, want /work", done.Cwd)
	}

	list, err := h.s.ListExecutions(sb.ID)
	wantNoError(t, err)
	if len(list) != 1 || list[0].Cwd == nil || *list[0].Cwd != "/work" {
		t.Errorf("summary cwd = %+v, want /work", list)
	}

	// The wire form is what a client or a log sees, so check it rather than
	// the struct: neither an env field nor the value may appear in any of
	// the three views.
	for name, view := range map[string]any{"start": started, "completed": done, "summary": list[0]} {
		raw, err := json.Marshal(view)
		wantNoError(t, err)
		if strings.Contains(string(raw), `"env"`) || strings.Contains(string(raw), "hunter2") {
			t.Errorf("%s response exposes the environment: %s", name, raw)
		}
	}
}

// TestCwdDefaultIsNull keeps "no cwd requested" distinguishable from a
// directory that happens to equal the image default.
func TestCwdDefaultIsNull(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.be.Programs["true"] = func(context.Context, []string) (string, string, int) { return "", "", 0 }
	sb := h.create(daemon.CreateRequest{})

	started, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"true"}})
	wantNoError(t, err)
	if started.Cwd != nil {
		t.Fatalf("cwd = %v, want null when the request sets none", *started.Cwd)
	}
}

// TestIdempotencyComparesTheWholeInvocation is the regression this feature
// needs: a replay that changes only the environment must not silently return
// the execution that ran with the old one.
func TestIdempotencyComparesTheWholeInvocation(t *testing.T) {
	ctx := context.Background()
	base := daemon.ExecuteRequest{
		Command:        []string{"true"},
		IdempotencyKey: "same-key",
		Cwd:            "/work",
		Env:            map[string]string{"A": "1", "B": "2"},
	}

	t.Run("identical request replays", func(t *testing.T) {
		h := newHarness(t)
		h.be.Programs["true"] = func(context.Context, []string) (string, string, int) { return "", "", 0 }
		sb := h.create(daemon.CreateRequest{})

		first, created, err := h.s.StartExecution(ctx, sb.ID, base)
		wantNoError(t, err)
		if !created {
			t.Fatal("first start reported no new execution")
		}
		h.waitCompleted(sb.ID, first.ID)

		// A different map with the same pairs must still count as the same
		// invocation, which is why the comparison sorts before comparing.
		again := base
		again.Env = map[string]string{"B": "2", "A": "1"}
		replayed, created, err := h.s.StartExecution(ctx, sb.ID, again)
		wantNoError(t, err)
		if created || replayed.ID != first.ID {
			t.Fatalf("replay started a new execution: created=%v id=%s", created, replayed.ID)
		}
	})

	for _, tc := range []struct {
		name   string
		mutate func(*daemon.ExecuteRequest)
	}{
		{"different env value", func(r *daemon.ExecuteRequest) {
			r.Env = map[string]string{"A": "1", "B": "3"}
		}},
		{"extra env entry", func(r *daemon.ExecuteRequest) {
			r.Env = map[string]string{"A": "1", "B": "2", "C": "3"}
		}},
		{"env removed", func(r *daemon.ExecuteRequest) { r.Env = nil }},
		{"different cwd", func(r *daemon.ExecuteRequest) { r.Cwd = "/other" }},
		{"cwd removed", func(r *daemon.ExecuteRequest) { r.Cwd = "" }},
		{"different command", func(r *daemon.ExecuteRequest) { r.Command = []string{"false"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.be.Programs["true"] = func(context.Context, []string) (string, string, int) { return "", "", 0 }
			h.be.Programs["false"] = func(context.Context, []string) (string, string, int) { return "", "", 1 }
			sb := h.create(daemon.CreateRequest{})

			first, _, err := h.s.StartExecution(ctx, sb.ID, base)
			wantNoError(t, err)
			h.waitCompleted(sb.ID, first.ID)

			changed := base
			tc.mutate(&changed)
			_, _, err = h.s.StartExecution(ctx, sb.ID, changed)
			wantCode(t, err, daemon.CodeIdempotencyConflict)
		})
	}
}

// TestExecSpecReachesTheBackend proves the fields survive the engine: a value
// that only reaches a local struct would pass the API tests and still run the
// command with no environment at all.
func TestExecSpecReachesTheBackend(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	var got daemon.ExecSpec
	h.be.Programs["capture"] = func(context.Context, []string) (string, string, int) { return "", "", 0 }
	// membackend records the spec on the process it returns, so the harness
	// observes it through the public surface.
	sb := h.create(daemon.CreateRequest{})

	ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{
		Command: []string{"capture"},
		Env:     map[string]string{"B": "2", "A": "1"},
		Cwd:     "/work",
	})
	wantNoError(t, err)
	h.waitCompleted(sb.ID, ex.ID)

	got = h.be.LastExecSpec()
	if got.Dir != "/work" {
		t.Errorf("dir = %q, want /work", got.Dir)
	}
	wantEnv := []string{"A=1", "B=2"}
	if strings.Join(got.Env, ",") != strings.Join(wantEnv, ",") {
		t.Errorf("env = %v, want %v (sorted, so a replay is deterministic)", got.Env, wantEnv)
	}
}

// TestExecEnvironmentTotalBoundary pins the total-bytes bound on both sides of
// the edge. Every entry stays under the per-entry limits, so only the sum can
// reject the request, and the accepted case exercises the arithmetic that
// counts the "=" between name and value.
func TestExecEnvironmentTotalBoundary(t *testing.T) {
	ctx := context.Background()

	// maxValueFilling fills MaxEnvEntries sized entries whose names, "=" and
	// values add up to exactly total.
	fill := func(total int) map[string]string {
		perEntry := total / daemon.MaxEnvEntries
		env := make(map[string]string, daemon.MaxEnvEntries)
		for i := 0; i < daemon.MaxEnvEntries; i++ {
			name := strings.Repeat(string(rune('a'+i%26)), daemon.MaxEnvKeyBytes)[:daemon.MaxEnvKeyBytes-4] + fmt.Sprintf("%04d", i)
			env[name] = strings.Repeat("v", perEntry-len(name)-1)
		}
		return env
	}

	total := func(env map[string]string) int {
		sum := 0
		for k, v := range env {
			sum += len(k) + 1 + len(v)
		}
		return sum
	}

	atLimit := fill(daemon.MaxEnvTotalBytes)
	if got := total(atLimit); got != daemon.MaxEnvTotalBytes {
		t.Fatalf("test built %d bytes, want exactly %d", got, daemon.MaxEnvTotalBytes)
	}
	h := newHarness(t)
	h.be.Programs["true"] = func(context.Context, []string) (string, string, int) { return "", "", 0 }
	sb := h.create(daemon.CreateRequest{})
	ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"true"}, Env: atLimit})
	wantNoError(t, err)
	h.waitCompleted(sb.ID, ex.ID)

	overLimit := fill(daemon.MaxEnvTotalBytes)
	for k, v := range overLimit {
		overLimit[k] = v + "x"
		break
	}
	if got := total(overLimit); got != daemon.MaxEnvTotalBytes+1 {
		t.Fatalf("test built %d bytes, want %d", got, daemon.MaxEnvTotalBytes+1)
	}
	_, _, err = h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"true"}, Env: overLimit})
	wantCode(t, err, daemon.CodeInvalidRequest)
	if !strings.Contains(err.Error(), "in total") {
		t.Fatalf("error = %v, want it to mention the total", err)
	}
}

// TestBadCwdReplayIsByteExact replays the execution behind a failed cwd. The
// marker is stderr-only, so a replay that lost stderr, downgraded it to stdout,
// or re-split it differently would hand an agent a different explanation than
// the one it would have streamed live.
func TestBadCwdReplayIsByteExact(t *testing.T) {
	ctx := context.Background()
	const dir = "/does-not-exist"
	const marker = "sandbox: cwd is not a usable directory: " + dir + "\n"

	h := newHarness(t)
	h.be.Programs["nowhere"] = func(context.Context, []string) (string, string, int) {
		return "", marker, 126
	}
	sb := h.create(daemon.CreateRequest{})
	ex, _, err := h.s.StartExecution(ctx, sb.ID, daemon.ExecuteRequest{Command: []string{"nowhere"}, Cwd: dir})
	wantNoError(t, err)
	done := h.waitCompleted(sb.ID, ex.ID)

	if done.ExitCode == nil || *done.ExitCode != 126 {
		t.Fatalf("exit code = %v, want 126", done.ExitCode)
	}
	if done.Cwd == nil || *done.Cwd != dir {
		t.Fatalf("cwd = %v, want %q", done.Cwd, dir)
	}
	if done.Stdout != "" {
		t.Fatalf("stdout = %q, want empty: the marker must not look like program output", done.Stdout)
	}
	if done.Stderr != marker {
		t.Fatalf("retained stderr = %q, want %q", done.Stderr, marker)
	}

	st, err := h.s.Events(sb.ID, ex.ID, 0)
	wantNoError(t, err)
	defer st.Close()
	var out, errOut strings.Builder
	var complete *daemon.Execution
	for _, ev := range collect(t, st) {
		switch ev.Type {
		case daemon.EventStdout:
			out.Write(ev.Data)
		case daemon.EventStderr:
			errOut.Write(ev.Data)
		case daemon.EventComplete:
			complete = ev.Execution
		}
	}
	if out.Len() != 0 {
		t.Fatalf("replayed %d stdout bytes, want none", out.Len())
	}
	if errOut.String() != marker {
		t.Fatalf("replayed stderr = %q, want %q", errOut.String(), marker)
	}
	if complete == nil || complete.ExitCode == nil || *complete.ExitCode != 126 {
		t.Fatalf("complete event = %+v, want exit code 126", complete)
	}
}
