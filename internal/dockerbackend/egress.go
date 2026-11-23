package dockerbackend

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
	"github.com/ruhuang2001/tiny-sandbox/internal/dockerapi"
	"github.com/ruhuang2001/tiny-sandbox/internal/egress"
)

type egressState struct {
	policy *egress.Policy
	mu     sync.Mutex
	cancel map[string]context.CancelFunc
}

func (b *Backend) ConfigureEgress(policy *egress.Policy) { b.egress.policy = policy }

func proxyEnv(mode daemon.NetworkMode) []string {
	if mode != daemon.NetworkFiltered {
		return nil
	}
	return []string{"HTTPS_PROXY=http://127.0.0.1:8888", "https_proxy=http://127.0.0.1:8888", "NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1,localhost"}
}

func (b *Backend) startEgress(_ context.Context, id string, spec daemon.ContainerSpec) error {
	policy := b.egress.policy
	if policy == nil {
		return errors.New("filtered network requires administrator egress policy")
	}
	ctx, cancel := context.WithCancel(context.Background())
	b.egress.mu.Lock()
	if b.egress.cancel == nil {
		b.egress.cancel = make(map[string]context.CancelFunc)
	}
	if prev := b.egress.cancel[id]; prev != nil {
		prev()
	}
	b.egress.cancel[id] = cancel
	b.egress.mu.Unlock()
	go b.acceptEgress(ctx, id, spec.Image, policy)
	return nil
}

func (b *Backend) acceptEgress(ctx context.Context, id, image string, policy *egress.Policy) {
	slots := make(chan struct{}, 64)
	var tunnels sync.WaitGroup
	defer tunnels.Wait()
	var delay time.Duration
	for ctx.Err() == nil {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			return
		}
		stream, err := b.api.ExecAttach(ctx, id, dockerapi.ExecConfig{User: "0", Cmd: []string{"/.tiny-sandbox/guest", "proxy-accept"}}, false)
		if err == nil {
			stop := context.AfterFunc(ctx, func() { _ = stream.Close() })
			var ready [1]byte
			_, err = io.ReadFull(stream, ready[:])
			stop()
			if err == nil && ready[0] == 1 {
				delay = 0
				tunnels.Go(func() { defer func() { <-slots }(); serveEgress(ctx, stream, policy, image) })
				continue
			}
			_ = stream.Close()
		}
		<-slots
		if ctx.Err() != nil {
			return
		}
		delay = min(5*time.Second, max(100*time.Millisecond, 2*delay))
		if delay == time.Second*5 {
			slog.DebugContext(ctx, "egress relay unavailable", "container", id, "retry_in", delay.String())
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

func (b *Backend) stopEgress(id string) {
	b.egress.mu.Lock()
	defer b.egress.mu.Unlock()
	if cancel := b.egress.cancel[id]; cancel != nil {
		cancel()
		delete(b.egress.cancel, id)
	}
}

type activeTunnel struct {
	io.ReadWriteCloser
	last  atomic.Int64
	since time.Time
}

func (t *activeTunnel) Read(p []byte) (int, error) {
	n, err := t.ReadWriteCloser.Read(p)
	if n != 0 {
		t.last.Store(time.Since(t.since).Nanoseconds())
	}
	return n, err
}

func (t *activeTunnel) Write(p []byte) (int, error) {
	n, err := t.ReadWriteCloser.Write(p)
	if n != 0 {
		t.last.Store(time.Since(t.since).Nanoseconds())
	}
	return n, err
}

func serveEgress(ctx context.Context, stream io.ReadWriteCloser, policy *egress.Policy, image string) {
	tunnel := &activeTunnel{ReadWriteCloser: stream, since: time.Now()}
	session, finish := context.WithCancel(ctx)
	defer finish()
	idle := policy.TunnelIdleTimeout()
	stop := context.AfterFunc(session, func() { _ = stream.Close() })
	defer stop()
	go func() {
		timer := time.NewTimer(idle)
		defer timer.Stop()
		for {
			select {
			case <-session.Done():
				return
			case <-timer.C:
			}
			remaining := idle - (time.Since(tunnel.since) - time.Duration(tunnel.last.Load()))
			if remaining <= 0 {
				finish()
				return
			}
			timer.Reset(remaining)
		}
	}()
	policy.Serve(session, tunnel, image)
}
