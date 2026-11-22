package sandbox

import (
	"context"
	"net/http"
	"net/url"
)

func (c *Client) CreateSandbox(ctx context.Context, req CreateSandboxRequest) (*Sandbox, error) {
	var required []string
	if req.Runtime != "" {
		required = append(required, "sandbox.runtime.v1")
	}
	if len(req.Env) > 0 {
		required = append(required, "execution.env-cwd.v1")
	}
	if req.MaxConcurrentExecutions != nil {
		required = append(required, "execution.concurrent.v1")
	}
	if err := c.RequireCapabilities(ctx, required...); err != nil {
		return nil, err
	}
	resp, err := c.do(ctx, request{method: http.MethodPost, path: "/v1/sandboxes", body: req})
	if err != nil {
		return nil, err
	}
	var sb Sandbox
	if err := decode(resp, &sb); err != nil {
		return nil, err
	}
	return &sb, nil
}

func (c *Client) GetSandbox(ctx context.Context, sandboxID string) (*Sandbox, error) {
	return c.sandboxOp(ctx, http.MethodGet, sandboxID, "")
}

func (c *Client) ListSandboxes(ctx context.Context) ([]*Sandbox, error) {
	resp, err := c.do(ctx, request{method: http.MethodGet, path: "/v1/sandboxes", repeatable: true})
	if err != nil {
		return nil, err
	}
	var list sandboxList
	if err := decode(resp, &list); err != nil {
		return nil, err
	}
	return list.Sandboxes, nil
}

func (c *Client) PauseSandbox(ctx context.Context, sandboxID string) (*Sandbox, error) {
	return c.sandboxOp(ctx, http.MethodPost, sandboxID, "/pause")
}

func (c *Client) ResumeSandbox(ctx context.Context, sandboxID string) (*Sandbox, error) {
	return c.sandboxOp(ctx, http.MethodPost, sandboxID, "/resume")
}

func (c *Client) KillSandbox(ctx context.Context, sandboxID string) (*Sandbox, error) {
	return c.sandboxOp(ctx, http.MethodDelete, sandboxID, "")
}

func (c *Client) DestroySandbox(ctx context.Context, id string) (*Sandbox, error) {
	if err := c.RequireCapabilities(ctx, "sandbox.purge.v1"); err != nil {
		return nil, err
	}
	return c.sandboxOp(ctx, http.MethodDelete, id, "?purge=true")
}

func (c *Client) sandboxOp(ctx context.Context, method, sandboxID, suffix string) (*Sandbox, error) {
	resp, err := c.do(ctx, request{
		method:     method,
		path:       "/v1/sandboxes/" + url.PathEscape(sandboxID) + suffix,
		repeatable: true,
	})
	if err != nil {
		return nil, err
	}
	var sb Sandbox
	if err := decode(resp, &sb); err != nil {
		return nil, err
	}
	return &sb, nil
}

func (c *Client) ReadFile(ctx context.Context, sandboxID, path string) ([]byte, error) {
	resp, err := c.do(ctx, request{
		method:     http.MethodGet,
		path:       "/v1/sandboxes/" + url.PathEscape(sandboxID) + "/files",
		query:      "?path=" + url.QueryEscape(path),
		repeatable: true,
	})
	if err != nil {
		return nil, err
	}
	return resp.body, nil
}

func (c *Client) WriteFile(ctx context.Context, sandboxID, path string, data []byte) error {
	_, err := c.putFile(ctx, sandboxID, path, data)
	return err
}

func (c *Client) WriteFileInfo(ctx context.Context, sandboxID, path string, data []byte) (*FileEntry, error) {
	resp, err := c.putFile(ctx, sandboxID, path, data)
	if err != nil {
		return nil, err
	}
	var entry FileEntry
	if err := decode(resp, &entry); err != nil {
		return nil, err
	}
	return &entry, nil
}

func (c *Client) putFile(ctx context.Context, sandboxID, path string, data []byte) (*response, error) {
	return c.do(ctx, request{
		method:     http.MethodPut,
		path:       "/v1/sandboxes/" + url.PathEscape(sandboxID) + "/files",
		query:      "?path=" + url.QueryEscape(path),
		raw:        data,
		repeatable: true,
	})
}
