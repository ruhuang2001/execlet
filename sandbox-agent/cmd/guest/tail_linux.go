package main

import (
	"bytes"
	"sync"

	"github.com/ruhuang2001/tiny-sandbox/internal/guestprotocol"
)

const tailPageBytes = 64 << 10

type tailChunk struct {
	stderr bool
	data   []byte
}

type jobTail struct {
	mu      sync.Mutex
	chunks  []tailChunk
	size    int
	baseOut int64
	baseErr int64
	changed chan struct{}
}

type tailWriter struct {
	tail   *jobTail
	stderr bool
}

func (w tailWriter) Write(p []byte) (int, error) {
	t := w.tail
	t.mu.Lock()
	defer t.mu.Unlock()
	n := len(p)
	for len(p) > 0 {
		part := p[:min(len(p), 32<<10)]
		t.chunks = append(t.chunks, tailChunk{w.stderr, bytes.Clone(part)})
		t.size += len(part)
		p = p[len(part):]
		for t.size > 1<<20 || len(t.chunks) > 4096 {
			old := t.chunks[0]
			if old.stderr {
				t.baseErr += int64(len(old.data))
			} else {
				t.baseOut += int64(len(old.data))
			}
			t.size -= len(old.data)
			t.chunks[0] = tailChunk{}
			t.chunks = t.chunks[1:]
		}
	}
	select {
	case t.changed <- struct{}{}:
	default:
	}
	return n, nil
}

func (t *jobTail) read(r protocol.Request, result *protocol.Result) {
	t.mu.Lock()
	defer t.mu.Unlock()
	out, errOut := t.baseOut, t.baseErr
	result.Next, result.ErrNext = max(out, r.Offset), max(errOut, r.ErrOffset)
	for _, chunk := range t.chunks {
		pos, next, data, other := &out, &result.Next, &result.Stdout, result.Stderr
		if chunk.stderr {
			pos, next, data, other = &errOut, &result.ErrNext, &result.Stderr, result.Stdout
		}
		start := *pos
		*pos += int64(len(chunk.data))
		if *pos <= *next {
			continue
		}
		if len(other) != 0 {
			break
		}
		n := min(*pos-*next, int64(tailPageBytes-len(*data)))
		*data = append(*data, chunk.data[*next-start:*next-start+n]...)
		*next += n
		if len(*data) == tailPageBytes {
			break
		}
	}
}
