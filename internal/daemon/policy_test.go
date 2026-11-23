package daemon_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
	"github.com/ruhuang2001/tiny-sandbox/internal/daemon/membackend"
)

type policyRuntime struct {
	*membackend.Backend
	verification error
	removal      error
}

func (b *policyRuntime) VerifyLimits(context.Context, string, daemon.ContainerSpec) error {
	return b.verification
}
func (b *policyRuntime) Remove(ctx context.Context, id string) error {
	if b.removal != nil {
		return b.removal
	}
	return b.Backend.Remove(ctx, id)
}

type stalledPrewarm struct {
	*policyRuntime
	stalled string
	started chan struct{}
}

func (b *stalledPrewarm) Create(ctx context.Context, spec daemon.ContainerSpec) (string, error) {
	if spec.Image == b.stalled {
		select {
		case b.started <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return "", ctx.Err()
	}
	return b.Backend.Create(ctx, spec)
}

func TestStalledPrewarmReleasesReservation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bad, good := "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64)
		be := &stalledPrewarm{&policyRuntime{Backend: membackend.New()}, bad, make(chan struct{}, 1)}
		policy := daemon.DeploymentPolicy{MaxSandboxes: 2, MemoryMB: 1024, NanoCPUs: 2_000_000_000, Pids: 4096,
			Images: []string{bad, good}, Networks: []daemon.NetworkMode{daemon.NetworkNone},
			Prewarm: []daemon.PrewarmTarget{{Image: bad, Count: 1}, {Image: good, Count: 1}}}
		limited, err := daemon.NewPolicyBackend(be, policy)
		wantNoError(t, err)
		ctx, stop := context.WithCancel(t.Context())
		defer stop()
		go daemon.New(limited).Run(ctx)
		<-be.started
		time.Sleep(2*time.Minute + time.Millisecond)
		synctest.Wait()
		if got := limited.Usage().Sandboxes; got != 1 {
			t.Fatalf("expected only healthy standby, got %d reservations", got)
		}
		stop()
		synctest.Wait()
		if got := limited.Usage().Sandboxes; got != 0 {
			t.Fatalf("shutdown retained %d reservations", got)
		}
	})
}

func TestDeploymentPolicyAdmissionAndCleanup(t *testing.T) {
	backend := &policyRuntime{Backend: membackend.New()}
	policy := daemon.DeploymentPolicy{MaxSandboxes: 3, MemoryMB: 96, NanoCPUs: 1_000_000_000, Pids: 192, Images: []string{"approved"}, Networks: []daemon.NetworkMode{daemon.NetworkNone}}
	limited, err := daemon.NewPolicyBackend(backend, policy)
	if err != nil {
		t.Fatal(err)
	}
	spec := daemon.ContainerSpec{Name: "one", Image: "approved", MemoryMB: 48, NanoCPUs: 500_000_000, PidsLimit: 64, Network: daemon.NetworkNone}
	for _, bad := range []daemon.ContainerSpec{
		{Name: "bad", Image: "unapproved", MemoryMB: 48, NanoCPUs: 500_000_000, PidsLimit: 64, Network: daemon.NetworkNone},
		{Name: "bad", Image: "approved", MemoryMB: 48, NanoCPUs: 500_000_000, PidsLimit: 64, Network: daemon.NetworkBridge},
		{Name: "bad", Image: "approved", MemoryMB: -1, NanoCPUs: 500_000_000, PidsLimit: 64, Network: daemon.NetworkNone},
	} {
		if _, err := limited.Create(t.Context(), bad); daemon.CodeOf(err) != daemon.CodeInvalidRequest {
			t.Fatalf("bad policy admission: %v", err)
		}
	}
	var wg sync.WaitGroup
	ids := make(chan string, 8)
	for n := range 8 {
		wg.Go(func() {
			request := spec
			request.Name = fmt.Sprint(n)
			id, err := limited.Create(t.Context(), request)
			if err == nil {
				ids <- id
			} else if daemon.CodeOf(err) != daemon.CodeRateLimited {
				t.Errorf("admission: %v", err)
			}
		})
	}
	wg.Wait()
	close(ids)
	if len(ids) != 2 || limited.Usage().MemoryMB != 96 {
		t.Fatalf("oversubscription: %d, %+v", len(ids), limited.Usage())
	}
	for id := range ids {
		if err := limited.Remove(t.Context(), id); err != nil {
			t.Fatal(err)
		}
	}
	backend.verification = errors.New("kernel cannot enforce")
	backend.removal = errors.New("runtime unavailable")
	id, err := limited.Create(t.Context(), spec)
	if err == nil || id == "" || limited.Usage().Sandboxes != 1 {
		t.Fatalf("uncertain cleanup released reservation: %q %v %+v", id, err, limited.Usage())
	}
	backend.removal = nil
	if err := limited.Remove(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if limited.Usage().Sandboxes != 0 {
		t.Fatal("confirmed removal did not release capacity")
	}
}

func TestPolicySweepWithoutBatchLiveness(t *testing.T) {
	backend := &policyRuntime{Backend: membackend.New()}
	policy := daemon.DeploymentPolicy{MaxSandboxes: 1, MemoryMB: 128, NanoCPUs: 1_000_000_000, Pids: 1024, Images: []string{"approved"}, Networks: []daemon.NetworkMode{daemon.NetworkNone}}
	limited, err := daemon.NewPolicyBackend(backend, policy)
	wantNoError(t, err)
	s := daemon.New(limited, daemon.WithOwnerLabel("test-owner"))
	sb, err := s.Create(t.Context(), daemon.CreateRequest{Image: new("approved"), MemoryMB: new(64)})
	wantNoError(t, err)
	backend.Disappear(s.ContainerIDForTest(sb.ID))
	s.EvictSweep(t.Context())
	got, err := s.Get(sb.ID)
	wantNoError(t, err)
	if got.State != daemon.StateKilled || got.KillReason == nil || *got.KillReason != daemon.KillEvicted {
		t.Fatalf("sweep left missing container in state %s (%v)", got.State, got.KillReason)
	}
}

func TestDeploymentPolicyRejectsUnknownConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	for _, data := range []string{`{"max_sandboxes":1,"total_memory_mb":64,"total_nano_cpus":1000000000,"total_pids":64,"allowed_images":["approved"],"allowed_networks":["filtered"]}`, `{"max_sandboxes":1,"total_memory_mb":64,"total_nano_cpus":1000000000,"total_pids":64,"allowed_images":["approved"],"allowed_networks":["none"],"allow_private":false}`} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := daemon.LoadPolicy(path); err == nil {
			t.Fatal("unsupported policy accepted")
		}
	}
}
