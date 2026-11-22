package dockerbackend

import (
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
)

func TestFailedCleanupRetriesWithoutAnotherWait(t *testing.T) {
	f := newFakeDaemon(t)
	b := f.backend(t)
	id, err := b.Create(t.Context(), daemon.ContainerSpec{Image: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	var attempts atomic.Int32
	f.setProgram(func(_ string, cmd []string) (string, string, int) {
		if len(cmd) == 6 && cmd[2] == cleanupStagedScript && attempts.Add(1) == 1 {
			return "", "rm failed", 1
		}
		return "", "", 0
	})
	const secret = "not-in-any-argv"
	proc, err := b.Exec(t.Context(), id, daemon.ExecSpec{Command: []string{"cat"}, Stdin: secret})
	if err != nil {
		t.Fatal(err)
	}
	if code, err := proc.Wait(); code != 0 || err != nil {
		t.Fatalf("Wait = %d, %v", code, err)
	}
	p := proc.(*process)
	f.mu.Lock()
	delete(f.execs, p.execID)
	cmds := slices.Clone(f.execCommands)
	f.mu.Unlock()
	if len(cmds) != 3 || cmds[0][3] != "stdin-stage" || cmds[1][3] != p.pidPath || !slices.Equal(cmds[2], []string{
		"/bin/sh", "-c", cleanupStagedScript, "cleanup", p.stdinDir, p.pidPath,
	}) {
		t.Fatalf("expected one stage exec, command, one combined rm: %q", cmds)
	}
	for _, cmd := range cmds {
		if strings.Contains(strings.Join(cmd, " "), secret) {
			t.Fatal("stdin appeared in a Docker exec argv")
		}
	}
	waitFor(t, "second rm and idle worker", func() bool {
		b.cleanup.mu.Lock()
		defer b.cleanup.mu.Unlock()
		return attempts.Load() == 2 && b.cleanup.items.Len() == 0 && !b.cleanup.running
	})
}

func TestMissingExecNotProofOfExit(t *testing.T) {
	f := newFakeDaemon(t)
	b := f.backend(t)
	id, err := b.Create(t.Context(), daemon.ContainerSpec{Image: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	files := &execFiles{containerID: id, execID: "missing", pidPath: b.nextPidPath()}
	if b.retryFiles(files) || f.nextExec != 0 {
		t.Fatal("missing exec in existing container discarded or unlinked control files")
	}
	if time.Until(files.missingRetryAt) <= 0 || b.retryFiles(files) {
		t.Fatal("no grace period after a missing exec")
	}
	files.missingRetryAt = time.Now().Add(-time.Second)
	if !b.retryFiles(files) || files.exited || files.cleaned || f.nextExec != 0 {
		t.Fatal("persistent missing exec must leave files to container removal without further polls")
	}
}

func TestCleanupBacklogSurvivesUntilExactContainerRemoval(t *testing.T) {
	f := newFakeDaemon(t)
	b := f.backend(t)
	for _, owner := range []string{"one", "two"} {
		id, err := b.Create(t.Context(), daemon.ContainerSpec{Image: "fixture", Labels: map[string]string{daemon.OwnerLabelKey: owner}})
		if err != nil {
			t.Fatal(err)
		}
		for i := range 300 {
			b.deferCleanup(&execFiles{containerID: id, execID: fmt.Sprint(i), pidPath: b.nextPidPath()})
		}
	}
	for i, owner := range []string{"one", "two"} {
		want := (2 - i) * 300
		b.cleanup.mu.Lock()
		count := b.cleanup.items.Len()
		b.cleanup.mu.Unlock()
		if count != want {
			t.Fatalf("backlog before removing %s = %d, want %d", owner, count, want)
		}
		if count, err := b.RemoveOwned(t.Context(), daemon.OwnerLabelKey, owner); count != 1 || err != nil {
			t.Fatalf("remove %s = %d, %v", owner, count, err)
		}
	}
	waitFor(t, "empty queue and idle worker", func() bool {
		b.cleanup.mu.Lock()
		defer b.cleanup.mu.Unlock()
		return b.cleanup.items.Len() == 0 && !b.cleanup.running
	})
}
