package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

type PrewarmTarget struct {
	Image     string      `json:"image"`
	Count     int         `json:"count"`
	MemoryMB  int         `json:"memory_mb,omitempty"`
	PidsLimit int         `json:"pids_limit,omitempty"`
	NanoCPUs  int64       `json:"nano_cpus,omitempty"`
	Network   NetworkMode `json:"network,omitempty"`
	Runtime   string      `json:"runtime,omitempty"`
}

type prewarmKey struct {
	image               string
	memoryMB, pidsLimit int
	nanoCPUs            int64
	network             NetworkMode
	runtime             string
}

type containerRenamer interface {
	Rename(context.Context, string, string) error
}

func keyOf(spec ContainerSpec) prewarmKey {
	return prewarmKey{spec.Image, spec.MemoryMB, spec.PidsLimit, spec.NanoCPUs, spec.Network, spec.Runtime}
}

func (p DeploymentPolicy) prewarmTargets() (map[prewarmKey]int, error) {
	targets := map[prewarmKey]int{}
	budget := PolicyUsage{Sandboxes: min(p.MaxSandboxes, MaxSandboxes), MemoryMB: p.MemoryMB, NanoCPUs: p.NanoCPUs, Pids: p.Pids}
	for _, t := range p.Prewarm {
		image, err := p.ResolveImage(t.Image)
		if err != nil {
			return nil, err
		}
		if !templateDigest.MatchString(image) {
			return nil, fmt.Errorf("prewarm %q requires an immutable image digest", t.Image)
		}
		runtime := t.Runtime
		if len(p.Runtimes) > 0 {
			runtime = cmp.Or(runtime, p.Runtimes[0])
		}
		k := prewarmKey{image, cmp.Or(t.MemoryMB, DefaultMemoryMB), cmp.Or(t.PidsLimit, DefaultPidsLimit), cmp.Or(t.NanoCPUs, min(int64(1_000_000_000), p.NanoCPUs)), cmp.Or(t.Network, p.Networks[0]), runtime}
		if !p.allowsImage(image) || !slices.Contains(p.Networks, k.network) || (k.runtime != "" && !slices.Contains(p.Runtimes, k.runtime)) || k.memoryMB < MinMemoryMB || k.pidsLimit < MinPidsLimit || k.nanoCPUs <= 0 {
			return nil, fmt.Errorf("prewarm %q violates deployment limits", t.Image)
		}
		if t.Count < 1 || t.Count > budget.Sandboxes || int64(k.memoryMB) > budget.MemoryMB/int64(t.Count) || int64(k.pidsLimit) > budget.Pids/int64(t.Count) || k.nanoCPUs > budget.NanoCPUs/int64(t.Count) {
			return nil, errors.New("prewarm targets exceed deployment budget")
		}
		budget.Sandboxes -= t.Count
		budget.MemoryMB -= int64(t.Count) * int64(k.memoryMB)
		budget.Pids -= int64(t.Count) * int64(k.pidsLimit)
		budget.NanoCPUs -= int64(t.Count) * k.nanoCPUs
		targets[k] += t.Count
	}
	return targets, nil
}

func (b *PolicyBackend) takeReady(k prewarmKey) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	ids := b.ready[k]
	if len(ids) == 0 {
		return ""
	}
	id := ids[len(ids)-1]
	b.ready[k] = ids[:len(ids)-1]
	return id
}

func (b *PolicyBackend) readyIDs(k prewarmKey) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.ready[k])
}

func (b *PolicyBackend) discardReady(ctx context.Context, k prewarmKey, id string, retry func(string)) {
	b.mu.Lock()
	i := slices.Index(b.ready[k], id)
	if i < 0 {
		b.mu.Unlock()
		return
	}
	b.ready[k] = slices.Delete(b.ready[k], i, i+1)
	b.mu.Unlock()
	if err := b.Remove(ctx, id); err != nil && !errors.Is(err, ErrNotFound) {
		retry(id)
	}
}

func (b *PolicyBackend) maintainPrewarm(ctx context.Context, owner string, retry func(string)) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	defer func() {
		for key := range b.targets {
			for _, id := range b.readyIDs(key) {
				cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), BackendCallTimeout)
				b.discardReady(cleanup, key, id, retry)
				cancel()
			}
		}
	}()
	for {
		for key, count := range b.targets {
			if !b.policy.allowsImage(key.image) {
				for _, id := range b.readyIDs(key) {
					b.discardReady(ctx, key, id, retry)
				}
				continue
			}
			for _, id := range b.readyIDs(key) {
				probe, cancel := backendCtx(ctx)
				live, err := b.Liveness(probe, id)
				cancel()
				if err == nil && live != LivenessRunning {
					b.discardReady(ctx, key, id, retry)
				}
			}
			for range count - len(b.readyIDs(key)) {
				if ctx.Err() != nil {
					return
				}
				spec := ContainerSpec{Image: key.image, MemoryMB: key.memoryMB, PidsLimit: key.pidsLimit, NanoCPUs: key.nanoCPUs, Network: key.network, Runtime: key.runtime, Name: newID("tiny-sandbox-prewarm-"), Labels: map[string]string{OwnerLabelKey: owner}}
				id, err := b.createFresh(ctx, spec)
				if err != nil {
					if id != "" {
						retry(id)
					}
					break
				}
				b.mu.Lock()
				b.ready[key] = append(b.ready[key], id)
				b.mu.Unlock()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
