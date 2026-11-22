//go:build live

package sandbox_test

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"

	sandbox "github.com/ruhuang2001/tiny-sandbox/sandbox-go"
)

type acceptedStartFault struct {
	once   sync.Once
	mutate func()
}

func (f *acceptedStartFault) RoundTrip(req *http.Request) (*http.Response, error) {
	response, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	faulted := false
	if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/executions") && response.StatusCode == http.StatusCreated {
		f.once.Do(func() {
			faulted = true
			_ = response.Body.Close()
			f.mutate()
		})
	}
	if faulted {
		return nil, req.Context().Err()
	}
	return response, nil
}

func TestLiveEnvRecoverySnapshotsInvocation(t *testing.T) {
	direct := liveClient(t)
	sb := liveSandbox(t, direct, "alpine:3.20", 64)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req := sandbox.ExecuteRequest{Command: []string{"sleep", "30"}, Env: map[string]string{"PHASE": "original"}, TimeoutSeconds: new(60)}
	fault := &acceptedStartFault{mutate: func() {
		req.Command[1] = "60"
		req.Env["PHASE"] = "mutated"
		*req.TimeoutSeconds = -1
		cancel()
	}}
	client, err := sandbox.NewClient(sandbox.Config{Domain: cmp.Or(os.Getenv("SANDBOX_DOMAIN"), "http://127.0.0.1:18080"), HTTPClient: &http.Client{Transport: fault}})
	if err != nil {
		t.Fatal(err)
	}
	execution, err := client.StartExecution(ctx, sb.ID, req)
	if !errors.Is(err, sandbox.ErrCanceled) {
		t.Fatalf("recovery: %v", err)
	}
	if execution == nil || execution.State != "completed" || execution.TerminationReason == nil || *execution.TerminationReason != sandbox.ReasonCanceled {
		t.Fatalf("recovery did not confirm cancellation: %+v", execution)
	}
	all, err := direct.ListExecutions(t.Context(), sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].ID != execution.ID || all[0].Command[1] != "30" {
		t.Fatalf("recovery changed or repeated invocation: %+v", all)
	}
	t.Logf("accepted execution %s recovered and canceled once; caller env and command mutation ignored", execution.ID)
}
