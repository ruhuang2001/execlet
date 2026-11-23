package main

import (
	"errors"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/ruhuang2001/tiny-sandbox/internal/guestprotocol"
)

type accounts struct{ users, groups map[int]string }

func newAccounts() accounts                        { return accounts{make(map[int]string), make(map[int]string)} }
func stat(path string) (protocol.FileEntry, error) { return newAccounts().stat(path) }
func (a accounts) stat(path string) (protocol.FileEntry, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return protocol.FileEntry{}, err
	}
	st := info.Sys().(*syscall.Stat_t)
	f := protocol.FileEntry{Name: filepath.Base(path), Mode: int64(st.Mode & 07777), Size: info.Size(), UID: int(st.Uid), GID: int(st.Gid), Mtime: info.ModTime().UTC().Truncate(time.Second), IsDir: info.IsDir(), Type: "other"}
	if _, ok := a.users[f.UID]; !ok {
		name := ""
		if u, err := user.LookupId(strconv.Itoa(f.UID)); err == nil {
			name = u.Username
		}
		a.users[f.UID] = name
	}
	if _, ok := a.groups[f.GID]; !ok {
		name := ""
		if g, err := user.LookupGroupId(strconv.Itoa(f.GID)); err == nil {
			name = g.Name
		}
		a.groups[f.GID] = name
	}
	f.Owner, f.Group = a.users[f.UID], a.groups[f.GID]
	switch {
	case f.IsDir:
		f.Size = 0
		f.Type = "dir"
	case info.Mode().IsRegular():
		f.Type = "file"
	case info.Mode()&os.ModeSymlink != 0:
		f.Type = "symlink"
		target, err := os.Readlink(path)
		if err != nil {
			return f, err
		}
		if utf8.ValidString(target) {
			f.SymlinkTarget = target
		}
	}
	return f, nil
}
func list(path, after string, limit int) ([]protocol.FileEntry, error) {
	if limit < 1 || limit > 1000 {
		return nil, &protocol.Error{Code: "invalid_request", Message: "invalid page limit"}
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.ELOOP) {
		return nil, &protocol.Error{Code: "invalid_request", Message: "path must identify a directory"}
	}
	if err != nil {
		return nil, err
	}
	dir := os.NewFile(uintptr(fd), path)
	defer func() { _ = dir.Close() }()
	names, err := dir.Readdirnames(10001)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(names) > 10000 {
		return nil, &protocol.Error{Code: "payload_too_large", Message: "directory exceeds scan entry limit"}
	}
	slices.Sort(names)
	entries := make([]protocol.FileEntry, 0, min(len(names), limit+1))
	owners := newAccounts()
	for _, name := range names {
		if !utf8.ValidString(name) {
			return nil, &protocol.Error{Code: "invalid_request", Message: "directory contains a non-UTF-8 name"}
		}
		if name <= after || len(entries) > limit {
			continue
		}
		entry, err := owners.stat(filepath.Join("/proc/self/fd", strconv.Itoa(fd), name))
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func writeListFrames(w io.Writer, entries []protocol.FileEntry, more bool) error {
	err := protocol.Write(w, 'l', protocol.ListPage[protocol.FileEntry]{Entries: entries, More: more})
	if !errors.Is(err, protocol.ErrFrameTooLarge) || len(entries) <= 1 {
		return err
	}
	mid := len(entries) / 2
	if err := writeListFrames(w, entries[:mid], true); err != nil {
		return err
	}
	return writeListFrames(w, entries[mid:], more)
}
