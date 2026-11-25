package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	sandbox "github.com/ruhuang2001/execlet/sandbox-go"
	"golang.org/x/term"
)

func (in *invocation) shell(args []string) int {
	pos, flags := splitArgs(args)
	if len(flags) != 0 || len(pos) < 1 || len(pos) > 2 {
		_, _ = fmt.Fprintln(in.stderr, "usage: execletctl shell SANDBOX [TERMINAL]")
		return 2
	}
	client, err := in.client()
	if err != nil {
		return in.fail(err)
	}
	ctx, cancel := contextWithSignals()
	defer cancel()
	sb := pos[0]
	fd := int(os.Stdin.Fd())
	size := func() sandbox.WindowSize {
		cols, rows, err := term.GetSize(fd)
		if err != nil || rows <= 0 || cols <= 0 || rows > 65535 || cols > 65535 {
			return sandbox.WindowSize{Rows: 24, Cols: 80}
		}
		return sandbox.WindowSize{Rows: uint16(rows), Cols: uint16(cols)}
	}
	var id string
	if len(pos) == 2 {
		id = pos[1]
	} else {
		id, err = client.OpenTerminal(ctx, sb, size())
		if err != nil {
			return in.fail(err)
		}
	}
	_, _ = fmt.Fprintf(in.stderr, "terminal %s; reconnect with execletctl shell %s %s (Ctrl-] detaches)\n", id, sb, id)
	if term.IsTerminal(fd) {
		prior, err := term.MakeRaw(fd)
		if err != nil {
			return in.fail(err)
		}
		defer func() { _ = term.Restore(fd, prior) }()
	}
	resizes := make(chan os.Signal, 1)
	signal.Notify(resizes, syscall.SIGWINCH)
	defer signal.Stop(resizes)
	done := make(chan error, 1)
	go func() { done <- client.StreamTerminal(ctx, sb, id, 0, in.stdout) }()
	input := make(chan error, 1)
	go func() {
		var data [4096]byte
		for {
			n, readErr := os.Stdin.Read(data[:])
			chunk := data[:n]
			i := bytes.IndexByte(chunk, 29)
			if i >= 0 {
				chunk = chunk[:i]
			}
			if len(chunk) > 0 {
				if err := client.TerminalInput(ctx, sb, id, chunk); err != nil {
					input <- err
					return
				}
			}
			if i >= 0 {
				input <- context.Canceled
				return
			}
			if readErr != nil {
				if errors.Is(readErr, io.EOF) {
					readErr = client.TerminalInput(ctx, sb, id, []byte{4})
					if errors.Is(readErr, sandbox.ErrInvalidState) {
						readErr = nil
					}
				}
				input <- readErr
				return
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return 0
		case <-resizes:
			if err := client.ResizeTerminal(ctx, sb, id, size()); err != nil {
				return in.fail(err)
			}
		case err := <-input:
			if errors.Is(err, context.Canceled) {
				return 0
			}
			if err != nil {
				return in.fail(err)
			}
			input = nil
		case err := <-done:
			if err != nil {
				return in.fail(err)
			}
			closeCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer stop()
			if err := client.CloseTerminal(closeCtx, sb, id); err != nil {
				return in.fail(err)
			}
			return 0
		}
	}
}
