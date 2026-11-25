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
	fmt.Fprintf(&out, "# HELP execlet_cleanup_failures_total Failed storage or container cleanup attempts.\n# TYPE execlet_cleanup_failures_total counter\nexeclet_cleanup_failures_total %d\n", m.cleanupFailures)
	m.mu.Unlock()
	h.store.retryMu.Lock()
	live := 0
	for _, active := range h.store.reservationLive {
		if active {
			live++
		}
	}
	pending := len(h.store.retryIDs)
	h.store.retryMu.Unlock()
	fmt.Fprintf(&out, "# HELP execlet_live Live sandbox records, excluding pending removals.\n# TYPE execlet_live gauge\nexeclet_live %d\n# HELP execlet_cleanup_pending Containers awaiting confirmed removal.\n# TYPE execlet_cleanup_pending gauge\nexeclet_cleanup_pending %d\n", live, pending)
	if policy, ok := h.store.backend.(*PolicyBackend); ok {
		usage := policy.Usage()
		policy.mu.Lock()
		ready := 0
		for _, ids := range policy.ready {
			ready += len(ids)
		}
		policy.mu.Unlock()
		fmt.Fprintf(&out, "# HELP execlet_prewarm_ready Unassigned prewarmed containers.\n# TYPE execlet_prewarm_ready gauge\nexeclet_prewarm_ready %d\n", ready)
		for _, name := range []string{"execlet_reserved_memory_bytes", "execlet_reserved_nano_cpus", "execlet_reserved_pids"} {
			fmt.Fprintf(&out, "# HELP %s Reserved deployment budget, including unconfirmed cleanup.\n# TYPE %s gauge\n", name, name)
		}
		fmt.Fprintf(&out, "execlet_reserved_memory_bytes %d\nexeclet_reserved_nano_cpus %d\nexeclet_reserved_pids %d\n", usage.MemoryMB<<20, usage.NanoCPUs, usage.Pids)
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = fmt.Fprint(w, out.String())
}
