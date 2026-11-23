package main

import (
	"archive/tar"
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/ruhuang2001/tiny-sandbox/internal/filearchive"
	"github.com/ruhuang2001/tiny-sandbox/internal/guestprotocol"
	"golang.org/x/sys/unix"
)

func transfer(args []string) error {
	if len(args) != 3 {
		return errors.New("invalid transfer arguments")
	}
	size, err := strconv.Atoi(args[2])
	if err != nil || size < 0 || size > filearchive.MaxBytes {
		return errors.New("invalid transfer size")
	}
	switch args[0] {
	case "put":
		err = extractTar(args[1], io.LimitReader(os.Stdin, int64(size)))
	case "get":
		var body []byte
		body, err = packTar(args[1])
		if err == nil {
			if err = protocol.Write(os.Stdout, 'a', len(body)); err != nil {
				return err
			}
			_, err = os.Stdout.Write(body)
			return err
		}
	default:
		return errors.New("invalid transfer operation")
	}
	if err != nil {
		code := "invalid_request"
		if errors.Is(err, os.ErrNotExist) {
			code = "not_found"
		}
		if e, ok := errors.AsType[*protocol.Error](err); ok {
			code = e.Code
		}
		return protocol.Write(os.Stdout, 'e', protocol.Error{Code: code, Message: err.Error()})
	}
	return protocol.Write(os.Stdout, 'a', 0)
}

func extractTar(dir string, src io.Reader) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	r := tar.NewReader(src)
	var directories []*tar.Header
	for {
		h, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(h.Name)
		if !filepath.IsLocal(name) {
			return errors.New("nonlocal archive path")
		}
		if err := root.MkdirAll(filepath.Dir(name), 0755); err != nil {
			return err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err = root.MkdirAll(name, 0700); err != nil {
				return err
			}
			h.Name = name
			directories = append(directories, h)
		case tar.TypeReg:
			if err := replaceRegular(root, name, h, r); err != nil {
				return err
			}
			continue
		case tar.TypeSymlink, tar.TypeLink:
			if err = root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if h.Typeflag == tar.TypeSymlink {
				err = restoreSymlink(root, name, h)
			} else {
				err = root.Link(h.Linkname, name)
			}
			if err != nil {
				return err
			}
		default:
			return errors.New("unsupported archive member")
		}
	}
	slices.SortFunc(directories, func(a, b *tar.Header) int { return strings.Compare(b.Name, a.Name) })
	for _, h := range directories {
		fd, err := root.OpenFile(h.Name, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		if err := errors.Join(setMetadata(fd, h), fd.Close()); err != nil {
			return err
		}
	}
	return nil
}
func restoreSymlink(root *os.Root, name string, h *tar.Header) error {
	parent, err := root.OpenFile(filepath.Dir(name), unix.O_PATH|unix.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()
	fd, base := int(parent.Fd()), filepath.Base(name)
	if err := unix.Symlinkat(h.Linkname, fd, base); err != nil {
		return err
	}
	t, err := unix.TimeToTimespec(h.ModTime)
	if err != nil {
		return err
	}
	return unix.UtimesNanoAt(fd, base, []unix.Timespec{t, t}, unix.AT_SYMLINK_NOFOLLOW)
}
func replaceRegular(root *os.Root, name string, h *tar.Header, r io.Reader) error {
	if f, err := root.Lstat(name); err == nil && f.IsDir() {
		return errors.New("file destination is a directory")
	}
	parent, err := root.OpenRoot(filepath.Dir(name))
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()
	temp := ".tiny-upload-" + rand.Text()
	f, err := parent.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = parent.Remove(temp) }()
	_, writeErr := io.CopyN(f, r, h.Size)
	if writeErr == nil {
		writeErr = setMetadata(f, h)
	}
	if err := errors.Join(writeErr, f.Close()); err != nil {
		return err
	}
	return parent.Rename(temp, filepath.Base(name))
}
func packTar(path string) ([]byte, error) {
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	var count int
	var payload int64
	links := map[[2]uint64]string{}
	rootName := filepath.Base(path)
	if rootName == "/" {
		rootName = "."
	}
	err := filepath.WalkDir(path, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		count++
		if count > filearchive.MaxEntries {
			return &protocol.Error{Code: "payload_too_large", Message: "archive entry limit exceeded"}
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		var target string
		if info.Mode()&os.ModeSymlink != 0 {
			target, err = os.Readlink(p)
			if err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(info, target)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(path, p)
		if err != nil {
			return err
		}
		hdr.Name = filepath.Join(rootName, rel)
		st := info.Sys().(*syscall.Stat_t)
		hdr.Uid, hdr.Gid = int(st.Uid), int(st.Gid)
		if info.Mode().IsRegular() && st.Nlink > 1 {
			key := [2]uint64{st.Dev, st.Ino}
			if first, ok := links[key]; ok {
				hdr.Typeflag, hdr.Linkname, hdr.Size = tar.TypeLink, first, 0
			} else {
				links[key] = hdr.Name
			}
		}
		payload += hdr.Size
		if payload > filearchive.MaxExtractedBytes || buf.Len() > filearchive.MaxBytes {
			return &protocol.Error{Code: "payload_too_large", Message: "archive byte limit exceeded"}
		}
		if err = w.WriteHeader(hdr); err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg {
			return nil
		}
		f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		_, err = io.CopyN(w, f, hdr.Size)
		return err
	})
	if err != nil {
		return nil, err
	}
	if err = w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func setMetadata(f *os.File, h *tar.Header) error {
	if err := f.Chown(h.Uid, h.Gid); err != nil {
		return err
	}
	if err := f.Chmod(os.FileMode(h.Mode) & 0777); err != nil {
		return err
	}
	t := syscall.NsecToTimeval(h.ModTime.UnixNano())
	return syscall.Futimes(int(f.Fd()), []syscall.Timeval{t, t})
}
