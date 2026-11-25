package membackend

import (
	"archive/tar"
	"bytes"
	"context"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/ruhuang2001/execlet/internal/daemon"
	"github.com/ruhuang2001/execlet/internal/filearchive"
)

func (b *Backend) ReadArchive(_ context.Context, id, p string) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, err := b.get(id)
	if err != nil {
		return nil, err
	}
	headers := map[string]tar.Header{}
	base := path.Base(p)
	if p == "/" {
		base = "."
	}
	if data, ok := c.files[p]; ok {
		headers[p] = tar.Header{Name: base, Typeflag: tar.TypeReg, Size: int64(len(data)), Mode: 0o644}
	}
	prefix := strings.TrimSuffix(p, "/") + "/"
	for name, data := range c.files {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		headers[name] = tar.Header{Name: path.Join(base, strings.TrimPrefix(name, prefix)), Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(data))}
		if len(headers) > filearchive.MaxEntries {
			return nil, &daemon.Error{Code: daemon.CodePayloadTooLarge, Message: "archive exceeds entry limit"}
		}
		for parent := path.Dir(name); parent != p && parent != "/"; parent = path.Dir(parent) {
			if len(headers) >= filearchive.MaxEntries {
				return nil, &daemon.Error{Code: daemon.CodePayloadTooLarge, Message: "archive exceeds entry limit"}
			}
			headers[parent] = tar.Header{Name: path.Join(base, strings.TrimPrefix(parent, prefix)), Typeflag: tar.TypeDir, Mode: 0o755}
		}
	}
	for name, h := range c.archiveHeaders {
		if name == p || strings.HasPrefix(name, prefix) {
			if name == p {
				h.Name = base
			} else {
				h.Name = path.Join(base, strings.TrimPrefix(name, prefix))
			}
			if h.Typeflag == tar.TypeLink {
				target := h.Linkname
				if target == p || strings.HasPrefix(target, prefix) {
					h.Linkname = path.Join(base, strings.TrimPrefix(target, prefix))
					h.Size = 0
				} else {
					h.Typeflag = tar.TypeReg
					h.Linkname = ""
					h.Size = int64(len(c.files[name]))
				}
			}
			headers[name] = h
			if len(headers) > filearchive.MaxEntries {
				return nil, &daemon.Error{Code: daemon.CodePayloadTooLarge, Message: "archive exceeds entry limit"}
			}
		}
	}
	if len(headers) == 0 && p != "/" {
		return nil, daemon.ErrNotFound
	}
	if _, ok := headers[p]; !ok {
		headers[p] = tar.Header{Name: base, Typeflag: tar.TypeDir, Mode: 0o755}
	}
	if len(headers) > filearchive.MaxEntries {
		return nil, &daemon.Error{Code: daemon.CodePayloadTooLarge, Message: "archive exceeds entry limit"}
	}
	total := int64(0)
	for _, h := range headers {
		total += h.Size
	}
	if total > filearchive.MaxExtractedBytes {
		return nil, &daemon.Error{Code: daemon.CodePayloadTooLarge, Message: "archive exceeds payload limit"}
	}
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	slices.SortFunc(names, func(a, b string) int {
		al, bl := headers[a].Typeflag == tar.TypeLink, headers[b].Typeflag == tar.TypeLink
		if al != bl {
			if al {
				return 1
			}
			return -1
		}
		return strings.Compare(a, b)
	})
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	for _, name := range names {
		h := headers[name]
		if h.ModTime.IsZero() {
			h.ModTime = time.Unix(0, 0)
		}
		if err := w.WriteHeader(&h); err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := w.Write(c.files[name]); err != nil {
				return nil, err
			}
		}
		if buf.Len() > filearchive.MaxBytes {
			return nil, &daemon.Error{Code: daemon.CodePayloadTooLarge, Message: "archive exceeds encoded limit"}
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	if buf.Len() > filearchive.MaxBytes {
		return nil, &daemon.Error{Code: daemon.CodePayloadTooLarge, Message: "archive exceeds encoded limit"}
	}
	return buf.Bytes(), nil
}

func (b *Backend) WriteArchive(_ context.Context, id, p string, archive *filearchive.Archive) error {
	if archive == nil || archive.Len() == 0 {
		return &daemon.Error{Code: daemon.CodeInvalidRequest, Message: "archive must be constructed by filearchive.Parse"}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	c, err := b.get(id)
	if err != nil {
		return err
	}
	if _, ok := c.files[p]; ok {
		return &daemon.Error{Code: daemon.CodeInvalidRequest, Message: "archive destination must be a directory"}
	}
	exists := p == "/"
	prefix := strings.TrimSuffix(p, "/") + "/"
	for name := range c.files {
		if strings.HasPrefix(name, prefix) {
			exists = true
			break
		}
	}
	for name := range c.archiveHeaders {
		if strings.HasPrefix(name, prefix) {
			exists = true
			break
		}
	}
	if h, ok := c.archiveHeaders[p]; ok && h.Typeflag == tar.TypeDir {
		exists = true
	}
	if !exists {
		return daemon.ErrNotFound
	}
	for _, entry := range archive.Entries() {
		target := path.Join(p, entry.Header.Name)
		if entry.Header.Typeflag != tar.TypeDir {
			if h, ok := c.archiveHeaders[target]; ok && h.Typeflag == tar.TypeDir {
				return &daemon.Error{Code: daemon.CodeInvalidRequest, Message: "archive member would replace a directory"}
			}
			for name := range c.files {
				if strings.HasPrefix(name, target+"/") {
					return &daemon.Error{Code: daemon.CodeInvalidRequest, Message: "archive member would replace a directory"}
				}
			}
			for name := range c.archiveHeaders {
				if strings.HasPrefix(name, target+"/") {
					return &daemon.Error{Code: daemon.CodeInvalidRequest, Message: "archive member would replace a directory"}
				}
			}
		}
		for current := path.Join(p, entry.Header.Name); current != "/"; current = path.Dir(current) {
			if current != path.Join(p, entry.Header.Name) {
				if _, ok := c.files[current]; ok {
					return &daemon.Error{Code: daemon.CodeInvalidRequest, Message: "archive ancestor is not a directory"}
				}
			}
			if h, ok := c.archiveHeaders[current]; ok && h.Typeflag == tar.TypeSymlink {
				return &daemon.Error{Code: daemon.CodeInvalidRequest, Message: "archive destination contains a symlink"}
			}
		}
	}
	if c.archiveHeaders == nil {
		c.archiveHeaders = make(map[string]tar.Header)
	}
	for _, entry := range archive.Entries() {
		h := entry.Header
		name := path.Join(p, h.Name)
		c.detachHardlinks(name)
		stored := h
		if h.Typeflag == tar.TypeLink {
			stored.Linkname = path.Join(p, h.Linkname)
		}
		c.archiveHeaders[name] = stored
		switch h.Typeflag {
		case tar.TypeReg:
			c.files[name] = bytes.Clone(entry.Data)
		case tar.TypeLink:
			c.files[name] = bytes.Clone(c.files[path.Join(p, h.Linkname)])
		default:
			delete(c.files, name)
		}
	}
	return nil
}

func (c *container) detachHardlinks(target string) {
	for name, h := range c.archiveHeaders {
		if h.Typeflag == tar.TypeLink && h.Linkname == target {
			h.Typeflag = tar.TypeReg
			h.Linkname = ""
			h.Size = int64(len(c.files[name]))
			c.archiveHeaders[name] = h
		}
	}
}
