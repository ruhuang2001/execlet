package dockerbackend

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
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
	sizes := make(map[string]int64)
	links := make(map[string]string)
	for count := 0; ; count++ {
		hdr, err := tr.Next()
		if bounded.N <= 0 {
			return nil, &daemon.Error{Code: daemon.CodePayloadTooLarge, Message: "directory archive exceeds scan byte limit"}
		}
		if errors.Is(err, io.EOF) {
			entries, err = resolveListingSizes(entries, root, sizes, links)
			if err != nil {
				return nil, err
			}
			return b.resolveOwnership(ctx, id, entries), nil
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
		archiveName := path.Clean(hdr.Name)
		if hdr.Typeflag == tar.TypeLink {
			links[archiveName] = path.Clean(hdr.Linkname)
		} else {
			size := hdr.Size
			if hdr.Typeflag == tar.TypeSymlink {
				size = int64(len(hdr.Linkname))
			}
			sizes[archiveName] = size
		}
		name := strings.TrimSuffix(hdr.Name, "/")
		name, ok := strings.CutPrefix(name, root)
		if !ok || name == "" || strings.Contains(name, "/") {
			continue
		}
		if !utf8.ValidString(name) {
			return nil, &daemon.Error{Code: daemon.CodeInvalidRequest, Message: "directory contains a non-UTF-8 name"}
		}
		entry := daemon.FileEntry{
			Name:          name,
			Size:          hdr.Size,
			Mode:          hdr.Mode & 0o7777,
			Mtime:         hdr.ModTime.UTC(),
			IsDir:         hdr.FileInfo().IsDir(),
			Type:          fileType(hdr),
			UID:           hdr.Uid,
			GID:           hdr.Gid,
			SymlinkTarget: symlinkTarget(hdr),
		}
		entries = daemon.SelectFilePage(entries, entry, after, limit)
	}
}

// fileType maps a tar header to the contract's kind. A hardlink is a file
// because it is another name for one, not a separate object kind.
func fileType(hdr *tar.Header) daemon.FileType {
	switch hdr.Typeflag {
	case tar.TypeDir:
		return daemon.FileTypeDir
	case tar.TypeSymlink:
		return daemon.FileTypeSymlink
	case tar.TypeReg, 0, tar.TypeLink: // Zero is the legacy regular-file flag.
		return daemon.FileTypeFile
	default:
		return daemon.FileTypeOther
	}
}

// symlinkTarget is only set for a symlink, and only when the target is valid
// UTF-8: a listing must not return bytes that cannot be encoded in JSON.
func symlinkTarget(hdr *tar.Header) string {
	if hdr.Typeflag != tar.TypeSymlink || !utf8.ValidString(hdr.Linkname) {
		return ""
	}
	return hdr.Linkname
}

// resolveOwnership names the uid/gid of each entry from the container's account
// database. Docker's archive carries only numeric ids, so a name has to be read
// from the guest; when that is unavailable the ids stay reported and the names
// stay empty, because a guess would be indistinguishable from a real answer.
func (b *Backend) resolveOwnership(ctx context.Context, containerID string, entries []daemon.FileEntry) []daemon.FileEntry {
	if len(entries) == 0 {
		return entries
	}
	users := map[int]string{}
	groups := map[int]string{}
	// One exec reads both files. A guest without them is not an error: the ids
	// are the contract's required fields.
	out, err := b.captureAs(ctx, containerID, "", []string{"/bin/sh", "-c",
		`cat /etc/passwd 2>/dev/null | cut -d: -f1,3; echo ---; cat /etc/group 2>/dev/null | cut -d: -f1,3`})
	if err != nil {
		return entries
	}
	section := 0
	for line := range strings.SplitSeq(string(out), "\n") {
		if line == "---" {
			section = 1
			continue
		}
		name, id, ok := strings.Cut(line, ":")
		if !ok || name == "" {
			continue
		}
		n, err := strconv.Atoi(id)
		if err != nil {
			continue
		}
		if section == 0 {
			users[n] = name
		} else {
			groups[n] = name
		}
	}
	for i := range entries {
		entries[i].Owner = users[entries[i].UID]
		entries[i].Group = groups[entries[i].GID]
	}
	return entries
}

// Docker tar hardlinks have no payload size; their target may be a nested entry.
func resolveListingSizes(entries []daemon.FileEntry, root string, sizes map[string]int64, links map[string]string) ([]daemon.FileEntry, error) {
	for i := range entries {
		name := path.Clean(root + entries[i].Name)
		for steps := 0; ; steps++ {
			if size, ok := sizes[name]; ok {
				entries[i].Size = size
				break
			}
			target, ok := links[name]
			if !ok || steps >= len(links) {
				return nil, fmt.Errorf("dockerbackend: unresolved hardlink metadata for %q", entries[i].Name)
			}
			name = target
		}
	}
	return entries, nil
}
