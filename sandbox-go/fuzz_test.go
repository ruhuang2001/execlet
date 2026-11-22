package sandbox

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func FuzzSSEReader(f *testing.F) {
	f.Add([]byte("id: 1\nevent: stdout\ndata: {\"data\":\"hello\\n\"}\n\n"))
	f.Add([]byte("id: 2\nevent: stderr\ndata: {\"data\":\"warn\\n\"}\n\n: ping\n\n"))
	f.Add([]byte("id: 3\nevent: complete\ndata: {\"id\":\"exe_1\",\"state\":\"completed\"}\n\n"))
	f.Add([]byte("event: stdout\ndata: {\"data\":\"a\"}\ndata: {\"data\":\"b\"}\n\n"))
	f.Add([]byte("id: 1\r\nevent: stdout\r\ndata: {\"data\":\"crlf\"}\r\n\r\n"))
	f.Add([]byte("id: not-a-number\nevent: stdout\ndata: {}\n\n"))
	f.Add([]byte("id: 1\nevent: stdout\ndata: {\"data\":\"unterminated\"}"))
	f.Add([]byte(": ping\n\n"))
	f.Add([]byte("data:\n\n"))
	f.Add([]byte(""))

	f.Fuzz(func(t *testing.T, data []byte) {
		counted := &countingReader{r: bytes.NewReader(data)}
		reader := newSSEReader(counted)

		var eventCount int
		for {
			event, err := reader.next()
			if errors.Is(err, io.EOF) {
				if remaining := counted.left(); remaining != 0 {
					t.Fatalf("parser stopped with %d bytes unconsumed: %q", remaining, data)
				}
				return
			}
			if err != nil {
				var sdkErr *Error
				if !errors.As(err, &sdkErr) {
					t.Fatalf("parser returned a bare error: %v", err)
				}
				return
			}
			if event == nil {
				continue
			}
			eventCount++
			if eventCount > len(data)+1 {
				t.Fatalf("parsed more events than the input can contain: %d", eventCount)
			}
			if event.Type == "" {
				t.Fatalf("event %d has no type", eventCount)
			}
			if event.Type == EventComplete && event.Execution == nil {
				t.Fatalf("complete event without an execution: %q", data)
			}
		}
	})
}

type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func (c *countingReader) left() int {
	if r, ok := c.r.(*bytes.Reader); ok {
		return r.Len()
	}
	return 0
}
