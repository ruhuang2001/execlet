package dockerbackend

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
)

const statBatchScript = `if [ -f /etc/passwd ] && [ -r /etc/passwd ]; then cut -d: -f1,3 /etc/passwd 2>/dev/null; fi
printf '\n---\n'
if [ -f /etc/group ] && [ -r /etc/group ]; then cut -d: -f1,3 /etc/group 2>/dev/null; fi
printf '\0'
flush() {
 [ "$#" -eq 0 ] && return 0
 printf 'B\0'
 for item do
  printf '%s\0' "${item##*/}"
 done
 printf '\0'
 stat -c '%f:%s:%Y:%u:%g' "$@" || exit 42
 printf '\0'
 for item do
  if [ -L "$item" ]; then
   target=$(readlink "$item" && printf x) || continue
   printf '%s\0' "${item##*/}"
   printf '%s\0' "${target%?x}"
  fi
 done
 printf '\0'
}
`

const listDirScript = `dir=$1
command -v stat >/dev/null 2>&1 && command -v readlink >/dev/null 2>&1 || exit 45
if [ -L "$dir" ]; then exit 40; fi
if [ ! -e "$dir" ]; then exit 45; fi
if [ ! -d "$dir" ]; then exit 40; fi
[ -r "$dir" ] && [ -x "$dir" ] || exit 45
stat -c '%f:%s:%Y:%u:%g' / >/dev/null 2>&1 || exit 45
prefix=$dir/
[ "$dir" = / ] && prefix=/
` + statBatchScript + `count=0
set --
for f in "$prefix"* "$prefix".[!.]* "$prefix"..?*; do
 [ -e "$f" ] || [ -L "$f" ] || continue
 count=$((count + 1))
 if [ "$count" -gt 10000 ]; then exit 43; fi
 set -- "$@" "$f"
 if [ "$#" -ge 400 ]; then
  flush "$@"
  set --
 fi
done
flush "$@"
`

const writeVerifyScript = `set -e
p=$1
if [ -L "$p" ]; then printf 'oksymlink\0'
elif [ -d "$p" ]; then printf 'okdir\0'
elif [ -f "$p" ]; then printf 'okfile\0'
elif [ -e "$p" ]; then printf 'notfile\0'
else printf 'missing'; exit 0
fi
command -v stat >/dev/null 2>&1 && command -v readlink >/dev/null 2>&1 || exit 45
[ "$p" != / ] || p=/.
` + statBatchScript + `flush "$p"`

func (b *Backend) statWrittenFile(ctx context.Context, id, p string) (daemon.FileEntry, error) {
	out, err := b.captureExec(ctx, id, "0", []string{"/bin/sh", "-c", writeVerifyScript, "stat-file", p}, daemon.MaxFileScanBytes)
	if err != nil {
		if exitErr, ok := errors.AsType[*execExitError](err); ok && (exitErr.code == 45 || exitErr.code == 126 || exitErr.code == 127) {
			return b.statFromArchive(ctx, id, p)
		}
		return daemon.FileEntry{}, fmt.Errorf("dockerbackend: stat %s: %w", p, err)
	}
	if string(out) == "missing" {
		return daemon.FileEntry{}, daemon.ErrNotFound
	}
	_, record, ok := bytes.Cut(out, []byte{0})
	if !ok {
		return daemon.FileEntry{}, errors.New("dockerbackend: incomplete stat response")
	}
	entries, err := parseDirectListOutput(record, "", 1)
	if err != nil {
		return daemon.FileEntry{}, err
	}
	if len(entries) != 1 {
		return daemon.FileEntry{}, errors.New("dockerbackend: expected one stat record")
	}
	entries[0].Name = path.Base(p)
	return entries[0], nil
}

func (b *Backend) statFromArchive(ctx context.Context, id, p string) (daemon.FileEntry, error) {
	rc, err := b.api.ArchiveGet(ctx, id, p)
	if err != nil {
		return daemon.FileEntry{}, notFound(err, "%s", p)
	}
	defer func() { _ = rc.Close() }()
	h, err := tar.NewReader(io.LimitReader(rc, daemon.MaxFileScanBytes)).Next()
	if err != nil {
		return daemon.FileEntry{}, fmt.Errorf("dockerbackend: stat archive: %w", err)
	}
	entry := entryFromHeader(path.Base(p), h)
	return b.resolveOwnership(ctx, id, []daemon.FileEntry{entry})[0], nil
}

func (b *Backend) ListFiles(ctx context.Context, id, p, after string, limit int) ([]daemon.FileEntry, error) {
	out, err := b.captureExec(ctx, id, "", []string{"/bin/sh", "-c", listDirScript, "list-files", p}, daemon.MaxFileScanBytes)
	if err != nil {
		if errors.Is(err, errCaptureLimit) {
			return nil, &daemon.Error{Code: daemon.CodePayloadTooLarge, Message: "directory metadata exceeds scan byte limit"}
		}
		if exitErr, ok := errors.AsType[*execExitError](err); ok {
			switch exitErr.code {
			case 40:
				return nil, &daemon.Error{Code: daemon.CodeInvalidRequest, Message: "path must identify a directory"}
			case 43:
				return nil, &daemon.Error{Code: daemon.CodePayloadTooLarge, Message: "directory exceeds scan entry limit"}
			case 45, 126, 127:
				return b.listFilesFromArchive(ctx, id, p, after, limit)
			}
		}
		return nil, notFound(err, "%s", p)
	}
	return parseDirectListOutput(out, after, limit)
}

func parseDirectListOutput(out []byte, after string, limit int) ([]daemon.FileEntry, error) {
	acctRaw, rest, ok := bytes.Cut(out, []byte{0})
	if !ok {
		return nil, errors.New("dockerbackend: malformed directory listing header")
	}
	users, groups := parseAccountSection(string(acctRaw))
	entries := make([]daemon.FileEntry, 0, limit+1)

	for len(rest) > 0 {
		tag, next, ok := bytes.Cut(rest, []byte{0})
		if !ok || string(tag) != "B" {
			return nil, errors.New("dockerbackend: malformed directory batch marker")
		}
		rest = next

		var names []string
		for {
			rawName, next, ok := bytes.Cut(rest, []byte{0})
			if !ok {
				return nil, errors.New("dockerbackend: truncated directory names")
			}
			rest = next
			if len(rawName) == 0 {
				break
			}
			if !utf8.Valid(rawName) {
				return nil, &daemon.Error{Code: daemon.CodeInvalidRequest, Message: "directory contains a non-UTF-8 name"}
			}
			names = append(names, string(rawName))
		}

		statBlock, next, ok := bytes.Cut(rest, []byte{0})
		if !ok {
			return nil, errors.New("dockerbackend: truncated stat block")
		}
		rest = next
		statLines := strings.Split(strings.TrimSuffix(string(statBlock), "\n"), "\n")
		if len(names) == 0 {
			statLines = nil
		}
		if len(statLines) != len(names) {
			return nil, errors.New("dockerbackend: stat count mismatch")
		}

		symlinks := make(map[string][]byte)
		for {
			linkName, next, ok := bytes.Cut(rest, []byte{0})
			if !ok {
				return nil, errors.New("dockerbackend: truncated symlink block")
			}
			rest = next
			if len(linkName) == 0 {
				break
			}
			target, next, ok := bytes.Cut(rest, []byte{0})
			if !ok {
				return nil, errors.New("dockerbackend: truncated symlink target")
			}
			rest = next
			symlinks[string(linkName)] = target
		}

		for i, name := range names {
			parts := strings.Split(statLines[i], ":")
			if len(parts) != 5 {
				return nil, errors.New("dockerbackend: invalid stat record")
			}
			rawMode, e1 := strconv.ParseInt(parts[0], 16, 64)
			size, e2 := strconv.ParseInt(parts[1], 10, 64)
			mtimeSec, e3 := strconv.ParseInt(parts[2], 10, 64)
			uid, e4 := strconv.Atoi(parts[3])
			gid, e5 := strconv.Atoi(parts[4])
			if errors.Join(e1, e2, e3, e4, e5) != nil {
				return nil, errors.New("dockerbackend: invalid numeric stat field")
			}
			kind := modeFileType(rawMode)
			isDir := kind == daemon.FileTypeDir
			var symTarget string
			if isDir {
				size = 0
			} else if kind == daemon.FileTypeSymlink {
				if rawTarget, ok := symlinks[name]; ok {
					size = int64(len(rawTarget))
					if utf8.Valid(rawTarget) {
						symTarget = string(rawTarget)
					}
				}
			}
			entry := daemon.FileEntry{
				Name:          name,
				Size:          size,
				Mode:          rawMode & 0o7777,
				Mtime:         time.Unix(mtimeSec, 0).UTC(),
				IsDir:         isDir,
				Type:          kind,
				UID:           uid,
				GID:           gid,
				Owner:         users[uid],
				Group:         groups[gid],
				SymlinkTarget: symTarget,
			}
			entries = daemon.SelectFilePage(entries, entry, after, limit)
		}
	}
	return entries, nil
}

func modeFileType(rawMode int64) daemon.FileType {
	switch rawMode & 0xf000 {
	case 0x4000:
		return daemon.FileTypeDir
	case 0xa000:
		return daemon.FileTypeSymlink
	case 0x8000:
		return daemon.FileTypeFile
	default:
		return daemon.FileTypeOther
	}
}

func parseAccountSection(raw string) (users, groups map[int]string) {
	users = make(map[int]string)
	groups = make(map[int]string)
	section := 0
	for line := range strings.SplitSeq(raw, "\n") {
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
	return users, groups
}

func (b *Backend) listFilesFromArchive(ctx context.Context, id, p, after string, limit int) ([]daemon.FileEntry, error) {
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
		entry := entryFromHeader(name, hdr)
		entries = daemon.SelectFilePage(entries, entry, after, limit)
	}
}

func entryFromHeader(name string, hdr *tar.Header) daemon.FileEntry {
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
	if entry.IsDir {
		entry.Size = 0
	}
	if entry.Type == daemon.FileTypeSymlink {
		entry.Size = int64(len(hdr.Linkname))
	}
	return entry
}

func fileType(hdr *tar.Header) daemon.FileType {
	switch hdr.Typeflag {
	case tar.TypeDir:
		return daemon.FileTypeDir
	case tar.TypeSymlink:
		return daemon.FileTypeSymlink
	case tar.TypeReg, 0, tar.TypeLink:
		return daemon.FileTypeFile
	default:
		return daemon.FileTypeOther
	}
}

func symlinkTarget(hdr *tar.Header) string {
	if hdr.Typeflag != tar.TypeSymlink || !utf8.ValidString(hdr.Linkname) {
		return ""
	}
	return hdr.Linkname
}

func (b *Backend) resolveOwnership(ctx context.Context, containerID string, entries []daemon.FileEntry) []daemon.FileEntry {
	if len(entries) == 0 {
		return entries
	}
	out, err := b.captureControl(ctx, containerID, []string{"/bin/sh", "-c",
		`cat /etc/passwd 2>/dev/null | cut -d: -f1,3; echo ---; cat /etc/group 2>/dev/null | cut -d: -f1,3`})
	if err != nil {
		return entries
	}
	users, groups := parseAccountSection(string(out))
	for i := range entries {
		entries[i].Owner = users[entries[i].UID]
		entries[i].Group = groups[entries[i].GID]
	}
	return entries
}

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
