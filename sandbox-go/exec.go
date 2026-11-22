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

func (c *Client) StartExecution(ctx context.Context, sandboxID string, req ExecuteRequest) (*Execution, error) {
	if len(req.Command) == 0 {
		return nil, &Error{Code: "invalid_request", Message: "sandbox: command must not be empty"}
	}
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
	if req.OutputTail {
		required = append(required, "execution.output-tail.v1")
	}
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

func (c *Client) cancelConfirmed(ctx context.Context, sandboxID, executionID string) (*Execution, error) {
	ex, err := c.cancelExecution(ctx, sandboxID, executionID, false)
	for {
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
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
