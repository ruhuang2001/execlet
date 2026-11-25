//go:build docker

package dockerbackend

import (
	"context"
	"crypto/rand"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ruhuang2001/execlet/internal/daemon"
	"github.com/ruhuang2001/execlet/internal/dockerapi"
)

func dockerSocket(t *testing.T) string {
	t.Helper()
	if host := os.Getenv("DOCKER_HOST"); strings.HasPrefix(host, "unix://") {
		return strings.TrimPrefix(host, "unix://")
	}
	return "/var/run/docker.sock"
}

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

func (h *realHarness) exec(sandboxID string, command ...string) string {
	h.t.Helper()
	ex, _, err := h.s.StartExecution(context.Background(), sandboxID, daemon.ExecuteRequest{Command: command})
	if err != nil {
		h.t.Fatalf("exec %v: %v", command, err)
	}
	return ex.ID
}

func (h *realHarness) output(sandboxID, executionID string) string {
	h.t.Helper()
	ex, err := h.s.GetExecution(sandboxID, executionID)
	if err != nil {
		h.t.Fatalf("get execution: %v", err)
	}
	return ex.Stdout
}

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

func TestRealCounterAdvancesAcrossPause(t *testing.T) {
	h := newRealHarness(t)
	sb := h.create()
	id := h.exec(sb.ID, "sh", "-c", "i=0; while true; do i=$((i+1)); echo $i; sleep 0.05; done")

	h.waitFor("initial output", 10*time.Second, func() bool {
		return strings.Count(h.output(sb.ID, id), "\n") >= 3
	})

	if _, err := h.s.Pause(context.Background(), sb.ID); err != nil {
		t.Fatalf("pause: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
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

func TestRealExternalKillIsEvicted(t *testing.T) {
	h := newRealHarness(t)
	sb := h.create()
	id := h.exec(sb.ID, "sh", "-c", "echo ready; exec sleep 100")
	h.waitFor("command to start", 10*time.Second, func() bool {
		return h.output(sb.ID, id) == "ready\n"
	})

	list, err := h.api.ContainerList(context.Background(), true, map[string][]string{
		"label": {daemon.OwnerLabelKey + "=" + h.owner},
	})
	if err != nil || len(list) != 1 {
		t.Fatalf("find container: %v, %d containers", err, len(list))
	}
	container := list[0].ID
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

func derefInt(v *int) int {
	if v == nil {
		return -1
	}
	return *v
}

func (h *realHarness) run(sandboxID string, req daemon.ExecuteRequest) daemon.Execution {
	h.t.Helper()
	ex, _, err := h.s.StartExecution(context.Background(), sandboxID, req)
	if err != nil {
		h.t.Fatalf("start %v: %v", req.Command, err)
	}
	return h.waitCompleted(sandboxID, ex.ID)
}

func TestRealWorkingDirectoryIsPhysical(t *testing.T) {
	h := newRealHarness(t)
	sb := h.create()

	fixture := h.run(sb.ID, daemon.ExecuteRequest{
		Command: []string{"sh", "-c", "mkdir -p /work /data/project && ln -s /data/project /work/link"},
	})
	if fixture.ExitCode == nil || *fixture.ExitCode != 0 {
		t.Fatalf("fixture: exit %d, stderr %q", derefInt(fixture.ExitCode), fixture.Stderr)
	}

	if ex := h.run(sb.ID, daemon.ExecuteRequest{Command: []string{"pwd"}, Cwd: "/work/link/.."}); strings.TrimSpace(ex.Stdout) != "/data" {
		t.Errorf("pwd in /work/link/.. = %q (exit %d), want /data; the path was "+
			"simplified lexically instead of resolved", ex.Stdout, derefInt(ex.ExitCode))
	}

	ex := h.run(sb.ID, daemon.ExecuteRequest{Command: []string{"pwd"}, Cwd: "/tmp/nonexistent-review-directory/.."})
	if ex.ExitCode == nil || *ex.ExitCode != 126 {
		t.Errorf("exit in /tmp/nonexistent-review-directory/.. = %d (stdout %q), "+
			"want 126: a missing component followed by .. is not a directory", derefInt(ex.ExitCode), ex.Stdout)
	}
	if !strings.Contains(ex.Stderr, "cwd is not a usable directory") {
		t.Errorf("stderr = %q, want the cwd marker", ex.Stderr)
	}

	if ex := h.run(sb.ID, daemon.ExecuteRequest{Command: []string{"pwd"}, Cwd: "/work"}); strings.TrimSpace(ex.Stdout) != "/work" {
		t.Errorf("pwd in /work = %q (exit %d), want /work", ex.Stdout, derefInt(ex.ExitCode))
	}
}

func TestRealEnvironmentSurvivesTheShell(t *testing.T) {
	h := newRealHarness(t)
	sb := h.create()

	want := map[string]string{
		"MY-VAR":     "hyphen",
		"CONFIG.KEY": "dot",
		"SHLVL":      "42",
		"IFS":        "connector",
		"NORMAL":     "kept",
		"-X":         "dash",
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

func TestRealEnvironmentOverrideAppliesWithoutEnvOnPath(t *testing.T) {
	h := newRealHarness(t)
	sb := h.create()

	fixture := h.run(sb.ID, daemon.ExecuteRequest{
		Command: []string{"sh", "-c", "mkdir -p /custom && printf '#!/bin/sh\\necho custom-bin-ran\\n' > /custom/mybin && chmod +x /custom/mybin"},
	})
	if fixture.ExitCode == nil || *fixture.ExitCode != 0 {
		t.Fatalf("fixture: exit %d, stderr %q", derefInt(fixture.ExitCode), fixture.Stderr)
	}

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
			launcher := `export n=image-n e=image-e
wrapper=$1
count=$2
shift 2
exec /bin/sh -c "$wrapper" /tmp/shadow-pid "" "$count" 0 "$@"`
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
			for _, entry := range tc.env {
				name, value, _ := strings.Cut(entry, "=")
				if got[name] != value {
					t.Errorf("%s = %q, want %q: the entry did not reach env", name, got[name], value)
				}
			}
		})
	}
}

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

func TestRealCreateNameConflict(t *testing.T) {
	h := newRealHarness(t)
	for _, tc := range []struct {
		name                                                     string
		foreignOwner, differentSandbox, legacy, running, replace bool
	}{
		{name: "different-owner", foreignOwner: true},
		{name: "different-sandbox", differentSandbox: true},
		{name: "legacy", legacy: true},
		{name: "running", running: true},
		{name: "stopped-owned", replace: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			id := "sbx_" + rand.Text()
			name := "execlet-" + id
			labels := map[string]string{daemon.OwnerLabelKey: h.owner, daemon.SandboxIDLabelKey: id}
			if tc.foreignOwner {
				labels[daemon.OwnerLabelKey] += "-other"
			}
			if tc.differentSandbox {
				labels[daemon.SandboxIDLabelKey] += "-other"
			}
			if tc.legacy {
				delete(labels, daemon.SandboxIDLabelKey)
			}
			cmd := []string{"true"}
			if tc.running {
				cmd = []string{"sleep", "120"}
			}
			cfg := dockerapi.ContainerConfig{Image: "alpine:3.20", Cmd: cmd, Labels: labels}
			original, err := h.api.ContainerCreate(ctx, name, cfg)
			if dockerapi.IsNoSuchImage(err) {
				if err := h.api.PullImage(ctx, cfg.Image); err != nil {
					t.Fatal(err)
				}
				original, err = h.api.ContainerCreate(ctx, name, cfg)
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				err := h.api.ContainerRemove(context.Background(), original)
				if err != nil && !dockerapi.IsNotFound(err) {
					t.Errorf("remove fixture: %v", err)
				}
			})
			if err := h.api.ContainerStart(ctx, original); err != nil {
				t.Fatal(err)
			}
			if !tc.running {
				h.waitFor("fixture exit", 10*time.Second, func() bool {
					inspect, err := h.api.ContainerInspect(ctx, original)
					return err == nil && inspect.State.Status == "exited"
				})
			}
			spec := daemon.ContainerSpec{Name: name, Image: cfg.Image, MemoryMB: 64,
				Labels: map[string]string{daemon.OwnerLabelKey: h.owner, daemon.SandboxIDLabelKey: id}}
			var backend daemon.Backend = h.be
			var policy *daemon.PolicyBackend
			if !tc.replace {
				spec.Network, spec.NanoCPUs, spec.PidsLimit = daemon.NetworkNone, 100_000_000, 64
				policy, err = daemon.NewPolicyBackend(h.be, daemon.DeploymentPolicy{MaxSandboxes: 1, MemoryMB: 64, NanoCPUs: spec.NanoCPUs, Pids: 64, Images: []string{cfg.Image}, Networks: []daemon.NetworkMode{spec.Network}})
				if err != nil {
					t.Fatal(err)
				}
				backend = policy
			}
			replacement, err := backend.Create(ctx, spec)
			t.Logf("original=%s replacement=%q", original, replacement)
			if tc.replace {
				if err != nil || replacement == "" || replacement == original {
					t.Fatalf("replacement = %q, err = %v", replacement, err)
				}
				if _, err := h.api.ContainerInspect(ctx, original); !dockerapi.IsNotFound(err) {
					t.Fatalf("original lookup: %v", err)
				}
			} else {
				if !dockerapi.IsConflict(err) || daemon.CodeOf(err) != daemon.CodeInvalidState {
					t.Fatalf("expected name conflict, got %v", err)
				}
				if policy.Usage().Sandboxes != 0 {
					t.Fatal("conflict retained a policy reservation")
				}
				inspect, err := h.api.ContainerInspect(ctx, name)
				if err != nil || inspect.ID != original || inspect.State.Running != tc.running {
					t.Fatalf("fixture changed: %#v, %v", inspect, err)
				}
			}
		})
	}
}
