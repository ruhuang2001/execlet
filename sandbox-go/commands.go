package sandbox

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
)

func (c *Client) Capabilities(ctx context.Context) ([]string, error) {
	flags, err := c.discover(ctx, true)
	return slices.Clone(flags), err
}

func (c *Client) discover(ctx context.Context, refresh bool) ([]string, error) {
	select {
	case c.discoveryGate <- struct{}{}:
		defer func() { <-c.discoveryGate }()
	case <-ctx.Done():
		return nil, &Error{Code: "canceled", Message: "sandbox: discovery canceled", Cause: ctx.Err()}
	}
	if err := ctx.Err(); err != nil {
		return nil, &Error{Code: "canceled", Message: "sandbox: discovery canceled", Cause: err}
	}
	if !refresh && c.capabilities != nil {
		return c.capabilities, nil
	}
	resp, err := c.do(ctx, request{method: http.MethodGet, path: "/v1/capabilities", repeatable: true})
	if err != nil {
		return nil, err
	}
	var value struct {
		Capabilities []*string `json:"capabilities"`
	}
	if err := decode(resp, &value); err != nil {
		return nil, err
	}
	if value.Capabilities == nil || slices.Contains(value.Capabilities, nil) {
		return nil, &Error{Code: "incompatible_server", Message: "invalid capability advertisement"}
	}
	flags := make([]string, len(value.Capabilities))
	for i, flag := range value.Capabilities {
		flags[i] = *flag
	}
	c.capabilities = flags
	return flags, nil
}

func (c *Client) RequireCapability(ctx context.Context, capability string) error {
	return c.RequireCapabilities(ctx, capability)
}

func (c *Client) RequireCapabilities(ctx context.Context, capabilities ...string) error {
	if len(capabilities) == 0 {
		return nil
	}
	flags, err := c.discover(ctx, false)
	if err != nil {
		if value, ok := errors.AsType[*Error](err); ok {
			switch value.StatusCode {
			case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented:
				return &Error{Code: "incompatible_server", Message: "server does not advertise required capabilities"}
			}
		}
		return err
	}
	for _, capability := range capabilities {
		if !slices.Contains(flags, capability) {
			return &Error{Code: "incompatible_server", Message: "server lacks " + capability}
		}
	}
	return nil
}

type CommandAttachment struct {
	Execution   Execution `json:"execution"`
	LastEventID uint64    `json:"last_event_id"`
}

type CommandKill struct {
	Execution Execution `json:"execution"`
	Signaled  bool      `json:"signaled"`
}

func (c *Client) AttachCommand(ctx context.Context, sandboxID string, pid int) (*CommandAttachment, error) {
	if pid <= 0 {
		return nil, &Error{Code: "invalid_request", Message: "pid must be positive"}
	}
	if err := c.RequireCapability(ctx, "e2b.background.v1"); err != nil {
		return nil, err
	}
	resp, err := c.do(ctx, request{method: http.MethodGet, path: "/v1/sandboxes/" + url.PathEscape(sandboxID) + "/commands/" + strconv.Itoa(pid), repeatable: true})
	if err != nil {
		return nil, err
	}
	var result CommandAttachment
	if err := decode(resp, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *Client) KillExecution(ctx context.Context, sandboxID, executionID string) (*CommandKill, error) {
	if err := c.RequireCapability(ctx, "e2b.background.v1"); err != nil {
		return nil, err
	}
	identity := &Execution{ID: executionID, SandboxID: sandboxID}
	resp, err := c.do(ctx, request{method: http.MethodPost, path: "/v1/sandboxes/" + url.PathEscape(sandboxID) + "/executions/" + url.PathEscape(executionID) + "/kill"})
	if err != nil {
		if sdkErr, ok := errors.AsType[*Error](err); ok {
			uncertain := sdkErr.StatusCode >= 500 || sdkErr.Code == "transport" || sdkErr.Code == "canceled"
			code := sdkErr.Code
			msg := sdkErr.Message
			if uncertain {
				code = "backend_error"
				msg = "sandbox: kill acknowledgement was not confirmed"
			}
			return nil, &Error{StatusCode: sdkErr.StatusCode, Code: code, Message: msg, Execution: identity, Cause: err}
		}
		return nil, &Error{Code: "backend_error", Message: "sandbox: kill acknowledgement was not confirmed", Execution: identity, Cause: err}
	}
	var wire struct {
		Execution Execution `json:"execution"`
		Signaled  *bool     `json:"signaled"`
	}
	if err := decode(resp, &wire); err != nil || wire.Signaled == nil {
		return nil, &Error{StatusCode: resp.status, Code: "backend_error", Message: "sandbox: kill acknowledgement was not confirmed", Execution: identity, Cause: err}
	}
	result := CommandKill{Execution: wire.Execution, Signaled: *wire.Signaled}
	ex := result.Execution
	if ex.ID != executionID || ex.SandboxID != sandboxID {
		return nil, &Error{StatusCode: resp.status, Code: "backend_error", Message: "sandbox: kill response identifies a different execution", Execution: identity}
	}
	if ex.State != "completed" || ex.TerminationReason == nil || *ex.TerminationReason == "" {
		return nil, &Error{StatusCode: resp.status, Code: "backend_error", Message: "sandbox: command termination was not confirmed", Execution: &ex}
	}
	if result.Signaled && *ex.TerminationReason != ReasonCanceled {
		return nil, &Error{StatusCode: resp.status, Code: "backend_error", Message: "sandbox: kill response does not confirm the requested signal", Execution: &ex}
	}
	return &result, nil
}
