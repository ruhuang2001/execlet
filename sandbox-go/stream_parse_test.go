package sandbox

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestSSEReaderRejectsDataWithoutEvent(t *testing.T) {
	for _, stream := range []string{
		"data:",
		"data: {\"data\":\"x\"}\n\n",
		"id: 7\ndata: {\"data\":\"x\"}\n\n",
	} {
		reader := newSSEReader(strings.NewReader(stream))
		event, err := reader.next()
		if err == nil {
			t.Fatalf("%q: event = %+v, want an error", stream, event)
		}
		var sdkErr *Error
		if !errors.As(err, &sdkErr) || sdkErr.Code != "stream" {
			t.Fatalf("%q: err = %v, want a stream Error", stream, err)
		}
	}
}

func TestSSEReaderHandlesFieldOrderAndCRLF(t *testing.T) {
	for name, stream := range map[string]string{
		"lf":   "id: 1\ndata: {\"data\":\"hello\"}\nevent: stdout\n\n",
		"crlf": "id: 1\r\ndata: {\"data\":\"hello\"}\r\nevent: stdout\r\n\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			reader := newSSEReader(strings.NewReader(stream))
			event, err := reader.next()
			if err != nil {
				t.Fatalf("next: %v", err)
			}
			if event.Type != EventStdout || event.Data != "hello" || event.ID != 1 {
				t.Fatalf("event = %+v, want stdout/hello/id=1", event)
			}
			if _, err := reader.next(); !errors.Is(err, io.EOF) {
				t.Fatalf("second next = %v, want EOF", err)
			}
		})
	}
}
