// Command file-e2e verifies the Go SDK against a live Docker sandbox.
package main

import (
	"context"
	"encoding/json/v2"
	"fmt"
	sandbox "github.com/ruhuang2001/tiny-sandbox/sandbox-go"
	"os"
	"slices"
)

func main() {
	c, err := sandbox.NewClient(sandbox.Config{})
	must(err)
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
