package sandbox

import (
	"context"
	"net/http"
	"net/url"
)

func (c *Client) UpdateSandbox(ctx context.Context, sandboxID string, update UpdateSandboxRequest) (*Sandbox, error) {
	resp, err := c.do(ctx, request{
		method: http.MethodPatch,
		path:   "/v1/sandboxes/" + url.PathEscape(sandboxID),
		body:   update,
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
