package main

import (
	"archive/tar"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/ruhuang2001/execlet/internal/guestprotocol"
)

func TestExtractTarReportsFailedMember(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "occupied"), 0700); err != nil {
		t.Fatal(err)
	}
	var data bytes.Buffer
	w := tar.NewWriter(&data)
	for _, name := range []string{"first", "./occupied", "last"} {
		h := tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0600, Uid: os.Getuid(), Gid: os.Getgid()}
		if err := w.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	member, err := extractTar(dir, &data)
	if err == nil || member != "occupied" {
		t.Fatalf("failed member = %q, err = %v", member, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "first")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "last")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected last member: %v", err)
	}
}

func TestArchiveTransferFailure(t *testing.T) {
	const member = "data/report"
	for _, tc := range []struct {
		err        error
		code, name string
	}{
		{syscall.ENOSPC, protocol.CodeInsufficientStorage, member},
		{syscall.EDQUOT, protocol.CodeInsufficientStorage, member},
		{syscall.EROFS, protocol.CodeReadOnlyFilesystem, member},
		{syscall.EIO, "invalid_request", ""},
		{syscall.ENOENT, "not_found", ""},
	} {
		err := errors.Join(nil, &os.PathError{Op: "close", Path: "temporary-file", Err: tc.err})
		got := transferFailure(member, err)
		if got.Code != tc.code || got.ArchiveMember != tc.name || got.Message != err.Error() {
			t.Errorf("%v: got %+v, want code %q, member %q and original message", err, got, tc.code, tc.name)
		}
	}
}
