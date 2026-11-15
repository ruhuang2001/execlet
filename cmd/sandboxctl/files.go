package main

import (
	"encoding/json/v2"
	"flag"
	"fmt"
	sandbox "github.com/ruhuang2001/tiny-sandbox/sandbox-go"
)

func (in *invocation) files(args []string) int {
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
