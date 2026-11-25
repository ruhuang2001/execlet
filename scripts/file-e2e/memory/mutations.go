package main

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"

	sandbox "github.com/ruhuang2001/execlet/sandbox-go"
)

func verifyMemoryMutations(ctx context.Context, c *sandbox.Client) {
	sb, err := c.CreateSandbox(ctx, sandbox.CreateSandboxRequest{})
	must(err)
	initial := memoryTar([]tar.Header{{Name: "target", Typeflag: tar.TypeReg, Size: 3}, {Name: "link", Typeflag: tar.TypeLink, Linkname: "target"}, {Name: "outer/inner", Typeflag: tar.TypeDir}}, map[string][]byte{"target": []byte("old")})
	must(c.WriteArchive(ctx, sb.ID, "/", initial))
	must(c.WriteArchive(ctx, sb.ID, "/", memoryTar([]tar.Header{{Name: "target", Typeflag: tar.TypeReg, Size: 3}}, map[string][]byte{"target": []byte("new")})))
	target, err := c.ReadFile(ctx, sb.ID, "/target")
	must(err)
	if !bytes.Equal(target, []byte("new")) {
		panic("target replacement not applied")
	}
	must(c.WriteArchive(ctx, sb.ID, "/outer", memoryTar([]tar.Header{{Name: "child", Typeflag: tar.TypeReg, Size: 2}}, map[string][]byte{"child": []byte("ok")})))
	data, err := c.ReadFile(ctx, sb.ID, "/link")
	must(err)
	if !bytes.Equal(data, []byte("old")) {
		panic("replaced target changed existing link bytes")
	}
	data, err = c.ReadArchive(ctx, sb.ID, "/")
	must(err)
	reader := tar.NewReader(bytes.NewReader(data))
	found := false
	for {
		h, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		must(err)
		if h.Name == "link" {
			found = true
			body, err := io.ReadAll(reader)
			must(err)
			if h.Typeflag != tar.TypeReg || !bytes.Equal(body, []byte("old")) {
				panic("round trip retargeted link")
			}
		}
	}
	if !found {
		panic("missing materialized link")
	}
	_, err = c.KillSandbox(ctx, sb.ID)
	must(err)
}
func memoryTar(headers []tar.Header, payloads map[string][]byte) []byte {
	var out bytes.Buffer
	w := tar.NewWriter(&out)
	for _, h := range headers {
		h.Mode = 0o755
		must(w.WriteHeader(&h))
		if h.Typeflag == tar.TypeReg {
			_, err := w.Write(payloads[h.Name])
			must(err)
		}
	}
	must(w.Close())
	return out.Bytes()
}
