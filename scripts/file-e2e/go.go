// Command file-e2e verifies the Go SDK against a live Docker sandbox.
package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"slices"

	sandbox "github.com/ruhuang2001/tiny-sandbox/sandbox-go"
)

func main() {
	c, err := sandbox.NewClient(sandbox.Config{})
	must(err)
	if len(os.Args) > 2 {
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
	if len(result) < 1024 || !bytes.Contains(result, []byte{0, 255, 98, 105, 110, 97, 114, 121, 10}) {
		panic("empty download")
	}
	fmt.Println("archive round trip passed")
}
