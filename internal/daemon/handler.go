package daemon

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ruhuang2001/tiny-sandbox/internal/wirejson"
)

type Handler struct {
	store        *Sandboxes
	version      string
	apiKey       string
	shuttingDown <-chan struct{}
	mux          *http.ServeMux
}

func NewHandler(store *Sandboxes, version string) *Handler {
	h := &Handler{store: store, version: version}
	h.mux = h.buildMux()
	return h
}

func (h *Handler) WithAPIKey(key string) *Handler {
	h.apiKey = key
	return h
}

func (h *Handler) WithShutdown(done <-chan struct{}) *Handler {
	h.shuttingDown = done
	return h
}

func (h *Handler) Handler(req *http.Request) (http.Handler, string) {
	return h.mux.Handler(req)
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	id := requestID(r.Header.Values("X-Request-ID"))
	w.Header().Set("X-Request-ID", id)
	_, pattern := h.mux.Handler(r)
	registered := false
	for _, route := range routes {
		if pattern == routeKey(route.Method, route.Path) {
			registered = true
			break
		}
	}
	if !registered {
		pattern = "unmatched"
	}
	logger := slog.Default().With("request_id", id, "method", r.Method, "route", pattern)
	r = r.WithContext(context.WithValue(r.Context(), requestLoggerKey{}, logger))
	recorded := &responseRecorder{ResponseWriter: w}
	var response http.ResponseWriter = recorded
	if _, ok := w.(http.Flusher); ok {
		response = &flushingResponse{responseRecorder: recorded}
	}
	w = response
	completed := false
	defer func() {
		status := recorded.status
		if status == 0 {
			status = http.StatusOK
			if !completed {
				status = http.StatusInternalServerError
			}
		}
		logger.InfoContext(r.Context(), "http request", "status", status, "duration_ms", float64(time.Since(started).Microseconds())/1000, "bytes", recorded.bytes, "completed", completed)
		if h.store != nil {
			h.store.metrics.observe(fmt.Sprintf("tiny_sandbox_http_seconds{route=%q,status=%q}", pattern, strconv.Itoa(status)), started)
		}
	}()
	if h.apiKey != "" && r.URL.Path != "/healthz" && r.URL.Path != "/readyz" && !h.authorized(r) {
		writeError(w, http.StatusUnauthorized, string(CodeUnauthorized), "missing or wrong API key")
		completed = true
		return
	}
	h.mux.ServeHTTP(w, r)
	completed = true
}

func (h *Handler) authorized(r *http.Request) bool {
	header := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || len(token) != len(h.apiKey) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(h.apiKey)) == 1
}

func (h *Handler) buildMux() *http.ServeMux {
	return newMux(h, routes)
}

func newMux(h *Handler, table []Route) *http.ServeMux {
	mux := http.NewServeMux()
	for _, route := range table {
		key := routeKey(route.Method, route.Path)
		if route.handle == nil {
			panic("daemon: no handler for " + key)
		}
		handle := route.handle
		mux.HandleFunc(key, func(w http.ResponseWriter, r *http.Request) {
			handle(h, w, r)
		})
	}
	return mux
}

func (h *Handler) createSandbox(w http.ResponseWriter, r *http.Request) {
	var req CreateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	sb, err := h.store.Create(r.Context(), req)
	if err != nil {
		writeDaemonError(r.Context(), w, err)
		return
	}
	contextLogger(r.Context()).InfoContext(r.Context(), "sandbox created", "sandbox_id", sb.ID, "state", sb.State)
	w.Header().Set("Location", "/v1/sandboxes/"+sb.ID)
	writeJSON(w, http.StatusCreated, sb)
}

func (h *Handler) listSandboxes(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"sandboxes": h.store.List()})
}

func (h *Handler) getSandbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("sandbox_id")
	if id == "" {
		writeError(w, http.StatusNotFound, string(CodeNotFound), "sandbox id is required")
		return
	}
	h.store.Touch(id)
	sb, err := h.store.Get(id)
	if err != nil {
		writeDaemonError(r.Context(), w, err)
		return
	}
	writeJSON(w, http.StatusOK, sb)
}

func (h *Handler) updateSandbox(w http.ResponseWriter, r *http.Request) {
	var req UpdateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	sb, err := h.store.Update(r.Context(), r.PathValue("sandbox_id"), req)
	if err != nil {
		writeDaemonError(r.Context(), w, err)
		return
	}
	contextLogger(r.Context()).InfoContext(r.Context(), "sandbox lifetime updated",
		"sandbox_id", sb.ID, "timeout_seconds", sb.TimeoutSeconds)
	writeJSON(w, http.StatusOK, sb)
}

func (h *Handler) deleteSandbox(w http.ResponseWriter, r *http.Request) {
	sb, err := h.store.Delete(r.Context(), r.PathValue("sandbox_id"))
	if err != nil {
		writeDaemonError(r.Context(), w, err)
		return
	}
	contextLogger(r.Context()).InfoContext(r.Context(), "sandbox deleted", "sandbox_id", sb.ID, "state", sb.State)
	writeJSON(w, http.StatusOK, sb)
}

func (h *Handler) pauseSandbox(w http.ResponseWriter, r *http.Request) {
	h.lifecycle(w, r, h.store.Pause)
}

func (h *Handler) resumeSandbox(w http.ResponseWriter, r *http.Request) {
	h.lifecycle(w, r, h.store.Resume)
}

func (h *Handler) lifecycle(w http.ResponseWriter, r *http.Request, op func(context.Context, string) (Sandbox, error)) {
	sb, err := op(r.Context(), r.PathValue("sandbox_id"))
	if err != nil {
		writeDaemonError(r.Context(), w, err)
		return
	}
	contextLogger(r.Context()).InfoContext(r.Context(), "sandbox state changed", "sandbox_id", sb.ID, "state", sb.State)
	writeJSON(w, http.StatusOK, sb)
}

func (h *Handler) startExecution(w http.ResponseWriter, r *http.Request) {
	var req ExecuteRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	ex, created, err := h.store.StartExecution(r.Context(), r.PathValue("sandbox_id"), req)
	if err != nil {
		writeDaemonError(r.Context(), w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, ex)
}

func (h *Handler) listExecutions(w http.ResponseWriter, r *http.Request) {
	list, err := h.store.ListExecutions(r.PathValue("sandbox_id"))
	if err != nil {
		writeDaemonError(r.Context(), w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"executions": list})
}

func (h *Handler) getExecution(w http.ResponseWriter, r *http.Request) {
	ex, err := h.store.GetExecution(r.PathValue("sandbox_id"), r.PathValue("execution_id"))
	if err != nil {
		writeDaemonError(r.Context(), w, err)
		return
	}
	writeJSON(w, http.StatusOK, ex)
}

func (h *Handler) cancelExecution(w http.ResponseWriter, r *http.Request) {
	ex, err := h.store.CancelExecution(r.Context(), r.PathValue("sandbox_id"), r.PathValue("execution_id"))
	if err != nil {
		writeDaemonError(r.Context(), w, err)
		return
	}
	writeJSON(w, http.StatusOK, ex)
}

func (h *Handler) readFile(w http.ResponseWriter, r *http.Request) {
	path, ok := filePath(w, r)
	if !ok {
		return
	}
	data, err := h.store.ReadFile(r.Context(), r.PathValue("sandbox_id"), path)
	if err != nil {
		writeDaemonError(r.Context(), w, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
}

func (h *Handler) writeFile(w http.ResponseWriter, r *http.Request) {
	path, ok := filePath(w, r)
	if !ok {
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(FileOperationTimeout))
	defer func() { _ = rc.SetReadDeadline(time.Time{}) }()
	body := http.MaxBytesReader(w, r.Body, MaxFileBytes)
	data, err := io.ReadAll(body)
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			writeDaemonError(r.Context(), w, errorf(CodePayloadTooLarge, "file upload exceeds %d bytes", MaxFileBytes))
		} else {
			writeDaemonError(r.Context(), w, errorf(CodeInvalidRequest, "cannot read file upload body"))
		}
		return
	}
	if err := h.store.WriteFile(r.Context(), r.PathValue("sandbox_id"), path, data); err != nil {
		writeDaemonError(r.Context(), w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func filePath(w http.ResponseWriter, r *http.Request) (string, bool) {
	p := r.URL.Query().Get("path")
	if err := validateRegularFilePath(p); err != nil {
		writeDaemonError(r.Context(), w, err)
		return "", false
	}
	return p, true
}

func (h *Handler) streamEvents(w http.ResponseWriter, r *http.Request) {
	sandboxID := r.PathValue("sandbox_id")
	executionID := r.PathValue("execution_id")

	var lastID uint64
	if header := r.Header.Get("Last-Event-ID"); header != "" {
		n, err := strconv.ParseUint(header, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, string(CodeInvalidRequest), "Last-Event-ID must be a non-negative integer")
			return
		}
		lastID = n
	}

	stream, err := h.store.Events(sandboxID, executionID, lastID)
	if err != nil {
		writeDaemonError(r.Context(), w, err)
		return
	}
	defer stream.Close()

	streamCtx, stopStream := context.WithCancel(r.Context())
	defer stopStream()
	if h.shuttingDown != nil {
		go func() {
			select {
			case <-h.shuttingDown:
				stopStream()
			case <-streamCtx.Done():
			}
		}()
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, string(CodeInternal), "streaming is not supported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	var writeMu sync.Mutex
	write := func(data []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		if _, err := w.Write(data); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	pingDone := make(chan struct{})
	var pingWait sync.WaitGroup
	pingWait.Add(1)
	defer func() {
		close(pingDone)
		pingWait.Wait()
	}()
	go func() {
		defer pingWait.Done()
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-pingDone:
				return
			case <-streamCtx.Done():
				return
			case <-ticker.C:
				if err := write([]byte(": ping\n\n")); err != nil {
					return
				}
			}
		}
	}()

	for {
		events, done, err := stream.Next(streamCtx)
		if err != nil {
			return
		}
		if len(events) > 0 {
			var batch bytes.Buffer
			for _, ev := range events {
				data, err := renderEvent(ev)
				if err != nil {
					return
				}
				batch.Write(data)
			}
			if err := write(batch.Bytes()); err != nil {
				return
			}
		}
		if done {
			return
		}
	}
}

func renderEvent(ev Event) ([]byte, error) {
	var payload any
	switch ev.Type {
	case EventStdout, EventStderr:
		payload = map[string]string{"data": string(ev.Data)}
	case EventTruncated:
		payload = map[string]int{"limit_bytes": ev.LimitBytes}
	case EventComplete:
		payload = ev.Execution
	default:
		payload = ev.Data
	}
	data, err := wirejson.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("daemon: encode %s event: %w", ev.Type, err)
	}
	return fmt.Appendf(nil, "id: %d\nevent: %s\ndata: %s\n\n", ev.ID, ev.Type, data), nil
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	limit := int64(MaxRequestBodyBytes)
	if _, execution := v.(*ExecuteRequest); execution {
		limit = MaxExecutionRequestBodyBytes
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			writeError(w, http.StatusRequestEntityTooLarge, string(CodePayloadTooLarge),
				fmt.Sprintf("request body exceeds %d bytes", limit))
			return false
		}
		writeError(w, http.StatusBadRequest, string(CodeInvalidRequest), "malformed JSON body")
		return false
	}
	if len(bytes.Trim(body, " \t\r\n")) != 0 && wirejson.Unmarshal(body, v) != nil {
		writeError(w, http.StatusBadRequest, string(CodeInvalidRequest), "malformed JSON body")
		return false
	}
	return true
}

func writeDaemonError(ctx context.Context, w http.ResponseWriter, err error) {
	e, ok := errors.AsType[*Error](err)
	if !ok {
		contextLogger(ctx).ErrorContext(ctx, "request failed", "code", CodeInternal)
		writeError(w, http.StatusInternalServerError, string(CodeInternal), "unexpected server error")
		return
	}
	if e.Code == CodeBackendError || e.Code == CodeInternal {
		contextLogger(ctx).ErrorContext(ctx, "request failed", "code", e.Code)
	}
	writeError(w, statusFor(e.Code), string(e.Code), e.Message)
}

func statusFor(code Code) int {
	switch code {
	case CodeInvalidRequest:
		return http.StatusBadRequest
	case CodeUnauthorized:
		return http.StatusUnauthorized
	case CodeNotFound:
		return http.StatusNotFound
	case CodeInvalidState, CodeBusy, CodeIdempotencyConflict:
		return http.StatusConflict
	case CodePayloadTooLarge:
		return http.StatusRequestEntityTooLarge
	case CodeRateLimited:
		return http.StatusTooManyRequests
	case CodeBackendError:
		return http.StatusBadGateway
	case CodeUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}
