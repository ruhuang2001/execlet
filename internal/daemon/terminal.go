package daemon

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/ruhuang2001/execlet/internal/wirejson"
)

type TerminalBackend interface {
	OpenTerminal(context.Context, string, string, ExecSpec, uint16, uint16) error
	TerminalInput(context.Context, string, string, []byte) error
	TerminalResize(context.Context, string, string, uint16, uint16) error
	ReadTerminal(context.Context, string, string, int64) ([]byte, int64, bool, error)
	CloseTerminal(context.Context, string, string) error
}

type terminalRef struct {
	backend   TerminalBackend
	container string
	id        string
}

func (s *Sandboxes) terminalBackend() (TerminalBackend, error) {
	backend := s.backend
	if policy, ok := backend.(*PolicyBackend); ok {
		backend = policy.Backend
	}
	term, ok := backend.(TerminalBackend)
	if !ok {
		return nil, errorf(CodeUnavailable, "terminal requires a guest agent")
	}
	return term, nil
}

func (s *Sandboxes) terminal(sbID, id string) (terminalRef, error) {
	backend, err := s.terminalBackend()
	if err != nil {
		return terminalRef{}, err
	}
	sb, err := s.lookup(sbID)
	if err != nil {
		return terminalRef{}, err
	}
	sb.mu.Lock()
	defer sb.mu.Unlock()
	if sb.state != StateRunning {
		return terminalRef{}, sb.invalidState()
	}
	if _, known := sb.terminals[id]; !known {
		return terminalRef{}, errorf(CodeNotFound, "terminal does not exist")
	}
	sb.touchLocked(s.now())
	return terminalRef{backend, sb.container, id}, nil
}

type terminalSize struct {
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

func (h *Handler) openTerminal(w http.ResponseWriter, r *http.Request) {
	var req struct {
		terminalSize
		Command []string `json:"command"`
		Cwd     string   `json:"cwd"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Command) == 0 {
		req.Command = []string{"/bin/sh"}
	}
	for _, arg := range req.Command {
		if strings.ContainsRune(arg, 0) {
			writeDaemonError(r.Context(), w, errorf(CodeInvalidRequest, "terminal command contains NUL"))
			return
		}
	}
	if err := validateCwd(req.Cwd); err != nil {
		writeDaemonError(r.Context(), w, err)
		return
	}
	id, err := h.store.openTerminal(r.Context(), r.PathValue("sandbox_id"), ExecSpec{Command: req.Command, Dir: req.Cwd}, req.terminalSize)
	if err != nil {
		writeDaemonError(r.Context(), w, err)
		return
	}
	writeJSON(w, 201, map[string]string{"terminal_id": id})
}

func (s *Sandboxes) openTerminal(ctx context.Context, sandboxID string, spec ExecSpec, size terminalSize) (string, error) {
	b, err := s.terminalBackend()
	if err != nil {
		return "", err
	}
	sb, err := s.lookup(sandboxID)
	if err != nil {
		return "", err
	}
	sb.mu.Lock()
	defer sb.mu.Unlock()
	if sb.state != StateRunning {
		return "", sb.invalidState()
	}
	if err := s.refreshTerminals(ctx, sb); err != nil {
		return "", err
	}
	active := 0
	for _, running := range sb.terminals {
		if running {
			active++
		}
	}
	if active >= 8 {
		return "", errorf(CodeRateLimited, "terminal limit reached")
	}

	if sb.terminals == nil {
		sb.terminals = make(map[string]bool)
	}
	id := newID("term_")
	sb.terminals[id] = true
	if err := s.saveSandboxLocked(sb); err != nil {
		delete(sb.terminals, id)
		return "", err
	}
	spec.Env = mergedEnv(sb.opts.env, nil)
	ctx, cancel := backendCtx(ctx)
	defer cancel()
	for _, ex := range sb.running {
		ex.mu.Lock()
		ex.terminalOverlap = true
		ex.mu.Unlock()
	}
	if err := b.OpenTerminal(ctx, sb.container, id, spec, cmp.Or(size.Rows, 24), cmp.Or(size.Cols, 80)); err != nil {
		cleanup, stop := backendCtx(context.WithoutCancel(ctx))
		defer stop()
		if CodeOf(err) != CodeUnavailable && CodeOf(err) != CodeInvalidState {
			if closeErr := b.CloseTerminal(cleanup, sb.container, id); closeErr != nil && CodeOf(closeErr) != CodeNotFound {
				_ = s.terminateLocked(cleanup, sb, KillEvicted)
			}
		}
		delete(sb.terminals, id)
		if saveErr := s.saveSandboxLocked(sb); saveErr != nil {
			return "", saveErr
		}
		return "", backendError(err)
	}
	sb.touchLocked(s.now())
	return id, nil
}

func (h *Handler) terminalControl(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := backendCtx(r.Context())
	defer cancel()
	sbID := r.PathValue("sandbox_id")
	t, err := h.store.terminal(sbID, r.PathValue("terminal_id"))
	if err != nil {
		writeDaemonError(ctx, w, err)
		return
	}
	switch r.Pattern {
	case "POST /v1/sandboxes/{sandbox_id}/terminals/{terminal_id}/input":
		data, err := io.ReadAll(io.LimitReader(r.Body, (32<<10)+1))
		if err != nil || len(data) > 32<<10 {
			writeDaemonError(ctx, w, errorf(CodeInvalidRequest, "terminal input must fit in 32 KiB"))
			return
		}
		if err = t.backend.TerminalInput(ctx, t.container, t.id, data); err != nil {
			writeDaemonError(ctx, w, backendError(err))
			return
		}
	case "POST /v1/sandboxes/{sandbox_id}/terminals/{terminal_id}/resize":
		var size terminalSize
		if !decodeJSON(w, r, &size) {
			return
		}
		if size.Rows == 0 || size.Cols == 0 {
			writeDaemonError(ctx, w, errorf(CodeInvalidRequest, "positive rows and cols required"))
			return
		}
		if err = t.backend.TerminalResize(ctx, t.container, t.id, size.Rows, size.Cols); err != nil {
			writeDaemonError(ctx, w, backendError(err))
			return
		}
	default:
		if err = t.backend.CloseTerminal(ctx, t.container, t.id); err != nil && CodeOf(backendError(err)) != CodeNotFound {
			writeDaemonError(ctx, w, backendError(err))
			return
		}
		sb, err := h.store.lookup(sbID)
		if err != nil {
			writeDaemonError(ctx, w, err)
			return
		}
		sb.mu.Lock()
		delete(sb.terminals, t.id)
		err = h.store.saveSandboxLocked(sb)
		sb.mu.Unlock()
		if err != nil {
			writeDaemonError(ctx, w, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) terminalEvents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sbID := r.PathValue("sandbox_id")
	t, err := h.store.terminal(sbID, r.PathValue("terminal_id"))
	if err != nil {
		writeDaemonError(ctx, w, err)
		return
	}
	offset, err := strconv.ParseInt(cmp.Or(r.Header.Get("Last-Event-ID"), "0"), 10, 64)
	if err != nil || offset < 0 {
		writeDaemonError(ctx, w, errorf(CodeInvalidRequest, "invalid terminal offset"))
		return
	}
	release, err := h.store.OpenStream(sbID)
	if err != nil {
		writeDaemonError(ctx, w, err)
		return
	}
	defer release()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	controller := http.NewResponseController(w)
	started := false
	for {
		call, stop := backendCtx(ctx)
		data, next, closed, err := t.backend.ReadTerminal(call, t.container, t.id, offset)
		stop()
		if err != nil {
			if !started {
				writeDaemonError(ctx, w, backendError(err))
			}
			return
		}
		started = true
		if len(data) > 0 {
			line, err := wirejson.Marshal(map[string]any{"data": data, "truncated": next-int64(len(data)) > offset})
			if err != nil {
				return
			}
			if _, err = fmt.Fprintf(w, "id: %d\nevent: stdout\ndata: %s\n\n", next, line); err != nil {
				return
			}
			offset = next
		}
		if closed {
			_, _ = fmt.Fprint(w, "event: closed\ndata: {}\n\n")
		}
		if err := controller.Flush(); err != nil || closed {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-h.shuttingDown:
			return
		default:
		}
	}
}

type terminalStatus interface {
	TerminalDone(context.Context, string, string) (bool, error)
}

func (s *Sandboxes) refreshTerminals(ctx context.Context, sb *sandbox) error {
	if len(sb.terminals) == 0 {
		return nil
	}
	b, err := s.terminalBackend()
	if err != nil {
		return err
	}
	status, ok := b.(terminalStatus)
	if !ok {
		return nil
	}
	finished := 0
	for id, running := range sb.terminals {
		if running {
			done, err := status.TerminalDone(ctx, sb.container, id)
			if err != nil {
				if CodeOf(backendError(err)) == CodeNotFound {
					delete(sb.terminals, id)
					sb.dirty = true
					continue
				}
				return backendError(err)
			}
			if done {
				sb.terminals[id] = false
				sb.dirty = true
			}
		}
		if !sb.terminals[id] {
			finished++
			if finished > 8 {
				if err := b.CloseTerminal(ctx, sb.container, id); err != nil && CodeOf(backendError(err)) != CodeNotFound {
					return backendError(err)
				}
				delete(sb.terminals, id)
			}
		}
	}
	return nil
}
