package daemon

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"sync"
)

type DeploymentPolicy struct {
	MaxSandboxes int           `json:"max_sandboxes"`
	MemoryMB     int64         `json:"total_memory_mb"`
	NanoCPUs     int64         `json:"total_nano_cpus"`
	Pids         int64         `json:"total_pids"`
	Images       []string      `json:"allowed_images"`
	Networks     []NetworkMode `json:"allowed_networks"`
}

func LoadPolicy(path string) (DeploymentPolicy, error) {
	var p DeploymentPolicy
	data, err := os.ReadFile(path)
	if err != nil {
		return p, err
	}
	if err = json.Unmarshal(data, &p, json.RejectUnknownMembers(true)); err != nil {
		return p, err
	}
	return p, p.Validate()
}

func (p DeploymentPolicy) Validate() error {
	if p.MaxSandboxes <= 0 || p.MemoryMB <= 0 || p.NanoCPUs <= 0 || p.Pids <= 0 || len(p.Images) == 0 || len(p.Networks) == 0 {
		return errors.New("policy requires positive aggregate budgets, allowed_images and allowed_networks")
	}
	if p.MemoryMB > math.MaxInt64>>20 {
		return errors.New("policy memory budget exceeds supported byte range")
	}
	for _, image := range p.Images {
		if image == "" {
			return errors.New("empty allowed image")
		}
	}
	for _, network := range p.Networks {
		if network != NetworkNone && network != NetworkBridge {
			return fmt.Errorf("unsupported network policy %q; no silent fallback", network)
		}
	}
	return nil
}

type PolicyUsage struct {
	Sandboxes int   `json:"sandboxes"`
	MemoryMB  int64 `json:"memory_mb"`
	NanoCPUs  int64 `json:"nano_cpus"`
	Pids      int64 `json:"pids"`
}

type PolicyBackend struct {
	Backend
	policy      DeploymentPolicy
	mu          sync.Mutex
	allocations map[string]ContainerSpec
}

func NewPolicyBackend(backend Backend, policy DeploymentPolicy) (*PolicyBackend, error) {
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	if _, ok := backend.(interface {
		VerifyLimits(context.Context, string, ContainerSpec) error
	}); !ok {
		return nil, errors.New("backend cannot verify deployment policy")
	}
	policy.Images = slices.Clone(policy.Images)
	policy.Networks = slices.Clone(policy.Networks)
	return &PolicyBackend{Backend: backend, policy: policy, allocations: make(map[string]ContainerSpec)}, nil
}

func (b *PolicyBackend) DefaultNetwork() NetworkMode { return b.policy.Networks[0] }

func (b *PolicyBackend) DefaultNanoCPUs() int64 { return min(int64(1_000_000_000), b.policy.NanoCPUs) }

func (b *PolicyBackend) BackgroundCommands() bool {
	v, ok := b.Backend.(interface{ BackgroundCommands() bool })
	return ok && v.BackgroundCommands()
}

func (b *PolicyBackend) Ping(ctx context.Context) error {
	if v, ok := b.Backend.(interface{ Ping(context.Context) error }); ok {
		return v.Ping(ctx)
	}
	return errors.New("backend cannot establish readiness")
}

func (b *PolicyBackend) OwnedLiveness(ctx context.Context, owner string) (map[string]Liveness, error) {
	if v, ok := b.Backend.(BatchLivenessBackend); ok {
		return v.OwnedLiveness(ctx, owner)
	}
	return nil, errors.ErrUnsupported
}

func (b *PolicyBackend) usageLocked() PolicyUsage {
	u := PolicyUsage{}
	for _, a := range b.allocations {
		u.Sandboxes++
		u.MemoryMB += int64(a.MemoryMB)
		u.NanoCPUs += a.NanoCPUs
		u.Pids += int64(a.PidsLimit)
	}
	return u
}

func (b *PolicyBackend) Usage() PolicyUsage { b.mu.Lock(); defer b.mu.Unlock(); return b.usageLocked() }

func (b *PolicyBackend) Create(ctx context.Context, spec ContainerSpec) (string, error) {
	if spec.Name == "" || !slices.Contains(b.policy.Images, spec.Image) || !slices.Contains(b.policy.Networks, spec.Network) || spec.NanoCPUs <= 0 || spec.MemoryMB < MinMemoryMB || int64(spec.MemoryMB) > math.MaxInt64>>20 || spec.PidsLimit < MinPidsLimit {
		return "", errorf(CodeInvalidRequest, "deployment policy rejects image, network or unlimited CPU")
	}
	b.mu.Lock()
	u := b.usageLocked()
	if _, exists := b.allocations[spec.Name]; exists || u.Sandboxes >= b.policy.MaxSandboxes || int64(spec.MemoryMB) > b.policy.MemoryMB-u.MemoryMB || spec.NanoCPUs > b.policy.NanoCPUs-u.NanoCPUs || int64(spec.PidsLimit) > b.policy.Pids-u.Pids {
		b.mu.Unlock()
		return "", errorf(CodeRateLimited, "deployment resource budget exhausted")
	}
	b.allocations[spec.Name] = spec
	b.mu.Unlock()
	id, err := b.Backend.Create(ctx, spec)
	if id != "" {
		b.mu.Lock()
		delete(b.allocations, spec.Name)
		b.allocations[id] = spec
		b.mu.Unlock()
	}
	if err == nil {
		err = b.Backend.(interface {
			VerifyLimits(context.Context, string, ContainerSpec) error
		}).VerifyLimits(ctx, id, spec)
	}
	if err != nil {
		target := id
		if target == "" {
			target = spec.Name
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), BackendCallTimeout)
		defer cancel()
		if removeErr := b.Remove(cleanup, target); removeErr != nil && !errors.Is(removeErr, ErrNotFound) {
			return target, errorf(CodeBackendError, "policy startup failed; removal unconfirmed and reservation retained")
		}
		return "", backendError(err)
	}
	return id, nil
}

func (b *PolicyBackend) Remove(ctx context.Context, id string) error {
	err := b.Backend.Remove(ctx, id)
	if err == nil || errors.Is(err, ErrNotFound) {
		b.mu.Lock()
		delete(b.allocations, id)
		b.mu.Unlock()
	}
	return err
}

func (b *PolicyBackend) Adopt(ctx context.Context, id string, spec ContainerSpec) error {
	if !slices.Contains(b.policy.Images, spec.Image) || !slices.Contains(b.policy.Networks, spec.Network) || spec.NanoCPUs <= 0 || spec.MemoryMB < MinMemoryMB || spec.PidsLimit < MinPidsLimit {
		return errors.New("recovered container violates deployment policy")
	}
	if err := b.Backend.(interface {
		VerifyLimits(context.Context, string, ContainerSpec) error
	}).VerifyLimits(ctx, id, spec); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	u := b.usageLocked()
	if _, exists := b.allocations[id]; exists {
		return errors.New("duplicate recovered container")
	}
	if u.Sandboxes >= b.policy.MaxSandboxes || int64(spec.MemoryMB) > b.policy.MemoryMB-u.MemoryMB || spec.NanoCPUs > b.policy.NanoCPUs-u.NanoCPUs || int64(spec.PidsLimit) > b.policy.Pids-u.Pids {
		return errors.New("recovered containers exceed deployment budget")
	}
	b.allocations[id] = spec
	return nil
}
