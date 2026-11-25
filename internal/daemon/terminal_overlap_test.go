package daemon_test

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/ruhuang2001/execlet/internal/daemon"
	"github.com/ruhuang2001/execlet/internal/daemon/membackend"
)

type forgottenTerminalBackend struct {
	*membackend.Backend
	daemon.TerminalBackend
}

func (b forgottenTerminalBackend) OpenTerminal(context.Context, string, string, daemon.ExecSpec, uint16, uint16) error {
	return nil
}

func (b forgottenTerminalBackend) TerminalDone(context.Context, string, string) (bool, error) {
	return false, daemon.ErrNotFound
}

func (b forgottenTerminalBackend) CloseTerminal(context.Context, string, string) error {
	return daemon.ErrNotFound
}

func TestTerminalOverlapAndForgottenJobs(t *testing.T) {
	for _, first := range []bool{true, false} {
		h := newHarness(t)
		h.s = daemon.New(forgottenTerminalBackend{Backend: h.be}, daemon.WithClassifierSleep(noSleep))
		srv := httptest.NewServer(daemon.NewHandler(h.s, "test"))
		t.Cleanup(srv.Close)
		http := &handlerHarness{t: t, srv: srv, be: h.be}
		ready := make(chan struct{})
		h.be.Programs["wait"] = func(context.Context, []string) (string, string, int) { <-ready; return "", "", 0 }
		sb := h.create(daemon.CreateRequest{})
		path := "/v1/sandboxes/" + sb.ID + "/terminals"
		open := func() string {
			r := http.do("POST", path, []byte(`{}`), nil)
			if r.Status != 201 {
				t.Fatalf("open: %s", r.Body)
			}
			var term struct {
				ID string `json:"terminal_id"`
			}
			http.jsonBody(r, &term)
			return term.ID
		}
		if first {
			open()
		}
		ex, _, err := h.s.StartExecution(t.Context(), sb.ID, daemon.ExecuteRequest{Command: []string{"wait"}})
		wantNoError(t, err)
		if !first {
			open()
		}
		h.be.SetOOMKills(h.containerOf(sb.ID), 1)
		close(ready)
		done := h.waitCompleted(sb.ID, ex.ID)
		if *done.TerminationReason != daemon.ReasonExited || *done.ExitCode != 0 {
			t.Fatalf("terminal first=%v: %+v", first, done)
		}
		id := open()
		if r := http.do("DELETE", path+"/"+id, nil, nil); r.Status != 204 {
			t.Fatalf("close: %s", r.Body)
		}
	}
}
