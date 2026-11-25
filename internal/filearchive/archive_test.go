package filearchive_test

import (
	"archive/tar"
	"bytes"
	"errors"
	"testing"

	"github.com/ruhuang2001/execlet/internal/filearchive"
)

func buildValidTar(t testing.TB) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "dir/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "dir/file.txt", Typeflag: tar.TypeReg, Mode: 0o640, Size: 5}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "dir/link", Typeflag: tar.TypeSymlink, Linkname: "file.txt"}); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "dir/hard", Typeflag: tar.TypeLink, Linkname: "dir/file.txt"}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestParseRejectsUnsafeArchives(t *testing.T) {
	valid := buildValidTar(t)
	parsed, err := filearchive.Parse(valid)
	if err != nil || len(parsed.Entries()) != 4 {
		t.Fatalf("Parse valid = %v, %v", parsed, err)
	}

	for _, hdr := range []tar.Header{
		{Name: "/abs.txt", Typeflag: tar.TypeReg},
		{Name: "a/../b.txt", Typeflag: tar.TypeReg},
		{Name: "bad\\slash", Typeflag: tar.TypeReg},
		{Name: "fifo", Typeflag: tar.TypeFifo},
		{Name: "dangling", Typeflag: tar.TypeSymlink, Linkname: "missing"},
		{Name: "escape", Typeflag: tar.TypeSymlink, Linkname: "../outside"},
	} {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		_ = tw.WriteHeader(&hdr)
		_ = tw.Close()
		if _, err := filearchive.Parse(buf.Bytes()); err == nil {
			t.Errorf("Parse(%+v) succeeded, want error", hdr)
		}
	}
}

func FuzzParse(f *testing.F) {
	f.Add(buildValidTar(f))
	f.Add(make([]byte, 1024))
	f.Add([]byte("not a tar"))

	f.Fuzz(func(t *testing.T, data []byte) {
		archive, err := filearchive.Parse(data)
		if err != nil {
			var ae *filearchive.Error
			if !errors.As(err, &ae) {
				t.Fatalf("Parse returned non-typed error: %T %v", err, err)
			}
			return
		}
		reparsed, err := filearchive.Parse(archive.Bytes())
		if err != nil {
			t.Fatalf("canonical archive failed to re-parse: %v", err)
		}
		if len(reparsed.Entries()) != len(archive.Entries()) {
			t.Fatalf("re-parsed entries = %d, want %d", len(reparsed.Entries()), len(archive.Entries()))
		}
	})
}
