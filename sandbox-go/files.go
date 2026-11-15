package sandbox

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// FileEntry describes an immediate child without following child symlinks.
type FileEntry struct {
	Name  string    `json:"name"`
	Size  int64     `json:"size"`
	Mode  int64     `json:"mode"`
	Mtime time.Time `json:"mtime"`
	IsDir bool      `json:"is_dir"`
}

// ListFilesOptions selects a page. Zero Limit uses the server default (100).
type ListFilesOptions struct {
	Limit  int
	Cursor string
}

// FileList contains a page and an optional opaque continuation cursor.
type FileList struct {
	Entries    []FileEntry `json:"entries"`
	NextCursor string      `json:"next_cursor,omitempty"`
}

// ListFiles returns one sorted page of immediate directory children.
func (c *Client) ListFiles(ctx context.Context, id, path string, options ListFilesOptions) (*FileList, error) {
	q := url.Values{"path": {path}}
	if options.Limit != 0 {
		q.Set("limit", strconv.Itoa(options.Limit))
	}
	if options.Cursor != "" {
		q.Set("cursor", options.Cursor)
	}
	resp, err := c.do(ctx, request{method: http.MethodGet, path: "/v1/sandboxes/" + url.PathEscape(id) + "/files/list", query: "?" + q.Encode(), repeatable: true})
	if err != nil {
		return nil, err
	}
	var out FileList
	if err := decode(resp, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
