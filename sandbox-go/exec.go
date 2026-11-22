package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"time"
)

// StartExecution starts a command and returns the new execution. A start is
// retried only with an idempotency key: without one, a retry could run the
// command twice.
//
// See docs/sdk.md "Starting an execution" for the cancellation recovery: if
// the caller's context ends while the start is in flight, the SDK cannot know
// whether the server accepted it, so it repeats the start with the same key
// and then cancels whatever it finds.
func (c *Client) StartExecution(ctx context.Context, sandboxID string, req ExecuteRequest) (*Execution, error) {
	if len(req.Command) == 0 {
		return nil, &Error{Code: "invalid_request", Message: "sandbox: command must not be empty"}
	}
	// Own the invocation across dispatch and cancellation recovery.
	req.Command = slices.Clone(req.Command)
	req.Env = maps.Clone(req.Env)
	if req.TimeoutSeconds != nil {
		req.TimeoutSeconds = new(*req.TimeoutSeconds)
	}
	if req.IdempotencyKey == "" {
		req.IdempotencyKey = newIdempotencyKey()
	}
	if ctx.Err() != nil {
		return nil, &Error{Code: "canceled", Message: "sandbox: request canceled", Cause: ctx.Err()}
	}
	var required []string
	if req.TimeoutMS != nil {
		req.TimeoutMS = new(*req.TimeoutMS)
		if req.TimeoutSeconds != nil || *req.TimeoutMS < 0 || *req.TimeoutMS > 9223372036854 {
			return nil, &Error{Code: "invalid_request", Message: "invalid or conflicting execution timeout"}
		}
		required = append(required, "e2b.background.v1")
	}
	if len(req.Env) > 0 || req.Cwd != "" {
		required = append(required, "execution.env-cwd.v1")
	}
	if req.Stdin != "" {
		required = append(required, "execution.stdin.v1")
	}
	if err := c.RequireCapabilities(ctx, required...); err != nil {
		return nil, err
	}

	ex, err := c.startExecution(ctx, sandboxID, req)
	if err == nil {
		return ex, nil
	}
	if !isCanceled(err) {
		return nil, err
	}
	return c.recoverStart(sandboxID, req)
}

// startExecution is the plain request. It is safe to repeat because the key is
// always set by the caller.
func (c *Client) startExecution(ctx context.Context, sandboxID string, req ExecuteRequest) (*Execution, error) {
	resp, err := c.do(ctx, request{
		method:     http.MethodPost,
		path:       "/v1/sandboxes/" + url.PathEscape(sandboxID) + "/executions",
		body:       req,
		repeatable: true,
	})
	if err != nil {
		return nil, err
	}
	var ex Execution
	if err := decode(resp, &ex); err != nil {
		return nil, err
	}
	return &ex, nil
}

// recoverStart learns whether the canceled start ran anything, and cancels it
// if it did. A failed recovery is ErrCancelUnconfirmed carrying the key, so
// the caller can clean up rather than guess.
func (c *Client) recoverStart(sandboxID string, req ExecuteRequest) (*Execution, error) {
	recoverCtx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5e9)
	defer cancel()

	ex, err := c.startExecution(recoverCtx, sandboxID, req)
	if err != nil {
		return nil, &Error{
			Code:           "cancel_unconfirmed",
			Message:        "sandbox: the start was canceled but the server could not confirm what ran",
			IdempotencyKey: req.IdempotencyKey,
			Cause:          err,
		}
	}
	if ex.ID == "" || ex.SandboxID != sandboxID {
		return nil, &Error{
			Code:           "cancel_unconfirmed",
			Message:        "sandbox: recovered start did not confirm the requested sandbox identity",
			Execution:      &Execution{SandboxID: sandboxID},
			IdempotencyKey: req.IdempotencyKey,
		}
	}
	canceled, err := c.cancelConfirmed(recoverCtx, sandboxID, ex.ID)
	if err != nil {
		return nil, &Error{
			Code:           "cancel_unconfirmed",
			Message:        "sandbox: the execution was started but could not be canceled",
			Execution:      ex,
			IdempotencyKey: req.IdempotencyKey,
			Cause:          err,
		}
	}
	return canceled, &Error{
		Code:      string(ReasonCanceled),
		Message:   "sandbox: execution canceled",
		Execution: canceled,
	}
}

// GetExecution returns one execution with its retained output.
func (c *Client) GetExecution(ctx context.Context, sandboxID, executionID string) (*Execution, error) {
	resp, err := c.do(ctx, request{
		method:     http.MethodGet,
		path:       executionPath(sandboxID, executionID),
		repeatable: true,
	})
	if err != nil {
		return nil, err
	}
	var ex Execution
	if err := decode(resp, &ex); err != nil {
		return nil, err
	}
	return &ex, nil
}

// ListExecutions returns the sandbox's executions, newest first. The contract
// omits output from list responses.
func (c *Client) ListExecutions(ctx context.Context, sandboxID string) ([]*Execution, error) {
	resp, err := c.do(ctx, request{
		method:     http.MethodGet,
		path:       "/v1/sandboxes/" + url.PathEscape(sandboxID) + "/executions",
		repeatable: true,
	})
	if err != nil {
		return nil, err
	}
	var list executionList
	if err := decode(resp, &list); err != nil {
		return nil, err
	}
	return list.Executions, nil
}

// CancelExecution requests termination of the execution's process group. The
// bounded server fallback can return a running snapshot; inspect State before
// treating it as confirmed termination. Idempotent on a completed execution.
func (c *Client) CancelExecution(ctx context.Context, sandboxID, executionID string) (*Execution, error) {
	return c.cancelExecution(ctx, sandboxID, executionID, true)
}

func (c *Client) cancelExecution(ctx context.Context, sandboxID, executionID string, repeatable bool) (*Execution, error) {
	resp, err := c.do(ctx, request{
		method:     http.MethodPost,
		path:       executionPath(sandboxID, executionID) + "/cancel",
		repeatable: repeatable,
	})
	if err != nil {
		return nil, err
	}
	var ex Execution
	if err := decode(resp, &ex); err != nil {
		return nil, err
	}
	return &ex, nil
}

// Unknown outcomes (including failed persistence) do not prove termination.
func (c *Client) cancelConfirmed(ctx context.Context, sandboxID, executionID string) (*Execution, error) {
	ex, err := c.cancelExecution(ctx, sandboxID, executionID, false)
	for {
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err() // cleanup deadline, not confirmed ErrCanceled
			}
			return nil, err
		}
		if ex.ID != executionID || ex.SandboxID != sandboxID {
			return nil, errors.New("sandbox: cancellation snapshot identifies another execution")
		}
		if ex.State != "running" {
			break
		}
		if !sleep(ctx, 50*time.Millisecond) {
			return nil, ctx.Err()
		}
		ex, err = c.GetExecution(ctx, sandboxID, executionID)
	}
	if ex.State == "completed" && ex.TerminationReason != nil {
		switch *ex.TerminationReason {
		case ReasonCanceled, ReasonTimeout, ReasonOOM, ReasonEvicted:
			return ex, nil
		case ReasonExited:
			if ex.ExitCode != nil {
				return ex, nil
			}
		}
	}
	return nil, errors.New("sandbox: cancellation response did not confirm the requested execution's terminal outcome")
}

func executionPath(sandboxID, executionID string) string {
	return "/v1/sandboxes/" + url.PathEscape(sandboxID) + "/executions/" + url.PathEscape(executionID)
}

func newIdempotencyKey() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("sandbox: generate idempotency key: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}
