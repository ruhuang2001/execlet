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

const MaxArchiveBytes = 10 << 20

func BuildFileTar(name string, data []byte) ([]byte, error) {
	return BuildOwnedFileTar(name, data, 0, 0, nil)
}

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

var (
	ErrNotRegular   = errors.New("dockerapi: path must identify a regular file")
	ErrFileTooLarge = errors.New("dockerapi: regular file exceeds the download limit")
)

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
	return io.ReadAll(tr)
}
