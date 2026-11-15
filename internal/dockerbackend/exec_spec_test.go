package dockerbackend

import (
	"context"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
	"github.com/ruhuang2001/tiny-sandbox/internal/dockerapi"
)

// TestExecPassesEnvironmentAlong proves the environment reaches the runtime
// twice: as the exec environment, and as arguments the wrapper re-applies at
// the last step. Only the second survives a shell that filters names it
// considers invalid, which is what dash does to MY-VAR and CONFIG.KEY.
func TestExecPassesEnvironmentAlong(t *testing.T) {
	f := newFakeDaemon(t)
	b := f.backend(t)
	id, err := b.Create(context.Background(), daemon.ContainerSpec{Image: "alpine:3.20", MemoryMB: 64})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	env := []string{"A=1", "B=two"}
	if _, err := b.Exec(context.Background(), id, daemon.ExecSpec{
		Command: []string{"true"},
		Env:     env,
	}); err != nil {
		t.Fatalf("exec: %v", err)
	}

	if got := f.lastExecEnv(); !slices.Equal(got, env) {
		t.Fatalf("exec env = %v, want the request's environment", got)
	}
	_, _, wrapped, actual, ok := unwrap(f.lastExecCommand())
	if !ok {
		t.Fatal("command was not wrapped")
	}
	if !slices.Equal(wrapped, env) {
		t.Errorf("wrapper environment = %v, want %v", wrapped, env)
	}
	if !slices.Equal(actual, []string{"true"}) {
		t.Errorf("real command = %v, want [true]", actual)
	}
}

// TestExecUsesTheWrapperForCwd pins how the directory reaches the process.
//
// Docker's own exec WorkingDir is applied before the command runs, so an
// unusable directory becomes a runtime startup failure: exit 127, the message
// on stdout, which is indistinguishable from a missing command. The directory
// must therefore be handled inside the wrapper, after the PID write, and must
// never appear as ExecConfig.WorkingDir.
func TestExecUsesTheWrapperForCwd(t *testing.T) {
	f := newFakeDaemon(t)
	b := f.backend(t)
	id, err := b.Create(context.Background(), daemon.ContainerSpec{Image: "alpine:3.20", MemoryMB: 64})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := b.Exec(context.Background(), id, daemon.ExecSpec{
		Command: []string{"pwd"},
		Dir:     "/work",
	}); err != nil {
		t.Fatalf("exec: %v", err)
	}

	cmd := f.lastExecCommand()
	if len(cmd) < 7 {
		t.Fatalf("command was not wrapped with a directory: %v", cmd)
	}
	if cmd[2] != execWrapper {
		t.Fatalf("cmd[2] is not the wrapper script: %q", cmd[2])
	}
	if cmd[4] != "/work" {
		t.Errorf("wrapper directory argument = %q, want /work", cmd[4])
	}
	// The wrapper must write the PID before it can fail on the directory, so
	// the write has to come first in the script.
	pidWrite := strings.Index(cmd[2], "printf %s $$ >")
	chdir := strings.Index(cmd[2], "cd -P --")
	if pidWrite < 0 || chdir < 0 {
		t.Fatalf("wrapper lost the PID write or the chdir: %q", cmd[2])
	}
	if pidWrite > chdir {
		t.Errorf("wrapper chdirs before writing the PID; every step after the PID "+
			"write must be one where a cancel still reaches the process:\n%s", cmd[2])
	}
}

// TestWrapperResolvesTheDirectoryPhysically pins -P. A logical chdir
// simplifies ".." before looking at the filesystem, so with link -> /data a
// request for /work/link/.. would run in /work, and /nope/.. would succeed
// instead of failing. TestRealWorkingDirectoryIsPhysical covers the same
// property against a real shell.
func TestWrapperResolvesTheDirectoryPhysically(t *testing.T) {
	if !strings.Contains(execWrapper, "cd -P -- ") {
		t.Fatalf("wrapper does not chdir physically:\n%s", execWrapper)
	}
}

// TestWrapperAppliesTheEnvironmentWithoutAShell pins the mechanism. The shell
// that starts the wrapper imports only names it accepts, so the environment is
// re-applied by env(1) at the last step; leaving it to the shell silently drops
// MY-VAR and CONFIG.KEY under dash. TestRealEnvironmentSurvivesTheShell covers
// the property end to end.
func TestWrapperAppliesTheEnvironmentWithoutAShell(t *testing.T) {
	for _, want := range []string{
		"exec /usr/bin/env --",
		"exec /bin/env --",
		// Absolutely addressed: the request may replace PATH, which would
		// otherwise hide env and fail every execution that sets a variable.
		`[ -x /usr/bin/env ]`,
	} {
		if !strings.Contains(execWrapper, want) {
			t.Errorf("wrapper does not re-apply the environment through %q:\n%s", want, execWrapper)
		}
	}
	if strings.Contains(execWrapper, "exec env ") {
		t.Errorf("wrapper looks env up through PATH:\n%s", execWrapper)
	}
}

// TestWrapperAssignsNoShellVariables pins the names the wrapper must not use.
//
// Assigning to a name the image already exports replaces it, and the wrapper
// runs in the container's environment. An earlier version read its argument
// count into "n" and probed env into "e", which rewrote an image's own n and e
// even for a request that set no environment.
func TestWrapperAssignsNoShellVariables(t *testing.T) {
	assignment := regexp.MustCompile(`^\s*([A-Za-z_][A-Za-z0-9_]*)=`)
	for _, line := range strings.Split(execWrapper, "\n") {
		if m := assignment.FindStringSubmatch(line); m != nil {
			t.Errorf("wrapper assigns the shell variable %q, which would overwrite "+
				"an exported image variable of that name:\n%s", m[1], execWrapper)
		}
	}
}

// TestExecConfigHasNoWorkingDir pins the absence of the field. Adding it back
// would reintroduce the failure above without any behavioural test noticing,
// because the wrapper argument alone still looks correct.
func TestExecConfigHasNoWorkingDir(t *testing.T) {
	typ := reflect.TypeFor[dockerapi.ExecConfig]()
	for i := range typ.NumField() {
		tag := typ.Field(i).Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name == "WorkingDir" {
			t.Fatalf("ExecConfig declares WorkingDir; the directory must go through " +
				"the wrapper, because the runtime applies WorkingDir before the " +
				"wrapper can write its PID file")
		}
	}
}

// TestExecWithNoCwdKeepsTheArgumentShape keeps the no-cwd path explicit: the
// wrapper always receives a directory argument and an environment count, both
// empty and zero when the request sets nothing, so the shift in the script
// always lines up.
func TestExecWithNoCwdKeepsTheArgumentShape(t *testing.T) {
	f := newFakeDaemon(t)
	b := f.backend(t)
	id, err := b.Create(context.Background(), daemon.ContainerSpec{Image: "alpine:3.20", MemoryMB: 64})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := b.Exec(context.Background(), id, daemon.ExecSpec{Command: []string{"pwd"}}); err != nil {
		t.Fatalf("exec: %v", err)
	}

	cmd := f.lastExecCommand()
	pidPath, dir, env, actual, ok := unwrap(cmd)
	if !ok {
		t.Fatalf("command was not wrapped: %v", cmd)
	}
	if pidPath == "" {
		t.Error("pid path is empty")
	}
	if dir != "" {
		t.Errorf("directory argument = %q, want empty when the request sets none", dir)
	}
	if cmd[5] != "0" {
		t.Errorf("environment count = %q, want \"0\"", cmd[5])
	}
	if len(env) != 0 {
		t.Errorf("environment = %v, want none", env)
	}
	if strings.Join(actual, " ") != "pwd" {
		t.Errorf("real command = %v, want pwd", actual)
	}
}
