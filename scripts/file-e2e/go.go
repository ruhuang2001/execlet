package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"

	sandbox "github.com/ruhuang2001/tiny-sandbox/sandbox-go"
)

func main() {
	c, err := sandbox.NewClient(sandbox.Config{})
	must(err)
	if len(os.Args) != 2 && (len(os.Args) != 4 || os.Args[2] != "archive") {
		panic("usage: go-files SANDBOX [archive FIXTURE]")
	}
	if len(os.Args) == 4 {
		archive(c)
		return
	}
	names := []string{}
	cursor := ""
	for {
		page, err := c.ListFiles(context.Background(), os.Args[1], "/listing", sandbox.ListFilesOptions{Limit: 2, Cursor: cursor})
		must(err)
		for _, entry := range page.Entries {
			names = append(names, entry.Name)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if !slices.Equal(names, []string{".hidden", "a space", "empty", "link", "nested", "z", "é"}) {
		panic(fmt.Sprint(names))
	}
	data, err := json.Marshal(map[string]any{"sdk": "go", "names": names})
	must(err)
	fmt.Println(string(data))
}
func must(err error) {
	if err != nil {
		panic(err)
	}
}

func archive(c *sandbox.Client) {
	data, err := os.ReadFile(os.Args[3])
	must(err)
	must(c.WriteArchive(context.Background(), os.Args[1], "/archives/go", data))
	result, err := c.ReadArchive(context.Background(), os.Args[1], "/archives/go/tree")
	must(err)
	reader := tar.NewReader(bytes.NewReader(result))
	headers := map[string]tar.Header{}
	payloads := map[string][]byte{}
	for {
		h, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		must(err)
		headers[h.Name] = *h
		data, err := io.ReadAll(reader)
		must(err)
		payloads[h.Name] = data
	}
	if !bytes.Equal(payloads["tree/data"], []byte{0, 255, 98, 105, 110, 97, 114, 121, 10}) || headers["tree/data"].Mode != 0o640 || headers["tree/empty"].Typeflag != tar.TypeDir || headers["tree/link"].Typeflag != tar.TypeSymlink || headers["tree/link"].Linkname != "data" || headers["tree/hard"].Typeflag != tar.TypeLink || headers["tree/hard"].Linkname != "tree/data" {
		panic("archive entries, content, metadata or links differ from fixture")
	}

	fmt.Println("archive round trip passed")
}
