package dockerbackend

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
)

// ListFiles scans a bounded Docker archive, retaining only the requested page.
func (b *Backend) ListFiles(ctx context.Context, id, p, after string, limit int) ([]daemon.FileEntry, error) {
	rc, err := b.api.ArchiveGet(ctx, id, p)
	if err != nil {
		return nil, notFound(err, "%s", p)
	}
	defer func() { _ = rc.Close() }()
	bounded := &io.LimitedReader{R: rc, N: daemon.MaxFileScanBytes + 1}
	tr := tar.NewReader(bounded)
	entries := make([]daemon.FileEntry, 0, limit+1)
	root := ""
	for count := 0; ; count++ {
		hdr, err := tr.Next()
		if bounded.N <= 0 {
			return nil, &daemon.Error{Code: daemon.CodePayloadTooLarge, Message: "directory archive exceeds scan byte limit"}
		}
		if errors.Is(err, io.EOF) {
			return entries, nil
		}
		if err != nil {
			return nil, fmt.Errorf("dockerbackend: list archive: %w", err)
		}
		if count >= daemon.MaxFileScanEntries {
			return nil, &daemon.Error{Code: daemon.CodePayloadTooLarge, Message: "directory archive exceeds scan entry limit"}
		}
		if count == 0 {
			if !hdr.FileInfo().IsDir() {
				return nil, &daemon.Error{Code: daemon.CodeInvalidRequest, Message: "path must identify a directory"}
			}
			root = strings.TrimSuffix(path.Clean(hdr.Name), "/") + "/"
			if root == "./" {
				root = ""
			}
			continue
		}
		name := strings.TrimSuffix(hdr.Name, "/")
		name, ok := strings.CutPrefix(name, root)
		if !ok || name == "" || strings.Contains(name, "/") {
			continue
		}
		if !utf8.ValidString(name) {
			return nil, &daemon.Error{Code: daemon.CodeInvalidRequest, Message: "directory contains a non-UTF-8 name"}
		}
		entries = daemon.SelectFilePage(entries, daemon.FileEntry{Name: name, Size: hdr.Size, Mode: hdr.Mode & 0o7777, Mtime: hdr.ModTime.UTC(), IsDir: hdr.FileInfo().IsDir()}, after, limit)
	}
}
