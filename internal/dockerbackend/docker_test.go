//go:build docker

// Package dockerbackend's real-Docker tests. They need a running Docker daemon
// and are excluded from ordinary runs:
//
//	go test -tags docker ./internal/dockerbackend/ -run TestReal -v
//
// Each test cleans up its own containers and asserts the daemon's container
// list is empty afterwards.
package dockerbackend

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
	"github.com/ruhuang2001/tiny-sandbox/internal/dockerapi"
)

// dockerSocket returns the daemon socket path, honouring DOCKER_HOST.
func dockerSocket(t *testing.T) string {
	t.Helper()
	if host := os.Getenv("DOCKER_HOST"); strings.HasPrefix(host, "unix://") {
		return strings.TrimPrefix(host, "unix://")
	}
	return "/var/run/docker.sock"
}

// realHarness is a daemon store over the real Docker backend, with an owner
// label unique to the test so cleanup can find everything it created.
type realHarness struct {
	t     *testing.T
	owner string
	api   *dockerapi.Client
	be    *Backend
	s     *daemon.Sandboxes
}

func newRealHarness(t *testing.T) *realHarness {
	t.Helper()
	sock := dockerSocket(t)
	if _, err := os.Stat(sock); err != nil {
		t.Skipf("docker socket %s: %v", sock, err)
	}
	api, err := dockerapi.Dial(context.Background(), sock)
	if err != nil {
		t.Skipf("docker daemon not reachable: %v", err)
	}
	owner := "test-" + t.Name()
	be := New(api)
	h := &realHarness{
		t:     t,
		owner: owner,
		api:   api,
		be:    be,
		s:     daemon.New(be, daemon.WithOwnerLabel(owner)),
	}
	t.Cleanup(func() {
		n, err := be.RemoveOwned(context.Background(), daemon.OwnerLabelKey, owner)
		if err != nil {
			t.Errorf("cleanup: %v", err)
		}
		t.Logf("cleanup removed %d containers", n)
		left, err := api.ContainerList(context.Background(), true, map[string][]string{
			"label": {daemon.OwnerLabelKey + "=" + owner},
		})
		if err != nil {
			t.Errorf("list after cleanup: %v", err)
		}
		if len(left) != 0 {
			t.Errorf("%d containers remain after cleanup", len(left))
		}
	})
	return h
}

func (h *realHarness) create() daemon.Sandbox {
	h.t.Helper()
	sb, err := h.s.Create(context.Background(), daemon.CreateRequest{
		Image:          ptr("alpine:3.20"),
		MemoryMB:       ptr(64),
		TimeoutSeconds: ptr(300),
	})
	if err != nil {
		h.t.Fatalf("create: %v", err)
	}
	return sb
}

func ptr[T any](v T) *T { return &v }

// exec runs a command and returns its execution ID.
func (h *realHarness) exec(sandboxID string, command ...string) string {
	h.t.Helper()
	ex, _, err := h.s.StartExecution(context.Background(), sandboxID, daemon.ExecuteRequest{Command: command})
	if err != nil {
		h.t.Fatalf("exec %v: %v", command, err)
	}
	return ex.ID
}

// output returns the retained stdout of an execution.
func (h *realHarness) output(sandboxID, executionID string) string {
	h.t.Helper()
	ex, err := h.s.GetExecution(sandboxID, executionID)
	if err != nil {
		h.t.Fatalf("get execution: %v", err)
	}
	return ex.Stdout
}

// waitFor polls cond until it holds, failing the test on timeout.
func (h *realHarness) waitFor(what string, timeout time.Duration, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for %s", what)
}

// TestRealCounterAdvancesAcrossPause proves pause freezes processes rather
// than only flipping a state field: a counter that prints while it runs must
// stop advancing while the sandbox is paused and continue after resume.
func TestRealCounterAdvancesAcrossPause(t *testing.T) {
	h := newRealHarness(t)
	sb := h.create()
	id := h.exec(sb.ID, "sh", "-c", "i=0; while true; do i=$((i+1)); echo $i; sleep 0.05; done")

	// Let a few lines accumulate.
	h.waitFor("initial output", 10*time.Second, func() bool {
		return strings.Count(h.output(sb.ID, id), "\n") >= 3
	})

	if _, err := h.s.Pause(context.Background(), sb.ID); err != nil {
		t.Fatalf("pause: %v", err)
	}
	time.Sleep(200 * time.Millisecond) // let an in-flight write land
	frozen := h.output(sb.ID, id)
	time.Sleep(700 * time.Millisecond)
	if after := h.output(sb.ID, id); after != frozen {
		t.Fatalf("output advanced while paused: %d -> %d bytes", len(frozen), len(after))
	}

	if _, err := h.s.Resume(context.Background(), sb.ID); err != nil {
		t.Fatalf("resume: %v", err)
	}
	h.waitFor("output after resume", 10*time.Second, func() bool {
		return len(h.output(sb.ID, id)) > len(frozen)
	})

	if _, err := h.s.Delete(context.Background(), sb.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

// TestRealExternalKillIsEvicted stops the container behind the daemon's back
// with kill -9. The process exits 137, which must classify as evicted, never
// as oom: no intent was recorded and the cgroup counter did not move.
func TestRealExternalKillIsEvicted(t *testing.T) {
	h := newRealHarness(t)
	sb := h.create()
	id := h.exec(sb.ID, "sleep", "100")

	// The sandbox's container is the one carrying this test's owner label;
	// the daemon's in-memory record is not reachable from another package.
	list, err := h.api.ContainerList(context.Background(), true, map[string][]string{
		"label": {daemon.OwnerLabelKey + "=" + h.owner},
	})
	if err != nil || len(list) != 1 {
		t.Fatalf("find container: %v, %d containers", err, len(list))
	}
	container := list[0].ID
	// Externally kill the container's init process. This is the `docker kill`
	// an operator would type, not a daemon request, so nothing records intent.
	if err := h.api.ContainerKill(context.Background(), container, "KILL"); err != nil {
		t.Fatalf("external kill: %v", err)
	}

	h.waitFor("execution to complete", 10*time.Second, func() bool {
		ex, err := h.s.GetExecution(sb.ID, id)
		return err == nil && ex.State == daemon.ExecCompleted
	})
	ex, err := h.s.GetExecution(sb.ID, id)
	if err != nil {
		t.Fatalf("get execution: %v", err)
	}
	if ex.TerminationReason == nil || *ex.TerminationReason != daemon.ReasonEvicted {
		t.Fatalf("termination_reason = %v, want evicted", ex.TerminationReason)
	}
	if ex.ExitCode == nil || *ex.ExitCode != 137 {
		t.Logf("exit code = %v (137 expected but not required for eviction)", ex.ExitCode)
	}

	// The eviction sweep must also kill the sandbox itself.
	h.waitFor("sandbox to be killed", 10*time.Second, func() bool {
		got, err := h.s.Get(sb.ID)
		return err == nil && got.State == daemon.StateKilled
	})
	got, err := h.s.Get(sb.ID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if got.KillReason == nil || *got.KillReason != daemon.KillEvicted {
		t.Fatalf("kill_reason = %v, want evicted", got.KillReason)
	}
}

// TestRealOOMIsOOM proves the cgroup counter distinguishes the kernel OOM
// killer from every other 137: a process that exceeds the container limit is
// killed by the kernel, and the delta attributes it to the execution.
func TestRealOOMIsOOM(t *testing.T) {
	h := newRealHarness(t)
	sb, err := h.s.Create(context.Background(), daemon.CreateRequest{
		Image:          ptr("python:3.12-alpine"),
		MemoryMB:       ptr(64),
		TimeoutSeconds: ptr(120),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Allocate 256 MiB of anonymous memory in a 64 MiB container. Anonymous
	// pages cannot be reclaimed the way page cache can, so the kernel kills
	// the process instead of dropping data. A file the page cache owns would
	// be reclaimed and the command would exit 0.
	id := h.exec(sb.ID, "python3", "-c", "x = bytearray(256 * 1024 * 1024)")

	h.waitFor("execution to complete", 30*time.Second, func() bool {
		ex, err := h.s.GetExecution(sb.ID, id)
		return err == nil && ex.State == daemon.ExecCompleted
	})
	ex, err := h.s.GetExecution(sb.ID, id)
	if err != nil {
		t.Fatalf("get execution: %v", err)
	}
	if ex.TerminationReason == nil || *ex.TerminationReason != daemon.ReasonOOM {
		reason := "nil"
		if ex.TerminationReason != nil {
			reason = string(*ex.TerminationReason)
		}
		exit := -1
		if ex.ExitCode != nil {
			exit = *ex.ExitCode
		}
		t.Fatalf("termination_reason = %s, want oom (exit %d, stderr %q)", reason, exit, ex.Stderr)
	}
}

// waitCompleted blocks until an execution is terminal and returns it.
func (h *realHarness) waitCompleted(sandboxID, executionID string) daemon.Execution {
	h.t.Helper()
	h.waitFor("execution to complete", 15*time.Second, func() bool {
		ex, err := h.s.GetExecution(sandboxID, executionID)
		return err == nil && ex.State == daemon.ExecCompleted
	})
	ex, err := h.s.GetExecution(sandboxID, executionID)
	if err != nil {
		h.t.Fatalf("get execution: %v", err)
	}
	return ex
}

// derefInt reports an exit code for a failure message. A nil code means the
// execution did not exit, which is worth seeing as such in the output.
func derefInt(v *int) int {
	if v == nil {
		return -1
	}
	return *v
}

// run starts an execution through the daemon, the same path a client takes,
// and returns it once terminal.
func (h *realHarness) run(sandboxID string, req daemon.ExecuteRequest) daemon.Execution {
	h.t.Helper()
	ex, _, err := h.s.StartExecution(context.Background(), sandboxID, req)
	if err != nil {
		h.t.Fatalf("start %v: %v", req.Command, err)
	}
	return h.waitCompleted(sandboxID, ex.ID)
}

// TestRealWorkingDirectoryIsPhysical proves the directory is resolved the way
// the filesystem resolves it, not the way a shell abbreviates it.
//
// The wrapper chdirs, so the shell's own rules apply unless -P overrides them:
// a logical cd simplifies ".." lexically, before it looks at the filesystem.
// With link -> /data, a request for /work/link/.. then lands in /work, and
// /nope/.. succeeds because ".." removes the component that does not exist.
// Both would run the command somewhere the caller never named.
func TestRealWorkingDirectoryIsPhysical(t *testing.T) {
	h := newRealHarness(t)
	sb := h.create()

	fixture := h.run(sb.ID, daemon.ExecuteRequest{
		Command: []string{"sh", "-c", "mkdir -p /work /data/project && ln -s /data/project /work/link"},
	})
	if fixture.ExitCode == nil || *fixture.ExitCode != 0 {
		t.Fatalf("fixture: exit %d, stderr %q", derefInt(fixture.ExitCode), fixture.Stderr)
	}

	// The symlink is followed first, so ".." applies to its target.
	if ex := h.run(sb.ID, daemon.ExecuteRequest{Command: []string{"pwd"}, Cwd: "/work/link/.."}); strings.TrimSpace(ex.Stdout) != "/data" {
		t.Errorf("pwd in /work/link/.. = %q (exit %d), want /data; the path was "+
			"simplified lexically instead of resolved", ex.Stdout, derefInt(ex.ExitCode))
	}

	// A component that does not exist cannot be removed by "..", so this is
	// an unusable directory rather than /tmp.
	ex := h.run(sb.ID, daemon.ExecuteRequest{Command: []string{"pwd"}, Cwd: "/tmp/nonexistent-review-directory/.."})
	if ex.ExitCode == nil || *ex.ExitCode != 126 {
		t.Errorf("exit in /tmp/nonexistent-review-directory/.. = %d (stdout %q), "+
			"want 126: a missing component followed by .. is not a directory", derefInt(ex.ExitCode), ex.Stdout)
	}
	if !strings.Contains(ex.Stderr, "cwd is not a usable directory") {
		t.Errorf("stderr = %q, want the cwd marker", ex.Stderr)
	}

	// The plain path still works, so -P did not break the ordinary case.
	if ex := h.run(sb.ID, daemon.ExecuteRequest{Command: []string{"pwd"}, Cwd: "/work"}); strings.TrimSpace(ex.Stdout) != "/work" {
		t.Errorf("pwd in /work = %q (exit %d), want /work", ex.Stdout, derefInt(ex.ExitCode))
	}
}

// TestRealEnvironmentSurvivesTheShell proves the requested entries reach the
// target unchanged, including names a shell rejects.
//
// The exec environment passes through /bin/sh, which imports only names it
// considers valid identifiers: dash drops MY-VAR and CONFIG.KEY outright, and
// both shells rewrite IFS, PWD and SHLVL. The wrapper's env(1) step is what
// makes the target's environment the one the caller asked for, on whichever
// shell the image ships.
func TestRealEnvironmentSurvivesTheShell(t *testing.T) {
	h := newRealHarness(t)
	sb := h.create()

	want := map[string]string{
		"MY-VAR":     "hyphen",
		"CONFIG.KEY": "dot",
		"SHLVL":      "42",
		"IFS":        "connector",
		"NORMAL":     "kept",
		// A name starting with "-" is accepted by the contract and would be
		// read as an option by env without "--" in front of the entries.
		"-X": "dash",
	}
	ex := h.run(sb.ID, daemon.ExecuteRequest{Command: []string{"env"}, Env: want})
	if ex.ExitCode == nil || *ex.ExitCode != 0 {
		t.Fatalf("env: exit %d, stderr %q", derefInt(ex.ExitCode), ex.Stderr)
	}

	got := map[string]string{}
	for _, line := range strings.Split(ex.Stdout, "\n") {
		if name, value, ok := strings.Cut(line, "="); ok {
			got[name] = value
		}
	}
	for name, value := range want {
		if got[name] != value {
			t.Errorf("%s = %q, want %q; the wrapper did not pass the environment "+
				"through unchanged", name, got[name], value)
		}
	}
}

// TestRealEnvironmentOverrideAppliesWithoutEnvOnPath proves the environment
// step does not depend on PATH. A request that replaces PATH would otherwise
// hide env(1) itself and fail every execution that sets a variable.
func TestRealEnvironmentOverrideAppliesWithoutEnvOnPath(t *testing.T) {
	h := newRealHarness(t)
	sb := h.create()

	fixture := h.run(sb.ID, daemon.ExecuteRequest{
		Command: []string{"sh", "-c", "mkdir -p /custom && printf '#!/bin/sh\\necho custom-bin-ran\\n' > /custom/mybin && chmod +x /custom/mybin"},
	})
	if fixture.ExitCode == nil || *fixture.ExitCode != 0 {
		t.Fatalf("fixture: exit %d, stderr %q", derefInt(fixture.ExitCode), fixture.Stderr)
	}

	// The command is found only through the PATH the request sets, which also
	// means env(1) is not on it.
	ex := h.run(sb.ID, daemon.ExecuteRequest{
		Command: []string{"mybin"},
		Env:     map[string]string{"PATH": "/custom"},
	})
	if ex.ExitCode == nil || *ex.ExitCode != 0 {
		t.Fatalf("exit = %d, stderr %q, want the command found through the requested PATH", derefInt(ex.ExitCode), ex.Stderr)
	}
	if strings.TrimSpace(ex.Stdout) != "custom-bin-ran" {
		t.Errorf("stdout = %q, want the command's output", ex.Stdout)
	}
}

// TestRealCommandPathWithEqualsIsNotAnAssignment proves the environment step
// cannot swallow the command.
//
// env(1) treats every leading NAME=VALUE as an assignment and only then starts
// the command; "--" ends option parsing, not assignment parsing. A command
// whose own path contains "=" would therefore become one more variable: env
// would run nothing, exit 0, and print the environment, secrets included, as
// if the program had produced that output.
func TestRealCommandPathWithEqualsIsNotAnAssignment(t *testing.T) {
	h := newRealHarness(t)
	sb := h.create()

	fixture := h.run(sb.ID, daemon.ExecuteRequest{
		Command: []string{"sh", "-c", `printf '#!/bin/sh\necho tool-ran\n' > '/tmp/tool=v1' && chmod +x '/tmp/tool=v1'`},
	})
	if fixture.ExitCode == nil || *fixture.ExitCode != 0 {
		t.Fatalf("fixture: exit %d, stderr %q", derefInt(fixture.ExitCode), fixture.Stderr)
	}

	ex := h.run(sb.ID, daemon.ExecuteRequest{
		Command: []string{"/tmp/tool=v1"},
		Env:     map[string]string{"TOKEN": "secret-value"},
	})
	if got := strings.TrimSpace(ex.Stdout); got != "tool-ran" {
		t.Fatalf("stdout = %q, want the program's own output: the command was "+
			"treated as another assignment (stderr %q)", got, ex.Stderr)
	}
	if strings.Contains(ex.Stdout, "secret-value") {
		t.Errorf("stdout leaked the environment, secrets included:\n%s", ex.Stdout)
	}
}

// TestRealWrapperDoesNotShadowImageVariables runs the real wrapper in a shell
// that already exports n and e.
//
// A shell assignment to a name that is already exported replaces that value,
// and the wrapper is a shell. An earlier version read its argument count into
// "n" and probed env into "e", so an image that exports either name had it
// silently rewritten, even for a request that set no environment at all.
//
// The variables come from the shell that starts the wrapper rather than from
// the image, because a request-level entry is supposed to win and so cannot
// express "the image already sets this". Exporting them first is exactly the
// environment an image produces.
//
// The outer shell is a launcher, not part of the subject: it exports the
// names, then execs the wrapper with the wrapper's own argument layout —
// $0 the PID path, $1 the directory, $2 the count, then the entries and the
// command. Passing the wrapper text through a positional parameter and
// forwarding the rest keeps every argument where the wrapper expects it; an
// earlier version of this test shifted them, so the wrapper ran against the
// wrong layout and passed for the wrong reason.
func TestRealWrapperDoesNotShadowImageVariables(t *testing.T) {
	h := newRealHarness(t)
	sb := h.create()

	for _, tc := range []struct {
		name  string
		count string
		env   []string
		cmd   []string
	}{
		{"no environment", "0", nil, []string{"/usr/bin/env"}},
		{"one unrelated entry", "1", []string{"EXTRA=value"}, []string{"/usr/bin/env"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// $0 is the launcher source, $1 the wrapper text to run, $2 the
			// count, and "$@" after the shift the rest of the wrapper's argv.
			launcher := `export n=image-n e=image-e
wrapper=$1
count=$2
shift 2
exec /bin/sh -c "$wrapper" /tmp/shadow-pid "" "$count" "$@"`
			argv := append([]string{"sh", "-c", launcher, "sandbox", execWrapper, tc.count}, tc.env...)
			argv = append(argv, tc.cmd...)

			proc, err := h.be.Exec(context.Background(), h.containerID(sb.ID), daemon.ExecSpec{Command: argv})
			if err != nil {
				t.Fatalf("exec: %v", err)
			}
			out, err := io.ReadAll(proc.Stdout())
			if err != nil {
				t.Fatalf("read stdout: %v", err)
			}
			stderr, err := io.ReadAll(proc.Stderr())
			if err != nil {
				t.Fatalf("read stderr: %v", err)
			}
			if code, err := proc.Wait(); err != nil || code != 0 {
				t.Fatalf("wait: code %d, err %v, stderr %q", code, err, stderr)
			}

			got := map[string]string{}
			for _, line := range strings.Split(string(out), "\n") {
				if name, value, ok := strings.Cut(line, "="); ok {
					got[name] = value
				}
			}
			// The probe must have produced an environment at all: an empty map
			// would make the two checks below pass without the wrapper ever
			// reaching env.
			if len(got) == 0 {
				t.Fatalf("the probe printed no environment, so it never ran (stderr %q)", stderr)
			}
			if got["n"] != "image-n" {
				t.Errorf("n = %q, want image-n: the wrapper overwrote a variable "+
					"that was already exported", got["n"])
			}
			if got["e"] != "image-e" {
				t.Errorf("e = %q, want image-e: the wrapper overwrote a variable "+
					"that was already exported", got["e"])
			}
		})
	}
}

// containerID returns the container behind a sandbox.
func (h *realHarness) containerID(sandboxID string) string {
	h.t.Helper()
	list, err := h.api.ContainerList(context.Background(), true, map[string][]string{
		"label": {daemon.OwnerLabelKey + "=" + h.owner},
	})
	if err != nil || len(list) != 1 {
		h.t.Fatalf("find container: %v, %d containers", err, len(list))
	}
	return list[0].ID
}
