package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"

	json "github.com/ruhuang2001/tiny-sandbox/internal/wirejson"
)

const (
	baseBackoff = 200 * time.Millisecond
	maxBackoff  = 2 * time.Second
)

func backoff(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		return retryAfter
	}
	limit := baseBackoff << attempt
	if limit > maxBackoff || limit <= 0 {
		limit = maxBackoff
	}
	half := limit / 2
	return half + time.Duration(rand.Int64N(int64(half)+1))
}

func retryAfter(header http.Header) time.Duration {
	value := header.Get("Retry-After")
	if value == "" {
		return 0
	}
	seconds, err := strconv.Atoi(value)
	if err != nil || seconds < 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

func retryableStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

type request struct {
	method     string
	path       string
	query      string
	body       any
	repeatable bool
	accept     string
	raw        []byte
}

type response struct {
	status int
	header http.Header
	body   []byte
}

func (c *Client) do(ctx context.Context, req request) (*response, error) {
	var lastErr error
	for attempt := 0; ; attempt++ {
		resp, err := c.once(ctx, req)
		if err == nil && resp.status >= 200 && resp.status < 300 {
			return resp, nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil, &Error{Code: "canceled", Message: "sandbox: request canceled", Cause: ctx.Err()}
			}
			lastErr = &Error{Code: "transport", Message: "sandbox: request failed", Cause: err}
		} else {
			lastErr = statusError(resp)
		}

		retryable := err != nil || retryableStatus(resp.status)
		if !retryable || !req.repeatable || attempt >= c.maxRetries {
			return nil, lastErr
		}
		var after time.Duration
		if resp != nil {
			after = retryAfter(resp.header)
		}
		if !sleep(ctx, backoff(attempt, after)) {
			return nil, lastErr
		}
	}
}

func (c *Client) once(ctx context.Context, req request) (*response, error) {
	url := c.domain + req.path + req.query
	var body io.Reader
	contentType := ""
	switch {
	case req.raw != nil:
		body = bytes.NewReader(req.raw)
		contentType = "application/octet-stream"
	case req.body != nil:
		encoded, err := json.Marshal(req.body)
		if err != nil {
			return nil, fmt.Errorf("sandbox: encode request: %w", err)
		}
		body = bytes.NewReader(encoded)
		contentType = "application/json"
	}

	httpReq, err := http.NewRequestWithContext(ctx, req.method, url, body)
	if err != nil {
		return nil, fmt.Errorf("sandbox: build request: %w", err)
	}
	if contentType != "" {
		httpReq.Header.Set("Content-Type", contentType)
	}
	if req.accept != "" {
		httpReq.Header.Set("Accept", req.accept)
	}
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer func() { _ = httpResp.Body.Close() }()

	data, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("sandbox: read response: %w", err)
	}
	return &response{status: httpResp.StatusCode, header: httpResp.Header, body: data}, nil
}

func statusError(resp *response) *Error {
	var payload struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(resp.body, &payload)
	message := payload.Message
	if message == "" {
		message = fmt.Sprintf("sandbox: unexpected status %d", resp.status)
	}
	return &Error{StatusCode: resp.status, Code: payload.Code, Message: message}
}

func decode(resp *response, out any) error {
	if err := json.Unmarshal(resp.body, out); err != nil {
		return &Error{
			StatusCode: resp.status,
			Code:       "internal",
			Message:    "sandbox: could not parse the server response",
			Cause:      err,
		}
	}
	return nil
}

func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func isCanceled(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
