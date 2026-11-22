package dockerbackend

import (
	"bytes"
	"io"
	"sync"
)

const bufferCap = 1 << 20

type bufferedStream struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    bytes.Buffer
	closed bool
}

func newBufferedStream() *bufferedStream {
	b := &bufferedStream{}
	b.cond = sync.NewCond(&b.mu)
	return b
}

func (b *bufferedStream) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	total := 0
	for len(p) > 0 {
		for b.closed {
			return total, io.ErrClosedPipe
		}
		room := bufferCap - b.buf.Len()
		if room <= 0 {
			b.cond.Wait()
			continue
		}
		n := min(room, len(p))
		b.buf.Write(p[:n])
		total += n
		p = p[n:]
		b.cond.Broadcast()
	}
	return total, nil
}

func (b *bufferedStream) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for b.buf.Len() == 0 && !b.closed {
		b.cond.Wait()
	}
	if b.buf.Len() == 0 {
		return 0, io.EOF
	}
	n := copy(p, b.buf.Bytes())
	b.buf.Next(n)
	b.cond.Broadcast()
	return n, nil
}

func (b *bufferedStream) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	b.cond.Broadcast()
	return nil
}

var (
	_ io.Writer = (*bufferedStream)(nil)
	_ io.Reader = (*bufferedStream)(nil)
)
