package dockerbackend

import (
	"context"
	"io"
	"path"
	"strings"

	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
	"github.com/ruhuang2001/tiny-sandbox/internal/filearchive"
)

// ReadArchive buffers a bounded archive; the daemon validates before serving it.
func (b *Backend) ReadArchive(ctx context.Context, id, p string) ([]byte, error) {
	rc, err := b.api.ArchiveGet(ctx, id, p)
	if err != nil {
		return nil, notFound(err, "%s", p)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(io.LimitReader(rc, filearchive.MaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > filearchive.MaxBytes {
		return nil, &daemon.Error{Code: daemon.CodePayloadTooLarge, Message: "archive exceeds 32 MiB encoded limit"}
	}
	return data, nil
}

// WriteArchive preflights destination components before Docker extracts validated bytes.
func (b *Backend) WriteArchive(ctx context.Context, id, p string, archive *filearchive.Archive) error {
	if err := b.checkArchiveDestination(ctx, id, p); err != nil {
		return err
	}
	batch := []string{}
	size := 0
	for _, entry := range archive.Entries {
		name := path.Join(p, entry.Header.Name)
		if size+len(name) > 32000 {
			if err := b.checkArchivePaths(ctx, id, batch); err != nil {
				return err
			}
			batch = nil
			size = 0
		}
		batch = append(batch, name)
		size += len(name)
	}
	if err := b.checkArchivePaths(ctx, id, batch); err != nil {
		return err
	}
	return notFound(b.api.ArchivePut(ctx, id, p, archive.Data), "%s", p)
}
func (b *Backend) checkArchiveDestination(ctx context.Context, id, p string) error {
	if err := b.checkArchivePaths(ctx, id, []string{p}); err != nil {
		return err
	}
	out, err := b.capture(ctx, id, []string{"sh", "-c", `if [ ! -e "$1" ]; then printf missing; elif [ ! -d "$1" ]; then printf notdir; fi`, "archive-check", p})
	if err != nil {
		return err
	}
	switch string(out) {
	case "":
		return nil
	case "missing":
		return daemon.ErrNotFound
	case "notdir":
		return &daemon.Error{Code: daemon.CodeInvalidRequest, Message: "archive destination must be a directory"}
	default:
		return &daemon.Error{Code: daemon.CodeBackendError, Message: "unexpected archive destination check output"}
	}
}
func (b *Backend) checkArchivePaths(ctx context.Context, id string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	command := []string{"sh", "-c", archivePathCheck, "archive-check"}
	command = append(command, paths...)
	out, err := b.capture(ctx, id, command)
	if err != nil {
		return err
	}
	if len(out) != 0 {
		return &daemon.Error{Code: daemon.CodeInvalidRequest, Message: "archive destination contains a symlink or non-directory ancestor: " + strings.TrimSpace(string(out))}
	}
	return nil
}

// Fixed shell program; names are positional arguments, never evaluated as code.
const archivePathCheck = `for file do
 current=$file
 first=1
 while [ "$current" != / ] && [ "$current" != . ]; do
  if [ -L "$current" ]; then printf '%s' "$current"; exit 0; fi
  if [ "$first" = 0 ] && [ -e "$current" ] && [ ! -d "$current" ]; then printf '%s' "$current"; exit 0; fi
  first=0
  current=${current%/*}
  [ -n "$current" ] || current=/
 done
done`
