package sandbox

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
)

// Capabilities returns the authenticated endpoint's currently supported flags.
func (c *Client) Capabilities(ctx context.Context) ([]string, error) {
	resp, err := c.do(ctx, request{method: http.MethodGet, path: "/v1/capabilities", repeatable: true})
	if err != nil {
		return nil, err
	}
	var value struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := decode(resp, &value); err != nil {
		return nil, err
	}
	return value.Capabilities, nil
}

// RequireCapability fails before mutation on old or unsupported servers.
func (c *Client) RequireCapability(ctx context.Context, capability string) error {
	flags, err := c.Capabilities(ctx)
	if err != nil {
		if value, ok := errors.AsType[*Error](err); !ok || value.StatusCode == 401 || value.StatusCode == 403 ||
			(value.StatusCode != 404 && value.StatusCode != 405 && value.StatusCode != 501) {
			return err
		}
	}
	if !slices.Contains(flags, capability) {
		return &Error{Code: "incompatible_server", Message: "server lacks " + capability}
	}
	return nil
}

// CommandAttachment contains an execution and its atomic attachment cursor.
type CommandAttachment struct {
	Execution   Execution `json:"execution"`
	LastEventID uint64    `json:"last_event_id"`
}

// CommandKill contains the terminal outcome and confirmed signal result.
type CommandKill struct {
	Execution Execution `json:"execution"`
	Signaled  bool      `json:"signaled"`
}

// AttachCommand selects the newest retained managed incarnation of a guest PID.
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

// KillExecution sends one immediate lifetime-bound kill, never retrying its ACK.
func (c *Client) KillExecution(ctx context.Context, sandboxID, executionID string) (*CommandKill, error) {
	if err := c.RequireCapability(ctx, "e2b.background.v1"); err != nil {
		return nil, err
	}
	// No automatic replay of a lost signal acknowledgement. The caller can
	// inspect this immutable execution, rather than mistake a later false for
	// confirmation of the original signal.
	resp, err := c.do(ctx, request{method: http.MethodPost, path: "/v1/sandboxes/" + url.PathEscape(sandboxID) + "/executions/" + url.PathEscape(executionID) + "/kill"})
	if err != nil {
		return nil, err
	}
	var result CommandKill
	if err := decode(resp, &result); err != nil {
		return nil, err
	}
	return &result, nil
}
