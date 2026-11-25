//go:build e2b_background_acceptance

package dockerbackend

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ruhuang2001/execlet/internal/daemon"
)

func TestBackgroundSignalLifetime(t *testing.T) {
	for _, reuse := range []bool{false, true} {
		name := "active_control"
		if reuse {
			name = "reuse_at_dispatch"
		}
		t.Run(name, func(t *testing.T) {
			f := newFakeDaemon(t)
			b := f.backend(t)
			id, err := b.Create(t.Context(), daemon.ContainerSpec{Image: "fixture", MemoryMB: 64})
			if err != nil {
				t.Fatal(err)
			}
			var incarnation, signaled atomic.Int64
			incarnation.Store(1)
			dispatch, release := make(chan struct{}), make(chan struct{})
			f.setProgram(func(_ string, cmd []string) (string, string, int) {
				if len(cmd) == 2 && cmd[0] == "cat" && strings.HasPrefix(cmd[1], PidDir+"/") {
					return `{"pid":4242}`, "", 0
				}
				if len(cmd) == 8 && cmd[0] == "python3" && cmd[3] == supervisor && cmd[4] == "start" {
					return "", "", 0
				}
				if (len(cmd) == 5 && cmd[3] == "stdin-stage") || (len(cmd) >= 4 && cmd[0] == "/bin/rm") {
					return "", "", 0
				}
				managed := len(cmd) == 8 && cmd[0] == "python3" && cmd[3] == supervisor && cmd[4] == "signal"
				numeric := len(cmd) == 3 && cmd[0] == "/bin/sh" && cmd[2] == "kill -KILL -4242"
				if managed || numeric {
					close(dispatch)
					select {
					case <-release:
						if managed && incarnation.Load() != 1 {
							return "", "retired control channel", 1
						}
						signaled.Store(incarnation.Load())
						return `{"signaled":true}`, "", 0
					case <-t.Context().Done():
						return "", "fixture canceled", 1
					}
				}
				t.Errorf("unmodeled runtime helper; update the boundary fixture, do not count as pass: %q", cmd)
				return "", "unmodeled helper", 1
			})
			proc, err := b.Exec(t.Context(), id, daemon.ExecSpec{Command: []string{"fixture-command"}, TimeoutMS: new(int64(0))})
			if err != nil {
				t.Fatal(err)
			}
			finished := make(chan error, 1)
			go func() { finished <- proc.Signal(daemon.SignalKill) }()
			select {
			case <-dispatch:
				t.Log("signal-boundary-reached")
			case err := <-finished:
				t.Fatalf("no dispatch boundary was exercised: %v", err)
			case <-time.After(8 * time.Second):
				close(release)
				t.Fatal("dispatch watchdog expired")
			}
			if reuse {
				incarnation.Store(2)
			}
			close(release)
			err = <-finished
			if reuse {
				if got := signaled.Load(); got != 0 || err == nil {
					t.Fatalf("stale process signaled replacement incarnation %d (signal error: %v)", got, err)
				}
			} else if err != nil || signaled.Load() != 1 {
				t.Fatalf("active control was not signaled: incarnation=%d error=%v", signaled.Load(), err)
			}
		})
	}
}
