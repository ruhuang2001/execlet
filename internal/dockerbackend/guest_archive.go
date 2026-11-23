package dockerbackend

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
	"github.com/ruhuang2001/tiny-sandbox/internal/dockerapi"
	"github.com/ruhuang2001/tiny-sandbox/internal/filearchive"
	"github.com/ruhuang2001/tiny-sandbox/internal/guestprotocol"
	"io"
	"strconv"
)

func (b *Backend) archiveGet(ctx context.Context, id, path string) (io.ReadCloser, error) {
	if g := b.guest(id); g != nil && g.runtime == "runsc" {
		data, err := b.transfer(ctx, id, "get", path, nil)
		if err != nil {
			return nil, err
		}
		return io.NopCloser(bytes.NewReader(data)), nil
	}
	return b.api.ArchiveGet(ctx, id, path)
}
func (b *Backend) archivePut(ctx context.Context, id, path string, data []byte) error {
	if g := b.guest(id); g != nil && g.runtime == "runsc" {
		_, err := b.transfer(ctx, id, "put", path, data)
		return err
	}
	return b.api.ArchivePut(ctx, id, path, data)
}
func (b *Backend) transfer(ctx context.Context, id, op, path string, data []byte) ([]byte, error) {
	stream, err := b.api.ExecAttach(ctx, id, dockerapi.ExecConfig{User: "0:0", Cmd: []string{protocol.Binary, "transfer", op, path, strconv.Itoa(len(data))}}, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = stream.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = stream.Close() })
	defer stop()
	if len(data) > 0 {
		if _, err := stream.Write(data); err != nil {
			return nil, err
		}
	}
	kind, raw, err := protocol.Read(stream)
	if err != nil {
		return nil, err
	}
	if kind == 'e' {
		var e protocol.Error
		if err := json.Unmarshal(raw, &e); err != nil {
			return nil, err
		}
		return nil, &daemon.Error{Code: daemon.Code(e.Code), Message: e.Message}
	}
	var size int
	if kind != 'a' || json.Unmarshal(raw, &size) != nil || size < 0 || size > filearchive.MaxBytes {
		return nil, errors.New("invalid guest archive reply")
	}
	result := make([]byte, size)
	_, err = io.ReadFull(stream, result)
	return result, err
}
