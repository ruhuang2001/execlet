package sandbox

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	json "github.com/ruhuang2001/execlet/internal/wirejson"
)

func (c *Client) StreamExecution(ctx context.Context, sandboxID, executionID string, onEvent func(Event) error) (*Execution, error) {
	return c.streamExecution(ctx, sandboxID, executionID, onEvent, true)
}

func (c *Client) ObserveExecution(ctx context.Context, sandboxID, executionID string) (*Execution, error) {
	return c.streamExecution(ctx, sandboxID, executionID, nil, false)
}

func (c *Client) streamExecution(ctx context.Context, sandboxID, executionID string, onEvent func(Event) error, cancelRemote bool) (*Execution, error) {
	if onEvent == nil {
		onEvent = func(Event) error { return nil }
	}
	lastID := uint64(0)
	failures := 0

	for {
		ex, complete, err := c.streamOnce(ctx, sandboxID, executionID, lastID, onEvent, &lastID)
		if complete {
			return ex, nil
		}
		if sdkErr, ok := errors.AsType[*Error](err); ok && sdkErr.Code == "callback" {
			return nil, err
		}
		if ctx.Err() != nil {
			if !cancelRemote {
				return nil, ctx.Err()
			}
			return c.cancelAfterContext(ctx, sandboxID, executionID, err)
		}
		if err == nil || !retryableStreamError(err) {
			return nil, err
		}

		failures++
		if failures > c.maxRetries {
			return nil, &Error{
				StatusCode: statusOf(err),
				Code:       "stream",
				Message:    fmt.Sprintf("sandbox: stream failed %d times during observation", failures),
				Cause:      err,
			}
		}
		if !sleep(ctx, backoff(failures-1, 0)) {
			if !cancelRemote {
				return nil, ctx.Err()
			}
			return c.cancelAfterContext(ctx, sandboxID, executionID, err)
		}
	}
}

func (c *Client) streamOnce(ctx context.Context, sandboxID, executionID string, lastID uint64, onEvent func(Event) error, cursor *uint64) (ex *Execution, complete bool, err error) {
	url := c.domain + executionPath(sandboxID, executionID) + "/events"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false, fmt.Errorf("sandbox: build stream request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	if lastID > 0 {
		req.Header.Set("Last-Event-ID", strconv.FormatUint(lastID, 10))
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, false, &Error{Code: "transport", Message: "sandbox: stream failed", Cause: err}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, false, statusError(&response{status: resp.StatusCode, body: data})
	}

	reader := newSSEReader(resp.Body)
	for {
		event, err := reader.next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, false, &Error{Code: "transport", Message: "sandbox: stream ended before the execution completed", Cause: err}
			}
			return nil, false, err
		}
		if event == nil {
			continue
		}
		if event.ID <= *cursor {
			continue
		}
		*cursor = event.ID

		if event.Type == EventComplete {
			if event.Execution == nil {
				return nil, false, &Error{Code: "stream", Message: "sandbox: the complete event carried no execution"}
			}
			if err := onEvent(*event); err != nil {
				return nil, false, callbackError(err)
			}
			return event.Execution, true, nil
		}
		if err := onEvent(*event); err != nil {
			return nil, false, callbackError(err)
		}
	}
}

func (c *Client) cancelAfterContext(_ context.Context, sandboxID, executionID string, cause error) (*Execution, error) {
	fresh, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ex, err := c.cancelConfirmed(fresh, sandboxID, executionID)
	if err != nil {
		return nil, &Error{
			Code:      "cancel_unconfirmed",
			Message:   "sandbox: the caller ended the stream but the execution could not be canceled",
			Execution: &Execution{ID: executionID, SandboxID: sandboxID},
			Cause:     err,
		}
	}
	return ex, &Error{
		Code:      string(ReasonCanceled),
		Message:   "sandbox: execution canceled",
		Execution: ex,
		Cause:     cause,
	}
}

func callbackError(err error) error {
	return &Error{Code: "callback", Message: "sandbox: event handler failed", Cause: err}
}

func retryableStreamError(err error) bool {
	var sdkErr *Error
	if errors.As(err, &sdkErr) {
		return sdkErr.Retryable()
	}
	return true
}

func statusOf(err error) int {
	var sdkErr *Error
	if errors.As(err, &sdkErr) {
		return sdkErr.StatusCode
	}
	return 0
}

type sseReader struct {
	scanner *bufio.Scanner
	pending *Event
	atEOF   bool
}

const maxEventBytes = 16 << 20

func newSSEReader(r io.Reader) *sseReader {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), maxEventBytes)
	return &sseReader{scanner: scanner}
}

func (r *sseReader) next() (*Event, error) {
	if r.pending != nil {
		event := r.pending
		r.pending = nil
		r.atEOF = true
		return event, nil
	}
	if r.atEOF {
		return nil, io.EOF
	}
	var (
		event   Event
		data    strings.Builder
		hasData bool
	)
	for r.scanner.Scan() {
		line := r.scanner.Text()
		switch {
		case line == "":
			if hasData || event.Type != "" {
				return parseEvent(&event, data.String())
			}
			continue
		case strings.HasPrefix(line, ":"):
			continue
		case strings.HasPrefix(line, "id:"):
			value := strings.TrimSpace(strings.TrimPrefix(line, "id:"))
			id, err := strconv.ParseUint(value, 10, 64)
			if err != nil {
				return nil, &Error{Code: "stream", Message: "sandbox: event id is not an integer: " + value}
			}
			event.ID = id
		case strings.HasPrefix(line, "event:"):
			event.Type = EventType(strings.TrimSpace(strings.TrimPrefix(line, "event:")))
		case strings.HasPrefix(line, "data:"):
			if hasData {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			hasData = true
		}
	}
	if err := r.scanner.Err(); err != nil {
		return nil, err
	}
	if hasData || event.Type != "" {
		event, parseErr := parseEvent(&event, data.String())
		if parseErr != nil {
			return nil, parseErr
		}
		r.pending = event
		return event, nil
	}
	r.atEOF = true
	return nil, io.EOF
}

func parseEvent(event *Event, data string) (*Event, error) {
	if event.Type == "" {
		return nil, &Error{Code: "stream", Message: "sandbox: an event carried data but no event type"}
	}
	switch event.Type {
	case EventStdout, EventStderr:
		var payload struct {
			Data string `json:"data"`
		}
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			return nil, &Error{Code: "stream", Message: "sandbox: could not parse an output event", Cause: err}
		}
		event.Data = payload.Data
	case EventTruncated:
		var payload struct {
			LimitBytes int `json:"limit_bytes"`
		}
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			return nil, &Error{Code: "stream", Message: "sandbox: could not parse a truncated event", Cause: err}
		}
		event.LimitBytes = payload.LimitBytes
	case EventComplete:
		ex, err := parseExecution([]byte(data))
		if err != nil {
			return nil, err
		}
		event.Execution = ex
	}
	return event, nil
}

func parseExecution(data []byte) (*Execution, error) {
	var ex Execution
	if err := json.Unmarshal(data, &ex); err != nil {
		return nil, &Error{Code: "stream", Message: "sandbox: could not parse the complete event", Cause: err}
	}
	return &ex, nil
}

func (c *Client) Exec(ctx context.Context, sandboxID string, command []string) (*Execution, error) {
	return c.ExecStream(ctx, sandboxID, command, nil)
}

func (c *Client) ExecStream(ctx context.Context, sandboxID string, command []string, onEvent func(Event) error) (*Execution, error) {
	started, err := c.StartExecution(ctx, sandboxID, ExecuteRequest{Command: command})
	if err != nil {
		return executionOf(err), err
	}
	ex, err := c.StreamExecution(ctx, sandboxID, started.ID, onEvent)
	if err != nil {
		if ex == nil {
			ex = executionOf(err)
		}
		return ex, err
	}
	if err := outcomeError(ex); err != nil {
		return ex, err
	}
	return ex, nil
}

func executionOf(err error) *Execution {
	var sdkErr *Error
	if errors.As(err, &sdkErr) {
		return sdkErr.Execution
	}
	return nil
}
