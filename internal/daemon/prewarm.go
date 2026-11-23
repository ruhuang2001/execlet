package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
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

func keyOf(spec ContainerSpec) prewarmKey {
	return prewarmKey{spec.Image, spec.MemoryMB, spec.PidsLimit, spec.NanoCPUs, spec.Network, spec.Runtime}
}

func (p DeploymentPolicy) prewarmTargets() (map[prewarmKey]int, error) {
	targets := map[prewarmKey]int{}
	budget := PolicyUsage{Sandboxes: min(p.MaxSandboxes, MaxSandboxes), MemoryMB: p.MemoryMB, NanoCPUs: p.NanoCPUs, Pids: p.Pids}
	for _, t := range p.Prewarm {
		name, alias := strings.CutPrefix(t.Image, "template:")
		if alias && !templateName.MatchString(name) || !alias && !templateDigest.MatchString(t.Image) {
			return nil, fmt.Errorf("prewarm %q requires a template name or immutable image digest", t.Image)
		}
		runtime := t.Runtime
		if len(p.Runtimes) > 0 {
			runtime = cmp.Or(runtime, p.Runtimes[0])
		}
		k := prewarmKey{t.Image, cmp.Or(t.MemoryMB, DefaultMemoryMB), cmp.Or(t.PidsLimit, DefaultPidsLimit), cmp.Or(t.NanoCPUs, min(int64(1_000_000_000), p.NanoCPUs)), cmp.Or(t.Network, p.Networks[0]), runtime}
		if (!alias && !p.allowsImage(t.Image)) || !slices.Contains(p.Networks, k.network) || (k.runtime != "" && !slices.Contains(p.Runtimes, k.runtime)) || k.memoryMB < MinMemoryMB || k.pidsLimit < MinPidsLimit || k.nanoCPUs <= 0 {
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

func (b *PolicyBackend) takeReady(k prewarmKey) (string, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ids := b.ready[k]
	if len(ids) == 0 {
		return "", ""
	}
	id := ids[len(ids)-1]
	b.ready[k] = ids[:len(ids)-1]
	if len(b.ready[k]) == 0 {
		delete(b.ready, k)
	}
	return id, b.allocations[id].Labels[SandboxIDLabelKey]
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
	if len(b.ready[k]) == 0 {
		delete(b.ready, k)
	}
	b.mu.Unlock()
	cleanup, cancel := backendCtx(ctx)
	defer cancel()
	if err := b.Remove(cleanup, id); err != nil && !errors.Is(err, ErrNotFound) {
		retry(id)
	}
}

func (b *PolicyBackend) desiredPrewarm() map[prewarmKey]int {
	p := b.policy
	catalog, err := p.currentTemplates()
	if err != nil {
		catalog = nil
	}
	p.catalogPath = ""
	p.Templates = catalog
	targets := make(map[prewarmKey]int, len(b.targets))
	for key, count := range b.targets {
		image, err := p.ResolveImage(key.image)
		if err != nil || !p.allowsImage(image) {
			continue
		}
		key.image = image
		targets[key] += count
	}
	return targets
}

func (b *PolicyBackend) trimPrewarm(ctx context.Context, targets map[prewarmKey]int, retry func(string)) {
	b.mu.Lock()
	keys := slices.Collect(maps.Keys(b.ready))
	b.mu.Unlock()
	for _, key := range keys {
		ids := b.readyIDs(key)
		for _, id := range ids[min(len(ids), targets[key]):] {
			b.discardReady(ctx, key, id, retry)
		}
	}
}

func (b *PolicyBackend) maintainPrewarm(ctx context.Context, owner string, retry func(string)) {
	type failure struct {
		delay time.Duration
		next  time.Time
	}
	failures := map[prewarmKey]failure{}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	defer func() {
		cleanup, cancel := backendCtx(context.WithoutCancel(ctx))
		defer cancel()
		b.trimPrewarm(cleanup, nil, retry)
	}()
	for {
		targets := b.desiredPrewarm()
		b.trimPrewarm(ctx, targets, retry)
		maps.DeleteFunc(failures, func(k prewarmKey, _ failure) bool { return targets[k] == 0 })
		for key, count := range targets {
			for _, id := range b.readyIDs(key) {
				probe, cancel := backendCtx(ctx)
				live, err := b.Liveness(probe, id)
				cancel()
				if err == nil && live != LivenessRunning {
					b.discardReady(ctx, key, id, retry)
				}
			}
			if time.Now().Before(failures[key].next) {
				continue
			}
			for range count - len(b.readyIDs(key)) {
				if ctx.Err() != nil {
					return
				}
				sandboxID := newID("sbx_")
				spec := ContainerSpec{Image: key.image, MemoryMB: key.memoryMB, PidsLimit: key.pidsLimit, NanoCPUs: key.nanoCPUs, Network: key.network, Runtime: key.runtime, Name: "tiny-sandbox-" + sandboxID, Labels: map[string]string{OwnerLabelKey: owner, SandboxIDLabelKey: sandboxID}}
				create, cancel := context.WithTimeout(ctx, 2*time.Minute)
				id, err := b.createFresh(create, spec)
				cancel()
				if err != nil {
					if ctx.Err() == nil {
						delay := min(time.Minute, max(2*time.Second, 2*failures[key].delay))
						failures[key] = failure{delay, time.Now().Add(delay)}
						slog.WarnContext(ctx, "prewarm create deferred", "image", key.image, "runtime", key.runtime, "error_code", CodeOf(err), "retry_in", delay.String())
					}
					if id != "" {
						retry(id)
					}
					break
				}
				delete(failures, key)
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
