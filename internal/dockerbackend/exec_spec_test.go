package dockerbackend

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
	"github.com/ruhuang2001/tiny-sandbox/internal/dockerapi"
)

// TestExecPassesEnvironmentAlong proves the environment reaches the runtime.
// The value is the whole feature: an exec that drops Env runs the command with
// no variables at all while every unit above it still passes.
func TestExecPassesEnvironmentAlong(t *testing.T) {
	f := newFakeDaemon(t)
	b := f.backend(t)
	id, err := b.Create(context.Background(), daemon.ContainerSpec{Image: "alpine:3.20", MemoryMB: 64})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := b.Exec(context.Background(), id, daemon.ExecSpec{
		Command: []string{"true"},
		Env:     []string{"A=1", "B=two"},
	}); err != nil {
		t.Fatalf("exec: %v", err)
	}

	if got := f.lastExecEnv(); strings.Join(got, ",") != "A=1,B=two" {
		t.Fatalf("exec env = %v, want the request's environment", got)
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
	if len(cmd) < 6 {
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
	chdir := strings.Index(cmd[2], "cd --")
	if pidWrite < 0 || chdir < 0 {
		t.Fatalf("wrapper lost the PID write or the chdir: %q", cmd[2])
	}
	if pidWrite > chdir {
		t.Errorf("wrapper chdirs before writing the PID; every step after the PID "+
			"write must be one where a cancel still reaches the process:\n%s", cmd[2])
	}
}

// TestExecConfigHasNoWorkingDir pins the absence of the field. Adding it back
// would reintroduce the failure above without any behavioural test noticing,
// because the wrapper argument alone still looks correct.
func TestExecConfigHasNoWorkingDir(t *testing.T) {
	typ := reflect.TypeOf(dockerapi.ExecConfig{})
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
// wrapper always receives a directory argument, empty when the request sets
// none, so the shift in the script always lines up.
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
	pidPath, actual, ok := unwrap(cmd)
	if !ok {
		t.Fatalf("command was not wrapped: %v", cmd)
	}
	if pidPath == "" {
		t.Error("pid path is empty")
	}
	if cmd[4] != "" {
		t.Errorf("directory argument = %q, want empty when the request sets none", cmd[4])
	}
	if strings.Join(actual, " ") != "pwd" {
		t.Errorf("real command = %v, want pwd", actual)
	}
}
