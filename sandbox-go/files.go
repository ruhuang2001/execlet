package sandbox

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/ruhuang2001/execlet/internal/wirejson"
)

type FileType string

const (
	FileTypeFile    FileType = "file"
	FileTypeDir     FileType = "dir"
	FileTypeSymlink FileType = "symlink"
	FileTypeOther   FileType = "other"
)

type FileEntry struct {
	Name          string    `json:"name"`
	Size          int64     `json:"size"`
	Mode          int64     `json:"mode"`
	Mtime         time.Time `json:"mtime"`
	IsDir         bool      `json:"is_dir"`
	Type          FileType  `json:"type"`
	UID           int       `json:"uid"`
	GID           int       `json:"gid"`
	Owner         string    `json:"owner,omitempty"`
	Group         string    `json:"group,omitempty"`
	SymlinkTarget string    `json:"symlink_target,omitempty"`
}

func (e *FileEntry) UnmarshalJSON(data []byte) error {
	var fields struct {
		Name          *string    `json:"name"`
		Size          *int64     `json:"size"`
		Mode          *int64     `json:"mode"`
		Mtime         *time.Time `json:"mtime"`
		IsDir         *bool      `json:"is_dir"`
		Type          *FileType  `json:"type"`
		UID           *int       `json:"uid"`
		GID           *int       `json:"gid"`
		Owner         string     `json:"owner"`
		Group         string     `json:"group"`
		SymlinkTarget string     `json:"symlink_target"`
	}
	if err := wirejson.Unmarshal(data, &fields); err != nil {
		return err
	}
	if fields.Name == nil || fields.Size == nil || fields.Mode == nil || fields.Mtime == nil || fields.IsDir == nil || fields.Type == nil || fields.UID == nil || fields.GID == nil {
		return errors.New("sandbox: missing required file metadata")
	}
	*e = FileEntry{Name: *fields.Name, Size: *fields.Size, Mode: *fields.Mode, Mtime: *fields.Mtime,
		IsDir: *fields.IsDir, Type: *fields.Type, UID: *fields.UID, GID: *fields.GID,
		Owner: fields.Owner, Group: fields.Group, SymlinkTarget: fields.SymlinkTarget}
	return nil
}

type ListFilesOptions struct {
	Limit  int
	Cursor string
}

type FileList struct {
	Entries    []FileEntry `json:"entries"`
	NextCursor string      `json:"next_cursor,omitempty"`
}

func (c *Client) ListFiles(ctx context.Context, id, path string, options ListFilesOptions) (*FileList, error) {
	if err := c.RequireCapability(ctx, "files.list.v1"); err != nil {
		return nil, err
	}
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
	if out.Entries == nil {
		return nil, &Error{StatusCode: resp.status, Code: "internal", Message: "sandbox: missing file list entries"}
	}
	return &out, nil
}

func (c *Client) ReadArchive(ctx context.Context, id, path string) ([]byte, error) {
	if err := c.RequireCapability(ctx, "files.archive.v1"); err != nil {
		return nil, err
	}
	resp, err := c.do(ctx, request{method: http.MethodGet, path: "/v1/sandboxes/" + url.PathEscape(id) + "/files/archive", query: "?path=" + url.QueryEscape(path), repeatable: true})
	if err != nil {
		return nil, err
	}
	return resp.body, nil
}

func (c *Client) WriteArchive(ctx context.Context, id, path string, data []byte) error {
	if err := c.RequireCapability(ctx, "files.archive.v1"); err != nil {
		return err
	}
	_, err := c.do(ctx, request{method: http.MethodPut, path: "/v1/sandboxes/" + url.PathEscape(id) + "/files/archive", query: "?path=" + url.QueryEscape(path), raw: data})
	return err
}

func (c *Client) StatFile(ctx context.Context, id, path string) (*FileEntry, error) {
	if err := c.RequireCapability(ctx, "files.stat.v1"); err != nil {
		return nil, err
	}
	resp, err := c.do(ctx, request{method: http.MethodGet, path: "/v1/sandboxes/" + url.PathEscape(id) + "/files/stat", query: "?path=" + url.QueryEscape(path), repeatable: true})
	if err != nil {
		return nil, err
	}
	var out FileEntry
	if err = decode(resp, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
