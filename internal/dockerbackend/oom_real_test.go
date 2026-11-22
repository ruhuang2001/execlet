//go:build docker && linux

package dockerbackend

import (
	"testing"

	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
)

func TestRealLocalOOMCounter(t *testing.T) {
	h := newRealHarness(t)
	b, err := Dial(t.Context(), dockerSocket(t))
	if err != nil {
		t.Fatal(err)
	}
	id, err := b.Create(t.Context(), daemon.ContainerSpec{
		Image: "python:3.12-alpine", MemoryMB: 64, Labels: map[string]string{daemon.OwnerLabelKey: h.owner},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := localOOMKillCount(b.cgroupRoot, id); !ok {
		t.Skip("exact container cgroup not visible in a supported host layout")
	}
	sample := func() int64 {
		t.Helper()
		if err := b.Pause(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		n, err := b.OOMKillCount(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if err := b.Unpause(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := sample()
	p, err := b.Exec(t.Context(), id, daemon.ExecSpec{Command: []string{"python3", "-c", "x=bytearray(256*1024*1024)"}})
	if err != nil {
		t.Fatal(err)
	}
	if code, err := p.Wait(); code != 137 || err != nil {
		t.Fatalf("Wait = %d, %v", code, err)
	}
	after := sample()
	if after <= before {
		t.Fatalf("paused samples did not see kill: %d -> %d", before, after)
	}
	t.Logf("oom_kill %d -> %d with exec unavailable during both samples", before, after)
}
