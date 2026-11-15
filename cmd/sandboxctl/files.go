package main

import (
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	sandbox "github.com/ruhuang2001/tiny-sandbox/sandbox-go"
)

func (in *invocation) files(args []string) int {
	if len(args) > 0 && args[0] == "archive" {
		return in.archive(args[1:])
	}
	if len(args) == 0 || args[0] != "list" {
		_, _ = fmt.Fprintln(in.stderr, "usage: sandboxctl files list SANDBOX PATH [--limit N] [--cursor TOKEN]")
		return 2
	}
	positional, flags := splitArgs(args[1:])
	if len(positional) != 2 {
		_, _ = fmt.Fprintln(in.stderr, "usage: sandboxctl files list SANDBOX PATH [--limit N] [--cursor TOKEN]")
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
		_, _ = fmt.Fprintln(in.stderr, "usage: sandboxctl files archive upload|download SANDBOX PATH LOCAL_TAR")
		return 2
	}
	c, err := in.client()
	if err != nil {
		return in.fail(err)
	}
	ctx, cancel := contextWithSignals()
	defer cancel()
	if args[0] == "upload" {
		f, err := os.Open(args[3])
		if err != nil {
			return in.fail(err)
		}
		data, readErr := io.ReadAll(io.LimitReader(f, (32<<20)+1))
		closeErr := f.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return in.fail(err)
		}
		if len(data) > 32<<20 {
			return in.fail(fmt.Errorf("archive exceeds 32 MiB encoded limit"))
		}
		if err := c.WriteArchive(ctx, args[1], args[2], data); err != nil {
			return in.fail(err)
		}
		return 0
	}
	data, err := c.ReadArchive(ctx, args[1], args[2])
	if err != nil {
		return in.fail(err)
	}
	if err := saveArchive(args[3], data); err != nil {
		return in.fail(err)
	}
	return 0
}

// saveArchive replaces the destination only after a complete local save.
func saveArchive(destination string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(destination), ".sandbox-archive-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer func() { _ = os.Remove(temporary) }()
	if _, err := file.Write(data); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := file.Sync(); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(temporary, destination)
}
