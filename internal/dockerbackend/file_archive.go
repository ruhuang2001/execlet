package dockerbackend

import (
	"context"
	"io"
	"strings"

	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
	"github.com/ruhuang2001/tiny-sandbox/internal/filearchive"
)

func (b *Backend) ReadArchive(ctx context.Context, id, p string) ([]byte, error) {
	rc, err := b.archiveGet(ctx, id, p)
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

func (b *Backend) WriteArchive(ctx context.Context, id, p string, archive *filearchive.Archive) error {
	if archive == nil || archive.Len() == 0 {
		return &daemon.Error{Code: daemon.CodeInvalidRequest, Message: "archive must be constructed by filearchive.Parse"}
	}
	if err := b.checkArchiveDestination(ctx, id, p); err != nil {
		return err
	}
	batch := []string{}
	size := 0
	for _, entry := range archive.Entries() {
		name := entry.Header.Name
		if size+len(name)+1 > 256<<10 {
			if err := b.checkArchivePaths(ctx, id, p, batch); err != nil {
				return err
			}
			batch = nil
			size = 0
		}
		batch = append(batch, name)
		size += len(name) + 1
	}
	if err := b.checkArchivePaths(ctx, id, p, batch); err != nil {
		return err
	}
	uid, gid, _ := b.writeOwnership(ctx, id, strings.TrimPrefix(p, "/"))
	data, err := archive.BytesOwnedBy(uid, gid)
	if err != nil {
		return err
	}
	return notFound(b.archivePut(ctx, id, p, data), "%s", p)
}
func (b *Backend) checkArchiveDestination(ctx context.Context, id, p string) error {
	if err := b.checkArchivePaths(ctx, id, "/", []string{p}); err != nil {
		return err
	}
	out, err := b.captureAs(ctx, id, "0", []string{"sh", "-c", `if [ ! -e "$1" ]; then printf missing; elif [ ! -d "$1" ]; then printf notdir; fi`, "archive-check", p})
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
func (b *Backend) checkArchivePaths(ctx context.Context, id, base string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	command := []string{"sh", "-c", archivePathCheck, base}
	command = append(command, paths...)
	out, err := b.captureAs(ctx, id, "0", command)
	if err != nil {
		return err
	}
	if len(out) != 0 {
		return &daemon.Error{Code: daemon.CodeInvalidRequest, Message: "archive destination contains a symlink or non-directory ancestor: " + strings.TrimSpace(string(out))}
	}
	return nil
}

const archivePathCheck = `for file do
 case "$file" in /*) current=$file;; *) current=$0/$file;; esac
 first=1
 while [ "$current" != / ] && [ "$current" != . ]; do
  if [ -L "$current" ]; then printf '%s' "$current"; exit 0; fi
  if [ "$first" = 0 ] && [ -e "$current" ] && [ ! -d "$current" ]; then printf '%s' "$current"; exit 0; fi
  first=0
  current=${current%/*}
  [ -n "$current" ] || current=/
 done
done`
