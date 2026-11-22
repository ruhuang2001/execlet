package egress

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"
)

func reject(w io.Writer, status int) {
	_, _ = fmt.Fprintf(w, "HTTP/1.1 %d %s\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", status, http.StatusText(status))
}

func (p Policy) Serve(ctx context.Context, stream io.ReadWriteCloser, image string) {
	defer func() { _ = stream.Close() }()
	reader := bufio.NewReaderSize(stream, 32<<10)
	var header bytes.Buffer
	for header.Len() < 32<<10 {
		line, err := reader.ReadSlice('\n')
		if err != nil {
			return
		}
		header.Write(line)
		if bytes.Equal(line, []byte("\r\n")) {
			break
		}
	}
	if header.Len() >= 32<<10 {
		reject(stream, 431)
		return
	}
	req, err := http.ReadRequest(bufio.NewReader(&header))
	if err != nil || req.ContentLength != 0 || len(req.TransferEncoding) > 0 {
		reject(stream, 400)
		return
	}
	if req.Method == http.MethodGet && req.RequestURI == "/credential" && req.Host == "127.0.0.1:8888" {
		p.fetchCredential(ctx, stream, image)
		return
	}
	if req.Method != http.MethodConnect || !strings.HasSuffix(req.Host, ":443") || req.Host != req.RequestURI {
		reject(stream, 403)
		return
	}
	dialCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	upstream, err := p.dial(dialCtx, "tcp", req.Host)
	cancel()
	if err != nil {
		reject(stream, 403)
		return
	}
	defer func() { _ = upstream.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = upstream.Close() })
	defer stop()
	if _, err = io.WriteString(stream, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	go func() {
		_, _ = io.Copy(upstream, reader)
		if tcp, ok := upstream.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
	}()
	_, _ = io.Copy(stream, upstream)
}

func (p Policy) fetchCredential(ctx context.Context, w io.Writer, image string) {
	c := p.Credential
	if c == nil || !slices.Contains(c.Images, image) {
		reject(w, 403)
		return
	}
	token := os.Getenv(c.TokenEnv)
	if token == "" {
		reject(w, 503)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
	if err != nil {
		reject(w, 502)
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	transport := &http.Transport{DialContext: p.dial, DisableKeepAlives: true, MaxResponseHeaderBytes: 32 << 10}
	defer transport.CloseIdleConnections()
	resp, err := transport.RoundTrip(req)
	if err != nil {
		reject(w, 502)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		reject(w, 502)
		return
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 || bytes.Contains(data, []byte(token)) {
		reject(w, 502)
		return
	}
	_, _ = fmt.Fprintf(w, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n", len(data))
	_, _ = w.Write(data)
}
