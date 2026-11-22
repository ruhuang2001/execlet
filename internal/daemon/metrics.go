package daemon

import (
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

type metrics struct {
	mu              sync.Mutex
	timings         map[string]*timing
	cleanupFailures uint64
}
type timing struct {
	count   uint64
	sum     float64
	buckets [8]uint64
}

var latencyBounds = [...]float64{.01, .05, .1, .5, 1, 5, 30, 120}

func (m *metrics) observe(series string, start time.Time) {
	value := time.Since(start).Seconds()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.timings == nil {
		m.timings = map[string]*timing{}
	}
	row := m.timings[series]
	if row == nil {
		row = &timing{}
		m.timings[series] = row
	}
	row.count++
	row.sum += value
	for i, bound := range latencyBounds {
		if value <= bound {
			row.buckets[i]++
		}
	}
}
func (m *metrics) cleanupFailed() { m.mu.Lock(); m.cleanupFailures++; m.mu.Unlock() }
func (h *Handler) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	m := &h.store.metrics
	var out strings.Builder
	m.mu.Lock()
	previous := ""
	for _, key := range slices.Sorted(maps.Keys(m.timings)) {
		name, labels, _ := strings.Cut(key, "{")
		if name != previous {
			fmt.Fprintf(&out, "# HELP %s Observed operation latency in seconds.\n# TYPE %s histogram\n", name, name)
			previous = name
		}
		labels = strings.TrimSuffix(labels, "}")
		row := m.timings[key]
		for i, bound := range latencyBounds {
			fmt.Fprintf(&out, "%s_bucket{%s,le=%q} %d\n", name, labels, fmt.Sprint(bound), row.buckets[i])
		}
		fmt.Fprintf(&out, "%s_bucket{%s,le=\"+Inf\"} %d\n%s_sum{%s} %g\n%s_count{%s} %d\n", name, labels, row.count, name, labels, row.sum, name, labels, row.count)
	}
	fmt.Fprintf(&out, "# HELP tiny_sandbox_cleanup_failures_total Failed container removal attempts.\n# TYPE tiny_sandbox_cleanup_failures_total counter\ntiny_sandbox_cleanup_failures_total %d\n", m.cleanupFailures)
	m.mu.Unlock()
	h.store.mu.Lock()
	live := h.store.live
	h.store.mu.Unlock()
	fmt.Fprintf(&out, "# HELP tiny_sandbox_live Live sandbox records, excluding pending removals.\n# TYPE tiny_sandbox_live gauge\ntiny_sandbox_live %d\n# HELP tiny_sandbox_cleanup_pending Containers awaiting confirmed removal.\n# TYPE tiny_sandbox_cleanup_pending gauge\ntiny_sandbox_cleanup_pending %d\n", live, len(h.store.pendingRemovals()))
	if policy, ok := h.store.backend.(*PolicyBackend); ok {
		usage := policy.Usage()
		for _, name := range []string{"tiny_sandbox_reserved_memory_bytes", "tiny_sandbox_reserved_nano_cpus", "tiny_sandbox_reserved_pids"} {
			fmt.Fprintf(&out, "# HELP %s Reserved deployment budget, including unconfirmed cleanup.\n# TYPE %s gauge\n", name, name)
		}
		fmt.Fprintf(&out, "tiny_sandbox_reserved_memory_bytes %d\ntiny_sandbox_reserved_nano_cpus %d\ntiny_sandbox_reserved_pids %d\n", usage.MemoryMB<<20, usage.NanoCPUs, usage.Pids)
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = fmt.Fprint(w, out.String())
}
