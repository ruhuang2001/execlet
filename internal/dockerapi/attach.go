package dockerapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"

	"github.com/ruhuang2001/execlet/internal/wirejson"
)

type ImageInfo struct {
	Architecture string
	ID           string `json:"Id"`
	Config       struct{ User string }
}

func (c *Client) ImageInspect(ctx context.Context, image string) (ImageInfo, error) {
	var out ImageInfo
	err := c.getJSON(ctx, "/images/"+url.PathEscape(image)+"/json", &out)
	return out, err
}

func (c *Client) ExecStartDetached(ctx context.Context, execID string) error {
	return c.postJSON(ctx, "/exec/"+execID+"/start", nil, execStart{Detach: true}, nil)
}
func (c *Client) ExecAttach(ctx context.Context, containerID string, cfg ExecConfig, tty bool) (io.ReadWriteCloser, error) {
	cfg.AttachStdin, cfg.AttachStdout, cfg.AttachStderr, cfg.Tty = true, true, true, tty
	id, err := c.execCreate(ctx, containerID, cfg)
	if err != nil {
		return nil, err
	}
	body, err := wirejson.Marshal(execStart{Tty: tty})
	if err != nil {
		return nil, err
	}
	req, err := c.request(ctx, http.MethodPost, "/exec/"+id+"/start", nil, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "tcp")
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	stream, ok := resp.Body.(io.ReadWriteCloser)
	if resp.StatusCode != http.StatusSwitchingProtocols || !ok {
		_ = resp.Body.Close()
		return nil, errors.New("docker exec did not upgrade to a bidirectional stream")
	}
	if tty {
		return stream, nil
	}
	r, w := io.Pipe()
	go func() { err := Demux(stream, w, io.Discard); _ = w.CloseWithError(err) }()
	return &attached{ReadWriteCloser: stream, reader: r}, nil
}

type attached struct {
	io.ReadWriteCloser
	reader *io.PipeReader
}

func (a *attached) Read(p []byte) (int, error) { return a.reader.Read(p) }
func (a *attached) Close() error               { _ = a.reader.Close(); return a.ReadWriteCloser.Close() }
