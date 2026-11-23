package dockerbackend

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"time"

	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
	"github.com/ruhuang2001/tiny-sandbox/internal/guestprotocol"
)

type guestProcess struct {
	g              *guestSession
	id             string
	pid            int
	stdout, stderr *bufferedStream
	done           chan struct{}
	result         protocol.Result
	err            error
}

func (b *Backend) execGuest(ctx context.Context, g *guestSession, spec daemon.ExecSpec) (daemon.Process, error) {
	id := spec.ExecutionID
	if id == "" {
		id = rand.Text()
	}
	reader := g.reader(id)
	var hello protocol.Result
	if err := reader.call(ctx, 'H', protocol.Request{}, &hello); err != nil {
		g.closeReader(id)
		return nil, err
	}
	req := protocol.Request{ID: id, Op: "start", OutputTail: spec.OutputTail, Command: spec.Command, Env: spec.Env, Dir: spec.Dir, TimeoutMS: *spec.TimeoutMS}
	var result protocol.Result
	if err := g.call(ctx, 'C', req, &result); err != nil {
		var known *daemon.Error
		if errors.As(err, &known) {
			g.closeReader(id)
			return nil, err
		}
		if err = g.call(context.WithoutCancel(ctx), 'C', protocol.Request{ID: id, Op: "read"}, &result); err != nil {
			g.closeReader(id)
			return nil, errors.Join(daemon.ErrStartUnconfirmed, err)
		}
	}
	p := &guestProcess{g: g, id: id, pid: result.PID, stdout: newBufferedStream(), stderr: newBufferedStream(), done: make(chan struct{})}
	go p.follow()
	return p, nil
}
func (p *guestProcess) follow() {
	defer close(p.done)
	defer func() { _ = p.stdout.Close() }()
	defer func() { _ = p.stderr.Close() }()
	reader := p.g.reader(p.id)
	defer p.g.closeReader(p.id)
	req := protocol.Request{ID: p.id, Op: "read", WaitMS: 750}
	idleDelay := time.Millisecond
	for {
		var result protocol.Result
		err := reader.call(context.Background(), 'C', req, &result)
		if err != nil {
			check, stop := context.WithTimeout(context.Background(), daemon.BackendCallTimeout)
			state, livenessErr := p.g.b.Liveness(check, p.g.id)
			stop()
			if livenessErr == nil && state == daemon.LivenessPaused {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			if err = reader.call(context.Background(), 'C', req, &result); err != nil {
				p.err = errors.Join(daemon.ErrGuestLost, err)
				return
			}
		}
		outEnd := req.Offset + int64(len(result.Stdout))
		errEnd := req.ErrOffset + int64(len(result.Stderr))
		if result.Next > outEnd {
			p.stdout.resumeAt(result.Next - int64(len(result.Stdout)))
		}
		if result.ErrNext > errEnd {
			p.stderr.resumeAt(result.ErrNext - int64(len(result.Stderr)))
		}
		_, _ = p.stdout.Write(result.Stdout)
		_, _ = p.stderr.Write(result.Stderr)
		req.Offset = max(outEnd, result.Next)
		req.ErrOffset = max(errEnd, result.ErrNext)
		if result.Done && len(result.Stdout) == 0 && len(result.Stderr) == 0 {
			p.result = result
			_ = p.g.call(context.Background(), 'C', protocol.Request{ID: p.id, Op: "forget"}, &result)
			return
		}
		if len(result.Stdout) == 0 && len(result.Stderr) == 0 && !result.Done {
			time.Sleep(idleDelay)
			idleDelay = min(25*time.Millisecond, 2*idleDelay)
		} else {
			idleDelay = time.Millisecond
		}
	}
}
func (p *guestProcess) Stdout() io.Reader { return p.stdout }
func (p *guestProcess) Stderr() io.Reader { return p.stderr }
func (p *guestProcess) PID() int          { return p.pid }
func (p *guestProcess) TimedOut() bool    { <-p.done; return p.result.Reason == "timeout" }
func (p *guestProcess) Killed() bool      { <-p.done; return p.result.Reason == "canceled" }
func (p *guestProcess) Wait() (int, error) {
	<-p.done
	if p.err != nil {
		return -1, p.err
	}
	return p.result.ExitCode, nil
}
func (p *guestProcess) signal(op string) (bool, error) {
	select {
	case <-p.done:
		return false, nil
	default:
	}
	var result protocol.Result
	err := p.g.call(context.Background(), 'K', protocol.Request{ID: p.id, Op: op}, &result)
	if daemon.CodeOf(err) == daemon.CodeNotFound {
		<-p.done
		return false, p.err
	}
	return result.Signaled, err
}
func (p *guestProcess) Signal(sig daemon.Signal) error {
	op := "kill"
	if sig == daemon.SignalTerm {
		op = "term"
	}
	_, err := p.signal(op)
	return err
}
func (p *guestProcess) KillManaged() (bool, error) { return p.signal("kill") }

func (b *Backend) terminal(ctx context.Context, container string, req protocol.Request) (protocol.Result, error) {
	var result protocol.Result
	g := b.guest(container)
	if g == nil {
		return result, &daemon.Error{Code: daemon.CodeUnavailable, Message: "terminal requires an active guest agent"}
	}
	err := g.call(ctx, 'P', req, &result)
	return result, err
}
func (b *Backend) OpenTerminal(ctx context.Context, container, id string, spec daemon.ExecSpec, rows, cols uint16) error {
	_, err := b.terminal(ctx, container, protocol.Request{ID: id, Op: "start", Command: spec.Command, Env: spec.Env, Dir: spec.Dir, Rows: rows, Cols: cols})
	return err
}
func (b *Backend) TerminalInput(ctx context.Context, container, id string, data []byte) error {
	_, err := b.terminal(ctx, container, protocol.Request{ID: id, Op: "input", Data: data})
	return err
}
func (b *Backend) TerminalResize(ctx context.Context, container, id string, rows, cols uint16) error {
	_, err := b.terminal(ctx, container, protocol.Request{ID: id, Op: "resize", Rows: rows, Cols: cols})
	return err
}
func (b *Backend) ReadTerminal(ctx context.Context, container, id string, offset int64) ([]byte, int64, bool, error) {
	g := b.guest(container)
	if g == nil {
		return nil, 0, false, &daemon.Error{Code: daemon.CodeUnavailable, Message: "terminal requires guest agent"}
	}
	var r protocol.Result
	err := g.reader(id).call(ctx, 'P', protocol.Request{ID: id, Op: "read", Offset: offset, WaitMS: 750}, &r)
	return r.Stdout, r.Next, r.Done && len(r.Stdout) == 0, err
}
func (b *Backend) CloseTerminal(ctx context.Context, container, id string) error {
	_, err := b.terminal(ctx, container, protocol.Request{ID: id, Op: "close"})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, daemon.BackendCallTimeout)
	defer cancel()
	for {
		r, err := b.terminal(ctx, container, protocol.Request{ID: id, Op: "forget"})
		if err != nil || r.Done {
			if g := b.guest(container); g != nil {
				g.closeReader(id)
			}
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (b *Backend) TerminalDone(ctx context.Context, container, id string) (bool, error) {
	r, err := b.terminal(ctx, container, protocol.Request{ID: id, Op: "status"})
	return r.Done, err
}

func (b *Backend) TerminalsEnabled() bool { return b.guestMode == "auto" }
