package sandbox

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

func terminalPath(sb, id string) string {
	path := "/v1/sandboxes/" + url.PathEscape(sb) + "/terminals"
	if id != "" {
		path += "/" + url.PathEscape(id)
	}
	return path
}

type WindowSize struct {
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

func (c *Client) OpenTerminal(ctx context.Context, sb string, size WindowSize) (string, error) {
	resp, err := c.do(ctx, request{method: http.MethodPost, path: terminalPath(sb, ""), body: size})
	if err != nil {
		return "", err
	}
	var result struct {
		ID string `json:"terminal_id"`
	}
	if err := decode(resp, &result); err != nil {
		return "", err
	}
	return result.ID, nil
}

func (c *Client) TerminalInput(ctx context.Context, sb, id string, data []byte) error {
	_, err := c.do(ctx, request{method: http.MethodPost, path: terminalPath(sb, id) + "/input", raw: data})
	return err
}

func (c *Client) ResizeTerminal(ctx context.Context, sb, id string, size WindowSize) error {
	_, err := c.do(ctx, request{method: http.MethodPost, path: terminalPath(sb, id) + "/resize", body: size, repeatable: true})
	return err
}

func (c *Client) CloseTerminal(ctx context.Context, sb, id string) error {
	_, err := c.do(ctx, request{method: http.MethodDelete, path: terminalPath(sb, id)})
	return err
}

func (c *Client) StreamTerminal(ctx context.Context, sb, id string, offset uint64, output io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.domain+terminalPath(sb, id)+"/events", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Last-Event-ID", strconv.FormatUint(offset, 10))
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return statusError(&response{status: resp.StatusCode, body: body})
	}
	events := newSSEReader(resp.Body)
	for {
		event, err := events.next()
		if errors.Is(err, io.EOF) {
			return io.ErrUnexpectedEOF
		}
		if err != nil {
			return err
		}
		if event == nil {
			continue
		}
		if event.Type == "closed" {
			return nil
		}
		if event.Type == EventStdout && event.ID > offset {
			data, err := base64.StdEncoding.DecodeString(event.Data)
			if err != nil {
				return err
			}
			if _, err := output.Write(data); err != nil {
				return err
			}
			offset = event.ID
		}
	}
}
