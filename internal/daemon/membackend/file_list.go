package membackend

import (
	"archive/tar"
	"context"
	"path"
	"strings"
	"time"

	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
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
	if h, ok := c.archiveHeaders[p]; ok {
		if h.Typeflag != tar.TypeDir {
			return nil, &daemon.Error{Code: daemon.CodeInvalidRequest, Message: "path must identify a directory"}
		}
		exists = true
	}
	for name, h := range c.archiveHeaders {
		rest, ok := strings.CutPrefix(path.Clean(name), prefix)
		if !ok || rest == "" {
			continue
		}
		scanned++
		if scanned > daemon.MaxFileScanEntries {
			return nil, &daemon.Error{Code: daemon.CodePayloadTooLarge, Message: "directory exceeds scan entry limit"}
		}
		exists = true
		child, _, nested := strings.Cut(rest, "/")
		if seen[child] {
			continue
		}
		seen[child] = true
		size := h.Size
		if h.Typeflag == tar.TypeLink {
			size = int64(len(c.files[name]))
		}
		if h.Typeflag == tar.TypeSymlink {
			size = int64(len(h.Linkname))
		}
		entry := daemon.FileEntry{Name: child, Size: size, Mode: h.Mode, Mtime: h.ModTime.UTC(), IsDir: h.Typeflag == tar.TypeDir}
		if nested {
			entry.Size = 0
			entry.Mode = 0o755
			entry.IsDir = true
		}
		entries = daemon.SelectFilePage(entries, entry, after, limit)
	}
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
