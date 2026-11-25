package dockerbackend

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ruhuang2001/execlet/internal/dockerapi"
)

func TestOOMHelperCalls(t *testing.T) {
	f := newFakeDaemon(t)
	b := f.backend(t)
	b.cgroupRoot = t.TempDir()
	id := strings.Repeat("a123", 16)
	f.containers[id] = &fakeContainer{id: id, state: "running"}
	write := func(dir, events string) {
		t.Helper()
		p := filepath.Join(b.cgroupRoot, dir)
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "memory.events"), []byte(events), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	check := func(count int64, helpers int) {
		t.Helper()
		got, err := b.OOMKillCount(t.Context(), id)
		if got != count || err != nil || len(f.execCommands) != helpers {
			t.Fatalf("OOMKillCount = %d, %v, total helper execs = %d; want %d, nil, %d", got, err, len(f.execCommands), count, helpers)
		}
	}
	write("docker/"+id[:12], "oom_kill 99\n")
	write("docker", "oom_kill 100\n")
	write("docker/"+id, "oom_group_kill 4\n")
	check(5, 1)
	if f.execCommands[0][2] != dockerapi.OOMScript {
		t.Fatal("missing portable cgroup helper")
	}
	write("docker/"+id, "oom_kill 0\n")
	check(0, 1)
	write("docker/"+id, "oom_kill 2\n")
	check(2, 1)
	if err := os.RemoveAll(filepath.Join(b.cgroupRoot, "docker", id)); err != nil {
		t.Fatal(err)
	}
	write("system.slice/docker-"+id+".scope", "oom_kill 3\n")
	check(3, 1)
	for _, badID := range []string{id[:12], strings.ToUpper(id), id[:61] + "/..", "docker", ""} {
		if n, ok := localOOMKillCount(b.cgroupRoot, badID); ok {
			t.Errorf("non-exact container ID %q returned host counter %d", badID, n)
		}
	}
}
