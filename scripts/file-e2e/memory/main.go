// Command memory verifies archive regression scenarios through HTTP and the Go SDK.
package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"time"

	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
	"github.com/ruhuang2001/tiny-sandbox/internal/daemon/membackend"
	sandbox "github.com/ruhuang2001/tiny-sandbox/sandbox-go"
)

func main() {
	ctx := context.Background()
	server := httptest.NewServer(daemon.NewHandler(daemon.New(membackend.New()), "memory-e2e"))
	defer server.Close()
	c, err := sandbox.NewClient(sandbox.Config{Domain: server.URL})
	must(err)
	sb, err := c.CreateSandbox(ctx, sandbox.CreateSandboxRequest{})
	must(err)
	var input bytes.Buffer
	w := tar.NewWriter(&input)
	mtime := time.Unix(1700000000, 0).UTC()
	for _, h := range []tar.Header{
		{Name: "tree", Typeflag: tar.TypeDir, Mode: 0o700, ModTime: mtime},
		{Name: "tree/zdata", Typeflag: tar.TypeReg, Mode: 0o640, ModTime: mtime.Add(time.Second), Size: 9 << 20},
		{Name: "tree/alink", Typeflag: tar.TypeLink, Linkname: "tree/zdata", Mode: 0o640, ModTime: mtime.Add(time.Second)},
	} {
		must(w.WriteHeader(&h))
		if h.Size > 0 {
			_, err = w.Write(bytes.Repeat([]byte("x"), int(h.Size)))
			must(err)
		}
	}
	must(w.Close())
	must(c.WriteArchive(ctx, sb.ID, "/", input.Bytes()))
	data, err := c.ReadArchive(ctx, sb.ID, "/tree")
	must(err)
	must(os.WriteFile(filepath.Join(os.Args[1], "memory-roundtrip.tar"), data, 0o600))
	reader := tar.NewReader(bytes.NewReader(data))
	regular, links, total := 0, 0, int64(0)
	seen := map[string]bool{}
	for {
		h, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		must(err)
		if h.Typeflag == tar.TypeReg {
			regular++
			total += h.Size
		}
		if h.Typeflag == tar.TypeLink {
			links++
			if !seen[h.Linkname] {
				panic("link target not earlier")
			}
		}
		seen[h.Name] = true
	}
	if regular != 1 || links != 1 || total != 9<<20 {
		panic(fmt.Sprintf("regular=%d links=%d total=%d", regular, links, total))
	}
	for range 1000 {
		page, err := c.ListFiles(ctx, sb.ID, "/", sandbox.ListFilesOptions{})
		must(err)
		if len(page.Entries) != 1 || page.Entries[0].Mode != 0o700 || !page.Entries[0].Mtime.Equal(mtime) {
			panic(fmt.Sprintf("metadata: %+v", page))
		}
	}
	single, err := c.ReadArchive(ctx, sb.ID, "/tree/alink")
	must(err)
	r := tar.NewReader(bytes.NewReader(single))
	h, err := r.Next()
	must(err)
	if h.Typeflag != tar.TypeReg || h.Size != 9<<20 {
		panic("outside-subtree link not materialized")
	}
	materialized, err := io.ReadAll(r)
	must(err)
	if !bytes.Equal(materialized, bytes.Repeat([]byte("x"), 9<<20)) {
		panic("materialized hardlink payload mismatch")
	}
	var conflict bytes.Buffer
	cw := tar.NewWriter(&conflict)
	must(cw.WriteHeader(&tar.Header{Name: "tree", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1}))
	_, err = cw.Write([]byte("x"))
	must(err)
	must(cw.Close())
	conflictErr := c.WriteArchive(ctx, sb.ID, "/", conflict.Bytes())
	if typed, ok := errors.AsType[*sandbox.Error](conflictErr); !ok || typed.Code != "invalid_request" || typed.StatusCode != 400 {
		panic(fmt.Sprintf("unexpected directory replacement error: %v", conflictErr))
	}
	still, err := c.ReadFile(ctx, sb.ID, "/tree/zdata")
	must(err)
	if len(still) != 9<<20 {
		panic("directory conflict mutated data")
	}
	verifyMemoryMutations(ctx, c)
	summary, err := json.Marshal(map[string]any{"status": "passed", "regular_members": regular, "hardlinks": links, "payload_bytes": total, "metadata_iterations": 1000})
	must(err)
	fmt.Println(string(summary))
	_, err = c.KillSandbox(ctx, sb.ID)
	must(err)
}
func must(err error) {
	if err != nil {
		panic(err)
	}
}
