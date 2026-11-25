package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	sandbox "github.com/ruhuang2001/execlet/sandbox-go"
)

func (in *invocation) files(args []string) int {
	if len(args) > 0 && args[0] == "archive" {
		return in.archive(args[1:])
	}
	if len(args) == 0 || args[0] != "list" {
		_, _ = fmt.Fprintln(in.stderr, "usage: execletctl files list SANDBOX PATH [--limit N] [--cursor TOKEN]\n       execletctl files archive upload|download SANDBOX PATH LOCAL_TAR")
		return 2
	}
	positional, flags := splitArgs(args[1:])
	if len(positional) != 2 {
		_, _ = fmt.Fprintln(in.stderr, "usage: execletctl files list SANDBOX PATH [--limit N] [--cursor TOKEN]\n       execletctl files archive upload|download SANDBOX PATH LOCAL_TAR")
		return 2
	}
	fs := flag.NewFlagSet("files list", flag.ContinueOnError)
	fs.SetOutput(in.stderr)
	opts := sandbox.ListFilesOptions{}
	fs.IntVar(&opts.Limit, "limit", 100, "maximum entries (1-1000)")
	fs.StringVar(&opts.Cursor, "cursor", "", "opaque continuation cursor")
	if err := fs.Parse(flags); err != nil || fs.NArg() != 0 {
		return 2
	}
	if opts.Limit < 1 || opts.Limit > 1000 {
		_, _ = fmt.Fprintln(in.stderr, "limit must be between 1 and 1000")
		return 2
	}
	c, err := in.client()
	if err != nil {
		return in.fail(err)
	}
	ctx, cancel := contextWithSignals()
	defer cancel()
	page, err := c.ListFiles(ctx, positional[0], positional[1], opts)
	if err != nil {
		return in.fail(err)
	}
	data, err := json.Marshal(page)
	if err != nil {
		return in.fail(err)
	}
	if _, err = fmt.Fprintln(in.stdout, string(data)); err != nil {
		return in.fail(err)
	}
	return 0
}

func (in *invocation) archive(args []string) int {
	if len(args) != 4 || (args[0] != "upload" && args[0] != "download") {
		_, _ = fmt.Fprintln(in.stderr, "usage: execletctl files archive upload|download SANDBOX PATH LOCAL_TAR")
		return 2
	}
	c, err := in.client()
	if err != nil {
		return in.fail(err)
	}
	ctx, cancel := contextWithSignals()
	defer cancel()
	if args[0] == "upload" {
		data, err := readLocalArchive(ctx, args[3])
		if err != nil {
			return in.fail(err)
		}
		if err := c.WriteArchive(ctx, args[1], args[2], data); err != nil {
			return in.fail(err)
		}
		return 0
	}
	file, err := os.CreateTemp(filepath.Dir(args[3]), ".sandbox-archive-*")
	if err != nil {
		return in.fail(err)
	}
	defer func() { _ = file.Close(); _ = os.Remove(file.Name()) }()
	data, err := c.ReadArchive(ctx, args[1], args[2])
	if err != nil {
		return in.fail(err)
	}
	if err := saveArchive(file, args[3], data); err != nil {
		return in.fail(err)
	}
	return 0
}

func saveArchive(file *os.File, destination string, data []byte) error {
	if _, err := file.Write(data); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := file.Sync(); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), destination)
}

func readLocalArchive(ctx context.Context, name string) ([]byte, error) {
	info, err := os.Stat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("archive input must be a regular file")
	}
	if err := ctx.Err(); err != nil {
		return nil, sandbox.ErrCanceled
	}
	file, err := os.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	info, err = file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("archive input must be a regular file")
	}
	stop := context.AfterFunc(ctx, func() { _ = file.Close() })
	defer stop()
	data, readErr := io.ReadAll(io.LimitReader(file, (32<<20)+1))
	closeErr := file.Close()
	if ctx.Err() != nil {
		return nil, sandbox.ErrCanceled
	}
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, err
	}
	if len(data) > 32<<20 {
		return nil, fmt.Errorf("archive exceeds 32 MiB encoded limit")
	}
	return data, nil
}
