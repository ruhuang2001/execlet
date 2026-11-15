package daemon

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path"
	"strconv"

	"github.com/ruhuang2001/tiny-sandbox/internal/filearchive"
)

// ReadArchive returns a validated tar archive containing the selected path's contents.
func (s *Sandboxes) ReadArchive(ctx context.Context, id, p string) ([]byte, error) {
	if err := ValidateFilePath(p); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, FileOperationTimeout)
	defer cancel()
	sb, err := s.prepareFiles(ctx, id)
	if err != nil {
		return nil, err
	}
	sb.mu.Lock()
	container := sb.container
	sb.mu.Unlock()
	data, err := s.backend.ReadArchive(ctx, container, path.Clean(p))
	if err != nil {
		return nil, backendError(err)
	}
	archive, err := filearchive.Parse(data)
	if err != nil {
		return nil, archiveError(err)
	}
	return archive.Bytes(), nil
}

// WriteArchive validates the complete tar before preparing or mutating a sandbox.
func (s *Sandboxes) WriteArchive(ctx context.Context, id, p string, data []byte) error {
	if err := ValidateFilePath(p); err != nil {
		return err
	}
	archive, err := filearchive.Parse(data)
	if err != nil {
		return archiveError(err)
	}
	ctx, cancel := context.WithTimeout(ctx, FileOperationTimeout)
	defer cancel()
	sb, err := s.prepareFiles(ctx, id)
	if err != nil {
		return err
	}
	sb.mu.Lock()
	container := sb.container
	sb.mu.Unlock()
	if err := s.backend.WriteArchive(ctx, container, path.Clean(p), archive); err != nil {
		return backendError(err)
	}
	return nil
}
func archiveError(err error) error {
	code := CodeInvalidRequest
	if e, ok := errors.AsType[*filearchive.Error](err); ok && e.TooLarge {
		code = CodePayloadTooLarge
	}
	return &Error{Code: code, Message: err.Error(), Err: err}
}
func archivePath(w http.ResponseWriter, r *http.Request) (string, bool) {
	q, err := strictFileQuery(r, "path")
	if err != nil {
		writeDaemonError(r.Context(), w, err)
		return "", false
	}
	if err := ValidateFilePath(q["path"]); err != nil {
		writeDaemonError(r.Context(), w, err)
		return "", false
	}
	return q["path"], true
}
func (h *Handler) readArchive(w http.ResponseWriter, r *http.Request) {
	p, ok := archivePath(w, r)
	if !ok {
		return
	}
	data, err := h.store.ReadArchive(r.Context(), r.PathValue("sandbox_id"), p)
	if err != nil {
		writeDaemonError(r.Context(), w, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
}
func (h *Handler) writeArchive(w http.ResponseWriter, r *http.Request) {
	p, ok := archivePath(w, r)
	if !ok {
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, filearchive.MaxBytes))
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			writeDaemonError(r.Context(), w, errorf(CodePayloadTooLarge, "archive exceeds 32 MiB encoded limit"))
		} else {
			writeDaemonError(r.Context(), w, errorf(CodeInvalidRequest, "cannot read archive body: %v", err))
		}
		return
	}
	if err := h.store.WriteArchive(r.Context(), r.PathValue("sandbox_id"), p, data); err != nil {
		writeDaemonError(r.Context(), w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
