package dockerbackend

import (
	"context"
	"errors"
	"io"
	"sync"
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
	for range 2 {
		go func() {
			for ctx.Err() == nil {
				stream, err := b.api.ExecAttach(ctx, id, dockerapi.ExecConfig{User: "0", Cmd: []string{"/.tiny-sandbox/guest", "proxy-accept"}}, false)
				if err == nil {
					serveEgress(ctx, stream, policy, spec.Image)
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(100 * time.Millisecond):
				}
			}
		}()
	}
	return nil
}

func (b *Backend) stopEgress(id string) {
	b.egress.mu.Lock()
	defer b.egress.mu.Unlock()
	if cancel := b.egress.cancel[id]; cancel != nil {
		cancel()
		delete(b.egress.cancel, id)
	}
}

func serveEgress(ctx context.Context, stream io.ReadWriteCloser, policy *egress.Policy, image string) {
	defer func() { _ = stream.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = stream.Close() })
	defer stop()
	var ready [1]byte
	if _, err := io.ReadFull(stream, ready[:]); err != nil || ready[0] != 1 {
		return
	}
	session, finish := context.WithTimeout(ctx, 2*time.Minute)
	defer finish()
	interrupt := context.AfterFunc(session, func() { _ = stream.Close() })
	defer interrupt()
	policy.Serve(session, stream, image)
}
