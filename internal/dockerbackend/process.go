package dockerbackend

import (
	"context"
	"fmt"
	"io"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
)

// process is one execution's process: a Docker exec inside the sandbox
// container.
type process struct {
	b            *Backend
	containerID  string
	execID       string
	pidPath      string
	stdinDir     string
	controlToken string
	guestPID     int

	stdout *bufferedStream
	stderr *bufferedStream
	done   chan struct{} // closed when the output stream has ended

	signalMu       sync.Mutex
	exited         bool
	pidCleaned     bool
	mu             sync.Mutex
	demuxErr       error
	managedTimeout bool
	managedKilled  bool
}

var _ daemon.Process = (*process)(nil)
var _ daemon.ManagedProcess = (*process)(nil)

// Stdout returns the process's stdout reader. It reaches EOF once the streams
// end, which Wait waits for.
func (p *process) Stdout() io.Reader { return p.stdout }

// Stderr returns the process's stderr reader.
func (p *process) Stderr() io.Reader { return p.stderr }

// Wait blocks until the output stream ends, then reads the exec's exit code,
// or -1 when the runtime reports none.
//
// The output stream ends when the process tree closes its stdout and stderr,
// which is what the engine pumps before it needs the exit status. The inspect
// poll only covers the moment between the stream closing and the daemon
// recording the exit code.
func (p *process) Wait() (int, error) {
	<-p.done
	defer p.b.cleanupStdin(p.containerID, p.stdinDir)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	inspect, err := p.b.waitInspect(ctx, p.execID)
	if err != nil {
		return -1, err
	}
	if p.controlToken != "" {
		if err := p.readManagedOutcome(ctx); err != nil {
			return -1, err
		}
	}
	p.cleanupPID()
	if inspect.ExitCode == nil {
		return -1, nil
	}
	return *inspect.ExitCode, nil
}

// Signal terminates the whole process group, including grandchildren. The
// exec's PID exists only inside the container, so the PID file written by the
// wrapper is read and the group is killed from inside.
//
// A missing PID file means the process has not started yet or has already
// been reaped; the first is retried briefly, the second is a success.
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
	// busybox kill does not accept "--", so the PID is passed as a separate
	// validated operand and the signal as the option.
	if _, err := p.b.capture(ctx, p.containerID, []string{"/bin/sh", "-c", "kill -" + name + " -" + pid}); err != nil {
		// The group may already be gone, which is the state this call wants.
		return err
	}
	return nil
}

// cleanupPID runs only after waitInspect confirms the exec stopped. The signal
// lock prevents unlinking a PID while a cancellation helper is using it.
func (p *process) cleanupPID() {
	p.signalMu.Lock()
	defer p.signalMu.Unlock()
	if p.pidCleaned {
		return
	}
	p.exited = true
	name := path.Base(p.pidPath)
	if path.Dir(p.pidPath) != PidDir || name == "." || name == ".." || name == "" {
		return
	}
	if strings.ContainsAny(name, "/\\\x00") {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for attempt := range 3 {
		if _, err := p.b.capture(ctx, p.containerID, []string{"/bin/rm", "-f", "--", p.pidPath}); err == nil {
			p.pidCleaned = true
			return
		}
		if attempt == 2 || ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// pid reads the wrapper's PID file, retrying while the exec is still starting.
//
// The read goes through an exec, not the archive API: the file lives on a
// tmpfs mount inside the container, and the archive endpoint resolves paths in
// the daemon's own view of the container filesystem, where a mount the
// container made for itself does not appear. Reading from inside asks the one
// namespace that can see it.
func (p *process) pid(limit time.Duration) (string, error) {
	deadline := time.Now().Add(limit)
	script := "cat " + p.pidPath + " 2>/dev/null"
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		data, err := p.b.capture(ctx, p.containerID, []string{"/bin/sh", "-c", script})
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
		time.Sleep(10 * time.Millisecond)
	}
}
