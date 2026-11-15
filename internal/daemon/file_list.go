package daemon

import (
	"cmp"
	"context"
	"encoding/base64"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// File scan limits bound recursive archive work and retained metadata.
const (
	MaxFileScanBytes     = 64 << 20
	MaxFileScanEntries   = 10000
	MaxFilePathBytes     = 4096
	FileOperationTimeout = 30 * time.Second
)

// FileEntry describes an immediate directory child; mode contains POSIX permission bits.
type FileEntry struct {
	Name  string    `json:"name"`
	Size  int64     `json:"size"`
	Mode  int64     `json:"mode"`
	Mtime time.Time `json:"mtime"`
	IsDir bool      `json:"is_dir"`
}

// FileList is one name-ordered page. NextCursor is absent on the last page.
type FileList struct {
	Entries    []FileEntry `json:"entries"`
	NextCursor string      `json:"next_cursor,omitempty"`
}

// ValidateFilePath validates directory/archive paths without changing legacy file paths.
func ValidateFilePath(p string) error {
	if !path.IsAbs(p) || len(p) > MaxFilePathBytes || strings.ContainsRune(p, 0) || !utf8.ValidString(p) {
		return errorf(CodeInvalidRequest, "path must be an absolute UTF-8 path of at most %d bytes without NUL", MaxFilePathBytes)
	}
	for part := range strings.SplitSeq(p, "/") {
		if part == ".." {
			return errorf(CodeInvalidRequest, "path must not contain traversal components")
		}
	}
	return nil
}

// SelectFilePage bounds retained metadata independently of directory size.
func SelectFilePage(entries []FileEntry, entry FileEntry, after string, limit int) []FileEntry {
	if entry.Name <= after {
		return entries
	}
	i, _ := slices.BinarySearchFunc(entries, entry, func(a, b FileEntry) int { return cmp.Compare(a.Name, b.Name) })
	if i > limit {
		return entries
	}
	entries = slices.Insert(entries, i, entry)
	if len(entries) > limit+1 {
		entries = entries[:limit+1]
	}
	return entries
}
func fileCursor(p, cursor string) (string, error) {
	if cursor == "" {
		return "", nil
	}
	if len(cursor) > 12000 {
		return "", errorf(CodeInvalidRequest, "invalid cursor")
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(cursor)
	if err != nil {
		return "", errorf(CodeInvalidRequest, "invalid cursor")
	}
	cursorPath, name, ok := strings.Cut(string(raw), "\x00")
	if !ok || cursorPath != p || name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") || !utf8.ValidString(name) || len(name) > MaxFilePathBytes {
		return "", errorf(CodeInvalidRequest, "cursor does not identify a child of path")
	}
	return name, nil
}

// ListFiles lists immediate children, resuming a paused sandbox first.
func (s *Sandboxes) ListFiles(ctx context.Context, id, p string, limit int, cursor string) (FileList, error) {
	if err := ValidateFilePath(p); err != nil {
		return FileList{}, err
	}
	p = path.Clean(p)
	if limit < 1 || limit > 1000 {
		return FileList{}, errorf(CodeInvalidRequest, "limit must be between 1 and 1000")
	}
	after, err := fileCursor(p, cursor)
	if err != nil {
		return FileList{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, FileOperationTimeout)
	defer cancel()
	sb, err := s.prepareFiles(ctx, id)
	if err != nil {
		return FileList{}, err
	}
	sb.mu.Lock()
	container := sb.container
	sb.mu.Unlock()
	entries, err := s.backend.ListFiles(ctx, container, p, after, limit)
	if err != nil {
		return FileList{}, backendError(err)
	}
	result := FileList{Entries: entries}
	if len(entries) > limit {
		result.Entries = entries[:limit]
		result.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(p + "\x00" + entries[limit-1].Name))
	}
	return result, nil
}
func (h *Handler) listFiles(w http.ResponseWriter, r *http.Request) {
	q, err := strictFileQuery(r, "path", "limit", "cursor")
	if err != nil {
		writeDaemonError(r.Context(), w, err)
		return
	}
	limit := 100
	if raw, ok := q["limit"]; ok {
		limit, err = strconv.Atoi(raw)
		if err != nil {
			writeDaemonError(r.Context(), w, errorf(CodeInvalidRequest, "limit must be an integer"))
			return
		}
	}
	result, err := h.store.ListFiles(r.Context(), r.PathValue("sandbox_id"), q["path"], limit, q["cursor"])
	if err != nil {
		writeDaemonError(r.Context(), w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
func strictFileQuery(r *http.Request, allowed ...string) (map[string]string, error) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, errorf(CodeInvalidRequest, "invalid query encoding")
	}
	result := make(map[string]string, len(q))
	for key, values := range q {
		if !slices.Contains(allowed, key) || len(values) != 1 {
			return nil, errorf(CodeInvalidRequest, "unexpected or repeated query parameter %q", key)
		}
		result[key] = values[0]
	}
	return result, nil
}
