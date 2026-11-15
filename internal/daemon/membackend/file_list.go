package membackend

import (
	"context"
	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
	"path"
	"strings"
	"time"
)

// ListFiles derives directories from stored file paths.
func (b *Backend) ListFiles(_ context.Context, id, p, after string, limit int) ([]daemon.FileEntry, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, err := b.get(id)
	if err != nil {
		return nil, err
	}
	if _, ok := c.files[p]; ok {
		return nil, &daemon.Error{Code: daemon.CodeInvalidRequest, Message: "path must identify a directory"}
	}
	prefix := strings.TrimSuffix(path.Clean(p), "/") + "/"
	entries := make([]daemon.FileEntry, 0, limit+1)
	seen := make(map[string]bool)
	exists := p == "/"
	scanned := 0
	for name, data := range c.files {
		rest, ok := strings.CutPrefix(path.Clean(name), prefix)
		if !ok {
			continue
		}
		scanned++
		if scanned > daemon.MaxFileScanEntries {
			return nil, &daemon.Error{Code: daemon.CodePayloadTooLarge, Message: "directory exceeds scan entry limit"}
		}
		exists = true
		child, _, directory := strings.Cut(rest, "/")
		if seen[child] {
			continue
		}
		seen[child] = true
		size, mode := int64(len(data)), int64(0o644)
		if directory {
			size, mode = 0, 0o755
		}
		entries = daemon.SelectFilePage(entries, daemon.FileEntry{Name: child, Size: size, Mode: mode, Mtime: time.Unix(0, 0).UTC(), IsDir: directory}, after, limit)
	}
	if !exists {
		return nil, daemon.ErrNotFound
	}
	return entries, nil
}
