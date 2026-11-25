package filearchive

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	MaxBytes          = 32 << 20
	MaxExtractedBytes = 16 << 20
	MaxEntries        = 4096
	MaxNameBytes      = 4096
	MaxPathBytes      = 1 << 20
)

type Error struct {
	Message  string
	TooLarge bool
}

func (e *Error) Error() string                 { return e.Message }
func invalid(format string, args ...any) error { return &Error{Message: fmt.Sprintf(format, args...)} }
func large(message string) error               { return &Error{Message: message, TooLarge: true} }

type Entry struct {
	Header tar.Header
	Data   []byte
}

type Archive struct {
	entries []Entry
	data    []byte
}

func Parse(data []byte) (*Archive, error) {
	if len(data) > MaxBytes {
		return nil, large("archive exceeds 32 MiB encoded limit")
	}
	if len(data) < 1024 || len(data)%512 != 0 || !allZero(data[len(data)-1024:]) {
		return nil, invalid("tar must end in two zero blocks and contain complete blocks")
	}
	source := bytes.NewReader(data)
	reader := tar.NewReader(source)
	entries := []Entry{}
	names := map[string]byte{}
	total := int64(0)
	pathBytes := 0
	for {
		before := source.Len()
		h, err := reader.Next()
		if errors.Is(err, io.EOF) {
			if before-source.Len() < 1024 {
				return nil, invalid("tar is missing its complete end marker")
			}
			break
		}
		if err != nil {
			return nil, invalid("invalid tar header: %v", err)
		}
		if len(entries) >= MaxEntries {
			return nil, large("archive exceeds 4096 entries")
		}
		pathBytes += len(h.Name) + len(h.Linkname)
		if pathBytes > MaxPathBytes {
			return nil, large("archive exceeds 1 MiB aggregate path bytes")
		}
		name, err := validName(h.Name)
		if err != nil {
			return nil, err
		}
		if _, exists := names[name]; exists {
			return nil, invalid("duplicate archive name %q", name)
		}
		if err := validHeader(h); err != nil {
			return nil, err
		}
		if h.Typeflag == 0 {
			h.Typeflag = tar.TypeReg
		}
		if name == "." && h.Typeflag != tar.TypeDir {
			return nil, invalid("archive root must be a directory")
		}
		if h.Typeflag != tar.TypeReg && h.Size != 0 {
			return nil, invalid("nonregular archive entry has payload")
		}
		if h.Size < 0 || h.Size > MaxExtractedBytes-total {
			return nil, large("archive exceeds 16 MiB total extracted bytes")
		}
		total += h.Size
		payload, err := io.ReadAll(reader)
		if err != nil {
			return nil, invalid("truncated tar payload: %v", err)
		}
		clean := tar.Header{Name: name, Typeflag: h.Typeflag, Mode: h.Mode & 0o777, Size: h.Size, ModTime: h.ModTime, Linkname: h.Linkname, Format: tar.FormatPAX}
		if h.Typeflag == tar.TypeLink || h.Typeflag == tar.TypeSymlink {
			target, err := validName(h.Linkname)
			if err != nil {
				return nil, invalid("invalid link target for %q: %v", name, err)
			}
			clean.Linkname = target
			if h.Typeflag == tar.TypeLink && names[target] != tar.TypeReg {
				return nil, invalid("hardlink %q must target an earlier regular member", name)
			}
		}
		names[name] = clean.Typeflag
		entries = append(entries, Entry{Header: clean, Data: payload})
	}
	if !allZero(data[len(data)-source.Len():]) {
		return nil, invalid("nonzero data after tar end")
	}
	if err := validateTree(entries, names); err != nil {
		return nil, err
	}
	encoded, err := encode(entries)
	if err != nil {
		return nil, err
	}
	return &Archive{entries: entries, data: encoded}, nil
}
func validName(name string) (string, error) {
	if name == "" || path.IsAbs(name) || len(name) > MaxNameBytes || strings.ContainsAny(name, "\\\x00") || !utf8.ValidString(name) {
		return "", invalid("archive name must be a relative UTF-8 path of at most 4096 bytes")
	}
	for part := range strings.SplitSeq(name, "/") {
		if part == ".." {
			return "", invalid("archive path contains parent traversal")
		}
	}
	return path.Clean(name), nil
}
func validHeader(h *tar.Header) error {
	switch h.Typeflag {
	case tar.TypeReg, 0, tar.TypeDir, tar.TypeLink, tar.TypeSymlink:
	default:
		return invalid("unsupported tar member type %d", h.Typeflag)
	}
	for key := range h.PAXRecords {
		switch key {
		case "path", "linkpath", "mtime", "atime", "ctime", "size", "uid", "gid", "uname", "gname":
		default:
			return invalid("unsupported tar metadata %q", key)
		}
	}
	return nil
}
func validateTree(entries []Entry, names map[string]byte) error {
	for _, entry := range entries {
		h := entry.Header
		for parent := path.Dir(h.Name); parent != "."; parent = path.Dir(parent) {
			if typ, ok := names[parent]; ok && typ != tar.TypeDir {
				return invalid("archive ancestor %q is not a directory", parent)
			}
		}
		if h.Typeflag == tar.TypeSymlink {
			target := path.Join(path.Dir(h.Name), h.Linkname)
			typ, ok := names[target]
			if !ok || (typ != tar.TypeReg && typ != tar.TypeDir) {
				return invalid("symlink %q must target a regular file or directory member", h.Name)
			}
		}
	}
	return nil
}
func encode(entries []Entry) ([]byte, error) {
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	for _, entry := range entries {
		h := entry.Header
		if err := w.WriteHeader(&h); err != nil {
			return nil, invalid("encode tar header: %v", err)
		}
		if _, err := w.Write(entry.Data); err != nil {
			return nil, invalid("encode tar payload: %v", err)
		}
		if buf.Len() > MaxBytes {
			return nil, large("canonical archive exceeds 32 MiB")
		}
	}
	if err := w.Close(); err != nil {
		return nil, invalid("finish tar: %v", err)
	}
	if buf.Len() > MaxBytes {
		return nil, large("canonical archive exceeds 32 MiB")
	}
	return buf.Bytes(), nil
}
func allZero(data []byte) bool {
	for _, b := range data {
		if b != 0 {
			return false
		}
	}
	return true
}

func (a *Archive) Entries() []Entry {
	result := make([]Entry, len(a.entries))
	for i, entry := range a.entries {
		result[i] = Entry{Header: entry.Header, Data: bytes.Clone(entry.Data)}
	}
	return result
}

func (a *Archive) HasMember(name string) bool {
	return slices.ContainsFunc(a.entries, func(e Entry) bool { return e.Header.Name == name })
}

func (a *Archive) BytesOwnedBy(uid, gid int) ([]byte, error) {
	if uid == 0 && gid == 0 {
		return a.Bytes(), nil
	}
	entries := slices.Clone(a.entries)
	for i := range entries {
		entries[i].Header.Uid = uid
		entries[i].Header.Gid = gid
	}
	return encode(entries)
}

func (a *Archive) Bytes() []byte { return bytes.Clone(a.data) }

func (a *Archive) Len() int { return len(a.data) }
