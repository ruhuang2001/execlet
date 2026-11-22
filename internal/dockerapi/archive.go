package dockerapi

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"
)

// MaxArchiveBytes bounds one archived file, matching the daemon's file limit.
const MaxArchiveBytes = 10 << 20

// BuildFileTar returns a tar archive holding data as a single regular file.
// name is the path inside the archive and must be relative: the archive is
// extracted at "/" in the container, so a leading separator would be doubled.
func BuildFileTar(name string, data []byte) ([]byte, error) {
	return BuildOwnedFileTar(name, data, 0, 0, nil)
}

// BuildOwnedFileTar is BuildFileTar for a container whose default user is not
// root. Docker extracts as root, so a missing parent directory would be created
// root-owned and a non-root guest could not write beside the file afterwards;
// dirs lists those missing ancestors, shallowest first, and they are written as
// explicit directory entries that carry the same owner. An ancestor that already
// exists must not be listed: its mode and owner belong to the container.
func BuildOwnedFileTar(name string, data []byte, uid, gid int, dirs []string) ([]byte, error) {
	name = path.Clean(name)
	if name == "." || name == "/" || path.IsAbs(name) ||
		name == ".." || strings.HasPrefix(name, "../") {
		return nil, fmt.Errorf("dockerapi: archive name %q must be a relative file path inside the container", name)
	}
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	modTime := time.Unix(0, 0)
	for _, dir := range dirs {
		dir = path.Clean(dir)
		if dir == "." || dir == "/" || path.IsAbs(dir) || dir == ".." || strings.HasPrefix(dir, "../") {
			return nil, fmt.Errorf("dockerapi: archive directory %q must be a relative path inside the container", dir)
		}
		hdr := &tar.Header{
			Typeflag: tar.TypeDir,
			Name:     dir + "/",
			Mode:     0o755,
			Uid:      uid,
			Gid:      gid,
			ModTime:  modTime,
		}
		if err := w.WriteHeader(hdr); err != nil {
			return nil, fmt.Errorf("dockerapi: write tar header: %w", err)
		}
	}
	hdr := &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     name,
		Mode:     0o644,
		Size:     int64(len(data)),
		Uid:      uid,
		Gid:      gid,
		ModTime:  modTime,
	}
	if err := w.WriteHeader(hdr); err != nil {
		return nil, fmt.Errorf("dockerapi: write tar header: %w", err)
	}
	if _, err := w.Write(data); err != nil {
		return nil, fmt.Errorf("dockerapi: write tar body: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("dockerapi: close tar: %w", err)
	}
	return buf.Bytes(), nil
}

// ErrNotRegular and ErrFileTooLarge classify errors attributable to the requested path.
var (
	ErrNotRegular   = errors.New("dockerapi: path must identify a regular file")
	ErrFileTooLarge = errors.New("dockerapi: regular file exceeds the download limit")
)

// ExtractFile reads only the requested root entry. Never search a directory
// archive for a child file or follow a link as a substitute for its contents.
func ExtractFile(r io.Reader) ([]byte, error) {
	tr := tar.NewReader(r)
	hdr, err := tr.Next()
	if errors.Is(err, io.EOF) {
		return nil, errors.New("dockerapi: archive contains no file")
	}
	if err != nil {
		return nil, fmt.Errorf("dockerapi: read archive: %w", err)
	}
	if hdr.Typeflag != tar.TypeReg {
		return nil, ErrNotRegular
	}
	if hdr.Size > MaxArchiveBytes {
		return nil, ErrFileTooLarge
	}
	return io.ReadAll(tr) // tar.Reader stops at the checked header's size
}
