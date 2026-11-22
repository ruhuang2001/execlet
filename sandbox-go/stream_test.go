package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeServer struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	requests []recordedRequest
	handler  http.HandlerFunc
}

type recordedRequest struct {
	method string
	path   string
	query  string
	header http.Header
	body   []byte
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	f := &fakeServer{t: t}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		f.mu.Lock()
		f.requests = append(f.requests, recordedRequest{
			method: r.Method, path: r.URL.Path, query: r.URL.RawQuery,
			header: r.Header.Clone(), body: body,
		})
		handler := f.handler
		f.mu.Unlock()
		if handler == nil {
			http.NotFound(w, r)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeServer) setHandler(h http.HandlerFunc) {
	f.mu.Lock()
	f.handler = h
	f.mu.Unlock()
}

func (f *fakeServer) count(method, path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if r.method == method && r.path == path {
			n++
		}
	}
	return n
}

func (f *fakeServer) client(t *testing.T, maxRetries int) *Client {
	t.Helper()
	t.Setenv("SANDBOX_DOMAIN", "")
	t.Setenv("SANDBOX_API_KEY", "")
	c, err := NewClient(Config{Domain: f.srv.URL, MaxRetries: maxRetries, HTTPClient: fastClient()})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return c
}

func fastClient() *http.Client {
	return &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 5 * time.Second}}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"code": code, "message": message})
}

func sseFrame(id uint64, kind EventType, payload string) string {
	return fmt.Sprintf("id: %d\nevent: %s\ndata: %s\n\n", id, kind, strings.ReplaceAll(payload, "\n", `\n`))
}

func completePayload(id string) string {
	code := 0
	reason := ReasonExited
	ex := Execution{ID: id, SandboxID: "sbx_1", State: "completed", ExitCode: &code, TerminationReason: &reason}
	data, _ := json.Marshal(ex)
	return string(data)
}

func TestStreamReconnectsFromLastEventID(t *testing.T) {
	f := newFakeServer(t)
	attempt := atomic.Int32{}

	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		last := r.Header.Get("Last-Event-ID")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		switch attempt.Add(1) {
		case 1:
			_, _ = io.WriteString(w, sseFrame(1, EventStdout, `{"data":"one\n"}`))
			_, _ = io.WriteString(w, sseFrame(2, EventStdout, `{"data":"two\n"}`))
			f.t.Logf("first attempt: Last-Event-ID=%q, dropping", last)
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			return
		default:
			_, _ = io.WriteString(w, sseFrame(1, EventStdout, `{"data":"one\n"}`))
			_, _ = io.WriteString(w, sseFrame(2, EventStdout, `{"data":"two\n"}`))
			_, _ = io.WriteString(w, sseFrame(3, EventStdout, `{"data":"three\n"}`))
			_, _ = io.WriteString(w, sseFrame(4, EventComplete, completePayload("exe_1")))
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			return
		}
	})

	c := f.client(t, 3)
	var got []string
	var ids []uint64
	ex, err := c.StreamExecution(context.Background(), "sbx_1", "exe_1", func(ev Event) error {
		if ev.Type == EventStdout {
			got = append(got, ev.Data)
		}
		ids = append(ids, ev.ID)
		return nil
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if ex.ID != "exe_1" {
		t.Errorf("execution = %+v", ex)
	}
	if strings.Join(got, "") != "one\ntwo\nthree\n" {
		t.Errorf("delivered output = %q", got)
	}
	want := []uint64{1, 2, 3, 4}
	if len(ids) != len(want) {
		t.Fatalf("delivered ids = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("delivered ids = %v, want %v", ids, want)
		}
	}
	if n := f.count(http.MethodGet, "/v1/sandboxes/sbx_1/executions/exe_1/events"); n != 2 {
		t.Errorf("%d stream attempts, want 2", n)
	}
}

func TestStreamDropsEventsAtOrBelowTheCursor(t *testing.T) {
	f := newFakeServer(t)
	f.setHandler(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, sseFrame(2, EventStdout, `{"data":"two\n"}`))
		_, _ = io.WriteString(w, sseFrame(1, EventStdout, `{"data":"stale\n"}`))
		_, _ = io.WriteString(w, sseFrame(2, EventStdout, `{"data":"again\n"}`))
		_, _ = io.WriteString(w, sseFrame(3, EventComplete, completePayload("exe_1")))
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
	})

	c := f.client(t, 0)
	var ids []uint64
	var out strings.Builder
	_, err := c.StreamExecution(context.Background(), "sbx_1", "exe_1", func(ev Event) error {
		ids = append(ids, ev.ID)
		if ev.Type == EventStdout {
			out.WriteString(ev.Data)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if out.String() != "two\n" {
		t.Errorf("delivered output = %q, want only the first event at or above the cursor", out.String())
	}
	want := []uint64{2, 3}
	if len(ids) != len(want) {
		t.Fatalf("delivered ids = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("delivered ids = %v, want %v", ids, want)
		}
	}
}

func TestStreamCallbackErrorStopsWithoutCancel(t *testing.T) {
	for _, boom := range []error{errors.New("renderer failed"), &Error{Code: "transport"}, fmt.Errorf("render: %w", &Error{StatusCode: 503})} {
		for _, endContext := range []bool{false, true} {
			t.Run(fmt.Sprintf("%T/context-ended=%v", boom, endContext), func(t *testing.T) {
				f := newFakeServer(t)
				f.setHandler(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, sseFrame(1, EventStdout, `{"data":"x"}`))
					_, _ = io.WriteString(w, sseFrame(2, EventComplete, completePayload("exe_1")))
				})
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				callbacks := 0
				_, err := f.client(t, 1).StreamExecution(ctx, "sbx_1", "exe_1", func(event Event) error {
					callbacks++
					if event.Type != EventStdout {
						return nil
					}
					if endContext {
						cancel()
					}
					return boom
				})
				if !errors.Is(err, boom) {
					t.Errorf("err = %v, want original callback error %v", err, boom)
				}
				if callbacks != 1 {
					t.Errorf("callbacks = %d, want 1", callbacks)
				}
				if n := f.count(http.MethodGet, "/v1/sandboxes/sbx_1/executions/exe_1/events"); n != 1 {
					t.Errorf("observations = %d, want 1", n)
				}
				if n := f.count(http.MethodPost, "/v1/sandboxes/sbx_1/executions/exe_1/cancel"); n != 0 {
					t.Errorf("callback canceled remote work %d times", n)
				}
			})
		}
	}
}

func TestAutomaticCancelRequiresTerminalIdentity(t *testing.T) {
	for _, entry := range []string{"stream", "exec", "start"} {
		for _, response := range []string{"running", "wrong-running", "wrong-poll", "wrong-execution", "wrong-sandbox", "missing-reason", "unknown-reason", "invalid-reason", "exited", "completed"} {
			t.Run(entry+"/"+response, func(t *testing.T) {
				f := newFakeServer(t)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				var starts atomic.Int32
				var polls atomic.Int32
				f.setHandler(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case strings.HasSuffix(r.URL.Path, "/executions"):
						if entry == "start" && starts.Add(1) == 1 {
							cancel()
							<-r.Context().Done()
							return
						}
						writeJSON(w, http.StatusOK, Execution{ID: "exe_1", SandboxID: "sbx_1", State: "running"})
					case strings.HasSuffix(r.URL.Path, "/cancel"):
						ex := Execution{ID: "exe_1", SandboxID: "sbx_1", State: "completed", TerminationReason: new(ReasonCanceled)}
						switch response {
						case "running", "wrong-poll", "wrong-running":
							ex.State = "running"
							ex.TerminationReason = nil
							if response == "wrong-running" {
								ex.ID = "another"
							}
						case "wrong-execution":
							ex.ID = "exe_other"
						case "wrong-sandbox":
							ex.SandboxID = "sbx_other"
						case "missing-reason":
							ex.TerminationReason = nil
						case "unknown-reason":
							ex.TerminationReason = new(ReasonUnknown)
						case "invalid-reason":
							ex.TerminationReason = new(Reason("future-reason"))
						case "exited":
							ex.TerminationReason = new(ReasonExited)
							ex.ExitCode = new(0)
						}
						writeJSON(w, http.StatusOK, ex)
					case strings.HasSuffix(r.URL.Path, "/exe_1") && r.Method == http.MethodGet:
						ex := Execution{ID: "exe_1", SandboxID: "sbx_1", State: "running"}
						if polls.Add(1) == 1 && response == "wrong-poll" {
							ex.SandboxID = "other"
						} else if polls.Load() > 1 {
							ex.State, ex.TerminationReason = "completed", new(ReasonCanceled)
						}
						writeJSON(w, http.StatusOK, ex)
					default:
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, sseFrame(1, EventStdout, `{"data":"ready"}`))
						w.(http.Flusher).Flush()
						<-r.Context().Done()
					}
				})
				c := f.client(t, -1)
				callback := func(Event) error { cancel(); return nil }
				var err error
				switch entry {
				case "start":
					_, err = c.StartExecution(ctx, "sbx_1", ExecuteRequest{Command: []string{"true"}, IdempotencyKey: "original-key"})
				case "exec":
					_, err = c.ExecStream(ctx, "sbx_1", []string{"true"}, callback)
				default:
					_, err = c.StreamExecution(ctx, "sbx_1", "exe_1", callback)
				}
				want := ErrCancelUnconfirmed
				if response == "completed" || response == "exited" || response == "running" {
					want = ErrCanceled
				}
				if n := f.count(http.MethodPost, "/v1/sandboxes/sbx_1/executions/exe_1/cancel"); n != 1 {
					t.Fatalf("sent %d cancel requests", n)
				}
				if wantPolls := map[string]int32{"running": 2, "wrong-poll": 1}[response]; polls.Load() != wantPolls {
					t.Fatalf("polls = %d, want %d", polls.Load(), wantPolls)
				}
				if !errors.Is(err, want) {
					t.Fatalf("err = %v, want %v", err, want)
				}
				if errors.Is(want, ErrCancelUnconfirmed) && errors.Is(err, ErrCanceled) {
					t.Fatal("unconfirmed cancellation also matched ErrCanceled")
				}
				sdkErr, ok := errors.AsType[*Error](err)
				if !ok || sdkErr.Execution == nil || sdkErr.Execution.ID != "exe_1" || sdkErr.Execution.SandboxID != "sbx_1" {
					t.Fatalf("lost requested identity: %+v", sdkErr)
				}
				if entry == "start" && errors.Is(want, ErrCancelUnconfirmed) && sdkErr.IdempotencyKey != "original-key" {
					t.Fatalf("lost start key: %+v", sdkErr)
				}
			})
		}
	}
}

func TestStreamContextCancelCancelsExecution(t *testing.T) {
	f := newFakeServer(t)
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cancel") {
			reason := ReasonCanceled
			writeJSON(w, http.StatusOK, Execution{ID: "exe_1", SandboxID: "sbx_1", State: "completed", TerminationReason: &reason})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, sseFrame(1, EventStdout, `{"data":"x"}`))
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		<-r.Context().Done()
	})

	c := f.client(t, 3)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	ex, err := c.StreamExecution(ctx, "sbx_1", "exe_1", nil)
	if !errors.Is(err, ErrCanceled) {
		t.Fatalf("err = %v, want ErrCanceled", err)
	}
	if ex == nil || ex.ID != "exe_1" {
		t.Errorf("execution = %+v", ex)
	}
	if n := f.count(http.MethodPost, "/v1/sandboxes/sbx_1/executions/exe_1/cancel"); n != 1 {
		t.Errorf("cancel count = %d, want 1", n)
	}
}

func TestStreamCancelUnconfirmed(t *testing.T) {
	f := newFakeServer(t)
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cancel") {
			writeAPIError(w, http.StatusServiceUnavailable, "unavailable", "shutting down")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		<-r.Context().Done()
	})

	c := f.client(t, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := c.StreamExecution(ctx, "sbx_1", "exe_1", nil)
	if !errors.Is(err, ErrCancelUnconfirmed) {
		t.Fatalf("err = %v, want ErrCancelUnconfirmed", err)
	}
}

func TestStartRecoversAfterCancelDuringStart(t *testing.T) {
	f := newFakeServer(t)
	var pushed atomic.Bool

	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/executions"):
			var req ExecuteRequest
			_ = json.Unmarshal(readBody(r), &req)
			if req.IdempotencyKey == "" {
				t.Error("start request carries no idempotency key")
			}
			if !pushed.Swap(true) {
				<-r.Context().Done()
				return
			}
			writeJSON(w, http.StatusOK, Execution{ID: "exe_1", SandboxID: "sbx_1", State: "running"})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cancel"):
			reason := ReasonCanceled
			writeJSON(w, http.StatusOK, Execution{ID: "exe_1", SandboxID: "sbx_1", State: "completed", TerminationReason: &reason})
		default:
			http.NotFound(w, r)
		}
	})

	c := f.client(t, 1)
	done, cancelDone := context.WithCancel(context.Background())
	cancelDone()
	if _, err := c.StartExecution(done, "sbx_1", ExecuteRequest{Command: []string{"sleep", "10"}}); err == nil {
		t.Fatal("an already-done context still sent a request")
	}
	if n := f.count(http.MethodPost, "/v1/sandboxes/sbx_1/executions"); n != 0 {
		t.Fatalf("%d starts sent with a done context, want 0", n)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	ex, err := c.StartExecution(ctx, "sbx_1", ExecuteRequest{Command: []string{"sleep", "10"}})
	if !errors.Is(err, ErrCanceled) {
		t.Fatalf("err = %v, want ErrCanceled", err)
	}
	if ex == nil || ex.ID != "exe_1" {
		t.Fatalf("execution = %+v, want exe_1", ex)
	}
	if n := f.count(http.MethodPost, "/v1/sandboxes/sbx_1/executions/exe_1/cancel"); n != 1 {
		t.Errorf("cancel count = %d, want 1", n)
	}
}

func readBody(r *http.Request) []byte {
	data, _ := io.ReadAll(r.Body)
	return data
}

func TestStreamAcceptsTheLargestCompleteEvent(t *testing.T) {
	f := newFakeServer(t)

	worst := strings.Repeat("\x01", 1<<20)
	code := 0
	reason := ReasonExited
	payload, err := json.Marshal(Execution{
		ID: "exe_1", SandboxID: "sbx_1", State: "completed",
		ExitCode: &code, TerminationReason: &reason,
		Stdout: worst[:512<<10], Stderr: worst[512<<10:],
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	f.setHandler(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "id: 1\nevent: complete\ndata: "+string(payload)+"\n\n")
	})
	if len(payload) < 6<<20 {
		t.Fatalf("test payload is only %d bytes; it does not reach the limit it exists to exercise", len(payload))
	}

	c := f.client(t, 1)
	ex, err := c.StreamExecution(context.Background(), "sbx_1", "exe_1", nil)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(ex.Stdout) != 512<<10 || len(ex.Stderr) != 512<<10 {
		t.Fatalf("stdout=%d stderr=%d, want 512 KiB each", len(ex.Stdout), len(ex.Stderr))
	}
}
