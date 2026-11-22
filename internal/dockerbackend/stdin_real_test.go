//go:build docker

package dockerbackend

import (
	"io"
	"os"
	"testing"

	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
)

func TestRealNonRootPrivateStdin(t *testing.T) {
	image := os.Getenv("SANDBOX_E2B_IMAGE")
	if image == "" {
		t.Skip("set SANDBOX_E2B_IMAGE to the non-root examples/e2b image")
	}
	h := newRealHarness(t)
	id, err := h.be.Create(t.Context(), daemon.ContainerSpec{Image: image, MemoryMB: 64, Labels: map[string]string{daemon.OwnerLabelKey: h.owner}})
	if err != nil {
		t.Fatal(err)
	}
	const input = "hello\x00世界\n"
	dir, err := h.be.stageStdin(t.Context(), id, input)
	if err != nil {
		t.Fatal(err)
	}
	// Inspect the actual archive destination before the wrapper unlinks it.
	const probe = `test "$(id -u)" != 0 && test "$(stat -c %u:%a "$1")" = "$(id -u):700" && cat "$1/input"`
	out, err := h.be.capture(t.Context(), id, []string{"/bin/sh", "-c", probe, "probe", dir})
	if err != nil || string(out) != input {
		t.Fatalf("stdin directory was not private, guest-owned and readable: %v", err)
	}
	h.be.cleanupStdin(id, dir)
	proc, err := h.be.Exec(t.Context(), id, daemon.ExecSpec{Command: []string{"cat"}, Stdin: input})
	if err != nil {
		t.Fatal(err)
	}
	out, err = io.ReadAll(proc.Stdout())
	if err != nil || string(out) != input {
		t.Fatalf("non-root command did not receive exact stdin: %v", err)
	}
	if code, err := proc.Wait(); code != 0 || err != nil {
		t.Fatalf("Wait = %d, %v", code, err)
	}
	p := proc.(*process)
	if _, err := h.be.capture(t.Context(), id, []string{"/bin/sh", "-c", `test ! -e "$1" && test ! -e "$2" && test ! -e "$3"`, "probe", dir, p.stdinDir, p.pidPath}); err != nil {
		t.Fatal("non-root cleanup left staging or control files:", err)
	}
}
