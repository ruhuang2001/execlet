package daemon

import (
	"context"
	"net/http"
	"time"

	"github.com/ruhuang2001/execlet/internal/wirejson"
)

type Operation struct {
	Method string
	Path   string
}

type routeHandler func(*Handler, http.ResponseWriter, *http.Request)

type Route struct {
	Operation
	handle routeHandler
}

var routes = []Route{
	{Operation{http.MethodPost, "/v1/sandboxes/{sandbox_id}/terminals"}, (*Handler).openTerminal},
	{Operation{http.MethodPost, "/v1/sandboxes/{sandbox_id}/terminals/{terminal_id}/input"}, (*Handler).terminalControl},
	{Operation{http.MethodPost, "/v1/sandboxes/{sandbox_id}/terminals/{terminal_id}/resize"}, (*Handler).terminalControl},
	{Operation{http.MethodGet, "/v1/sandboxes/{sandbox_id}/terminals/{terminal_id}/events"}, (*Handler).terminalEvents},
	{Operation{http.MethodDelete, "/v1/sandboxes/{sandbox_id}/terminals/{terminal_id}"}, (*Handler).terminalControl},
	{Operation{http.MethodGet, "/healthz"}, (*Handler).handleHealth},
	{Operation{http.MethodGet, "/readyz"}, (*Handler).handleReady},
	{Operation{http.MethodGet, "/metrics"}, (*Handler).handleMetrics},
	{Operation{http.MethodGet, "/v1/capabilities"}, (*Handler).capabilities},
	{Operation{http.MethodGet, "/v1/sandboxes/{sandbox_id}/commands/{pid}"}, (*Handler).attachCommand},
	{Operation{http.MethodPost, "/v1/sandboxes/{sandbox_id}/executions/{execution_id}/kill"}, (*Handler).killExecution},
	{Operation{http.MethodPost, "/v1/volumes"}, (*Handler).createVolume},
	{Operation{http.MethodGet, "/v1/volumes"}, (*Handler).listVolumes},
	{Operation{http.MethodGet, "/v1/volumes/{volume_id}"}, (*Handler).getVolume},
	{Operation{http.MethodDelete, "/v1/volumes/{volume_id}"}, (*Handler).deleteVolume},

	{Operation{http.MethodPost, "/v1/sandboxes"}, (*Handler).createSandbox},
	{Operation{http.MethodGet, "/v1/sandboxes"}, (*Handler).listSandboxes},
	{Operation{http.MethodGet, "/v1/sandboxes/{sandbox_id}"}, (*Handler).getSandbox},
	{Operation{http.MethodPatch, "/v1/sandboxes/{sandbox_id}"}, (*Handler).updateSandbox},
	{Operation{http.MethodDelete, "/v1/sandboxes/{sandbox_id}"}, (*Handler).deleteSandbox},
	{Operation{http.MethodPost, "/v1/sandboxes/{sandbox_id}/pause"}, (*Handler).pauseSandbox},
	{Operation{http.MethodPost, "/v1/sandboxes/{sandbox_id}/resume"}, (*Handler).resumeSandbox},

	{Operation{http.MethodPost, "/v1/sandboxes/{sandbox_id}/executions"}, (*Handler).startExecution},
	{Operation{http.MethodGet, "/v1/sandboxes/{sandbox_id}/executions"}, (*Handler).listExecutions},
	{Operation{http.MethodGet, "/v1/sandboxes/{sandbox_id}/executions/{execution_id}"}, (*Handler).getExecution},
	{Operation{http.MethodPost, "/v1/sandboxes/{sandbox_id}/executions/{execution_id}/cancel"}, (*Handler).cancelExecution},
	{Operation{http.MethodGet, "/v1/sandboxes/{sandbox_id}/executions/{execution_id}/events"}, (*Handler).streamEvents},

	{Operation{http.MethodGet, "/v1/sandboxes/{sandbox_id}/files/archive"}, (*Handler).readArchive},
	{Operation{http.MethodPut, "/v1/sandboxes/{sandbox_id}/files/archive"}, (*Handler).writeArchive},
	{Operation{http.MethodGet, "/v1/sandboxes/{sandbox_id}/files/stat"}, (*Handler).statFile},
	{Operation{http.MethodGet, "/v1/sandboxes/{sandbox_id}/files/list"}, (*Handler).listFiles},
	{Operation{http.MethodGet, "/v1/sandboxes/{sandbox_id}/files"}, (*Handler).readFile},
	{Operation{http.MethodPut, "/v1/sandboxes/{sandbox_id}/files"}, (*Handler).writeFile},
}

func Routes() []Operation {
	ops := make([]Operation, len(routes))
	for i, route := range routes {
		ops[i] = route.Operation
	}
	return ops
}

func (h *Handler) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": h.version})
}

type ReadinessBackend interface{ Ping(context.Context) error }

func (h *Handler) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	status, state, docker := http.StatusServiceUnavailable, "unavailable", "unreachable"
	if h.store != nil && h.store.durableReady() == nil {
		if backend, ok := h.store.backend.(ReadinessBackend); ok && backend.Ping(ctx) == nil {
			status, state, docker = http.StatusOK, "ok", "ok"
		}
	}
	writeJSON(w, status, map[string]string{"status": state, "version": h.version, "docker": docker})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if data, err := wirejson.Marshal(body); err == nil {
		_, _ = w.Write(append(data, '\n'))
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"code": code, "message": message})
}

func routeKey(method, path string) string {
	return method + " " + path
}
