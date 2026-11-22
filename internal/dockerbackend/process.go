package dockerbackend

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
)

type process struct {
	*execFiles
	b            *Backend
	controlToken string
	guestPID     int

	stdout *bufferedStream
	stderr *bufferedStream
	done   chan struct{}

	mu             sync.Mutex
	demuxErr       error
	managedTimeout bool
	managedKilled  bool
}

var _ daemon.Process = (*process)(nil)
var _ daemon.ManagedProcess = (*process)(nil)

func (p *process) Stdout() io.Reader { return p.stdout }

func (p *process) Stderr() io.Reader { return p.stderr }

func (p *process) Wait() (int, error) {
	<-p.done

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	inspect, err := p.b.waitInspect(ctx, p.execID)
	if err != nil {
		p.b.deferCleanup(p.execFiles)
		return -1, err
	}
	defer p.b.cleanupAfterExit(p.execFiles)
	if p.controlToken != "" {
		if err := p.readManagedOutcome(ctx); err != nil {
			return -1, err
		}
	}
	if inspect.ExitCode == nil {
		return -1, nil
	}
	return *inspect.ExitCode, nil
}

func (p *process) Signal(sig daemon.Signal) error {
	if p.controlToken != "" {
		_, err := p.signalManaged(sig)
		return err
	}
	p.signalMu.Lock()
	defer p.signalMu.Unlock()
	if p.exited {
		return nil
	}
	name := "TERM"
	if sig == daemon.SignalKill {
		name = "KILL"
	}
	pid, err := p.pid(3 * time.Second)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := p.b.captureControl(ctx, p.containerID, []string{"/bin/sh", "-c", "kill -" + name + " -" + pid}); err != nil {
		return err
	}
	return nil
}

func (p *process) pid(limit time.Duration) (string, error) {
	deadline := time.Now().Add(limit)
	script := "cat " + p.pidPath + " 2>/dev/null"
	for step := 0; ; step++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		data, err := p.b.captureControl(ctx, p.containerID, []string{"/bin/sh", "-c", script})
		cancel()
		if err == nil && len(data) > 0 {
			return parsePid(data)
		}
		if time.Now().After(deadline) {
			if err != nil {
				return "", err
			}
			return "", fmt.Errorf("dockerbackend: pid file %s stayed empty for %v", p.pidPath, limit)
		}
		time.Sleep(pollDelay(step))
	}
}
