package dockerbackend

import (
	"context"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/ruhuang2001/execlet/internal/daemon"
	"github.com/ruhuang2001/execlet/internal/dockerapi"
)

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

func TestWrapperResolvesTheDirectoryPhysically(t *testing.T) {
	if !strings.Contains(execWrapper, "cd -P -- ") {
		t.Fatalf("wrapper does not chdir physically:\n%s", execWrapper)
	}
}

func TestWrapperAppliesTheEnvironmentWithoutAShell(t *testing.T) {
	for _, want := range []string{
		"exec /usr/bin/env --",
		"exec /bin/env --",
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

func TestWrapperAssignsNoShellVariables(t *testing.T) {
	assignment := regexp.MustCompile(`^\s*([A-Za-z_][A-Za-z0-9_]*)=`)
	for _, line := range strings.Split(execWrapper, "\n") {
		if m := assignment.FindStringSubmatch(line); m != nil {
			t.Errorf("wrapper assigns the shell variable %q, which would overwrite "+
				"an exported image variable of that name:\n%s", m[1], execWrapper)
		}
	}
}

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

func TestUnwrapDoesNotMisreadALegitimateCommand(t *testing.T) {
	command := []string{"command", "echo", "hi"}
	cmd := append([]string{"/bin/sh", "-c", execWrapper, "/run/pid", "/work", "1", "0", "A=1"}, command...)

	_, dir, env, actual, ok := unwrap(cmd)
	if !ok {
		t.Fatalf("a wrapped command was not recognized: %v", cmd)
	}
	if dir != "/work" || !slices.Equal(env, []string{"A=1"}) {
		t.Errorf("dir = %q env = %v", dir, env)
	}
	if !slices.Equal(actual, command) {
		t.Errorf("command = %v, want %v: its own arguments were mistaken for "+
			"the injected separator", actual, command)
	}
}

func TestWrapCommandAddsTheSeparatorOnlyWhenNeeded(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command []string
		wantSep bool
	}{
		{"plain path", []string{"/usr/bin/env"}, false},
		{"path with equals", []string{"/tmp/tool=v1"}, true},
		{"argument with equals", []string{"env", "A=1"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			argv := wrapCommand("/run/pid", "", []string{"A=1"}, tc.command)
			_, _, _, actual, ok := unwrap(argv)
			if !ok {
				t.Fatalf("wrapped argv was not recognized: %v", argv)
			}
			if !slices.Equal(actual, tc.command) {
				t.Errorf("command = %v, want %v", actual, tc.command)
			}
			if gotSep := argv[6] == "1"; gotSep != tc.wantSep {
				t.Errorf("separator present = %v, want %v for %v", gotSep, tc.wantSep, tc.command)
			}
		})
	}
}
