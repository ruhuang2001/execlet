package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	sandbox "github.com/ruhuang2001/tiny-sandbox/sandbox-go"
)

func (s *sandboxTools) runTool(ctx context.Context, in runInput) (toolOutput, error) {
	cwd := in.Cwd
	if cwd == "" {
		if _, err := os.Stat(s.manifestPath(in.SandboxID)); err == nil {
			cwd = guestWorkspace
		} else if !errors.Is(err, os.ErrNotExist) {
			return toolOutput{}, err
		}
	}
	deadline := time.Now().Add(60 * time.Second)
	ex, err := s.client.StartExecution(ctx, in.SandboxID, sandbox.ExecuteRequest{OutputTail: true, Command: in.Command, Cwd: cwd, TimeoutSeconds: in.Timeout, IdempotencyKey: in.Key})
	if err != nil {
		return toolOutput{}, err
	}
	wait, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	if ex.State == "running" {
		final, err := s.client.ObserveExecution(wait, in.SandboxID, ex.ID)
		if errors.Is(wait.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return executionResult(ex, getInput{}), nil
		}
		if err != nil {
			return toolOutput{}, err
		}
		ex = final
	}
	return executionResult(ex, getInput{}), nil
}

func (s *sandboxTools) getTool(ctx context.Context, in getInput) (toolOutput, error) {
	if (in.Offset != nil && *in.Offset < 0) || (in.StdoutOffset != nil && *in.StdoutOffset < 0) || (in.StderrOffset != nil && *in.StderrOffset < 0) {
		return toolOutput{}, toolError("invalid_request", "offset must be non-negative")
	}
	ex, err := s.client.GetExecution(ctx, in.SandboxID, in.ExecutionID)
	if err != nil {
		return toolOutput{}, err
	}
	return executionResult(ex, in), nil
}

func (s *sandboxTools) cancelTool(ctx context.Context, in executionInput) (toolOutput, error) {
	ex, err := s.client.CancelExecution(ctx, in.SandboxID, in.ExecutionID)
	if err != nil {
		return toolOutput{}, err
	}
	return toolOutput{"Execution " + ex.ID + " is " + ex.State, map[string]any{"state": ex.State, "termination_reason": ex.TerminationReason}}, nil
}

const mcpPage = 64 << 10

func executionResult(ex *sandbox.Execution, in getInput) toolOutput {
	out := map[string]any{"execution_id": ex.ID, "state": ex.State}
	summary := "Execution " + ex.ID + " is " + ex.State
	if ex.State == "running" && in.Offset == nil && in.StdoutOffset == nil && in.StderrOffset == nil {
		return toolOutput{summary, out}
	}
	if ex.ExitCode != nil {
		summary += fmt.Sprintf(" (exit %d)", *ex.ExitCode)
	}
	stdoutRequest, stderrRequest := in.StdoutOffset, in.StderrOffset
	if stdoutRequest == nil {
		stdoutRequest = in.Offset
	}
	if stderrRequest == nil {
		stderrRequest = in.Offset
	}
	sharedLimit := int64(mcpPage)
	if in.Offset != nil && in.StdoutOffset == nil && in.StderrOffset == nil {
		for _, stream := range []struct {
			text string
			base int64
		}{{ex.Stdout, ex.StdoutOffset}, {ex.Stderr, ex.StderrOffset}} {
			pos := max(0, *in.Offset-stream.base)
			if pos < int64(len(stream.text))-mcpPage && !utf8.RuneStart(stream.text[pos+mcpPage]) {
				sharedLimit -= utf8.UTFMax - 1
				break
			}
		}
	}
	stdout, so, sn, sc, sl := outputWindow(ex.Stdout, ex.StdoutOffset, stdoutRequest, sharedLimit)
	stderr, eo, en, ec, el := outputWindow(ex.Stderr, ex.StderrOffset, stderrRequest, sharedLimit)
	truncated := sc || ec
	next := max(sn, en)
	switch {
	case in.Offset == nil && truncated:
		next = min(ex.StdoutOffset, ex.StderrOffset)
	case sc && ec:
		next = min(sn, en)
	case sc:
		next = sn
	case ec:
		next = en
	}
	out["stdout"], out["stderr"] = stdout, stderr
	out["stdout_start_offset"], out["stderr_start_offset"] = so, eo
	out["next_stdout_offset"], out["next_stderr_offset"] = sn, en
	out["output_lost"] = sl || el
	out["exit_code"], out["termination_reason"] = ex.ExitCode, ex.TerminationReason
	out["output_truncated"], out["next_offset"] = truncated || ex.OutputTruncated, next
	var display strings.Builder
	display.WriteString(summary)
	if stdout != "" {
		display.WriteString("\n\nstdout\n")
		display.WriteString(stdout)
	}
	if stderr != "" {
		display.WriteString("\n\nstderr\n")
		display.WriteString(stderr)
	}
	if truncated {
		_, _ = fmt.Fprintf(&display, "\nUse get_execution with byte offset %d to read retained output.", next)
	}
	if ex.OutputTruncated {
		display.WriteString("\nThe daemon reached its retention limit.")
	}
	return toolOutput{display.String(), out}
}

func runeBoundary(s string, offset int) int {
	offset = min(offset, len(s))
	for offset < len(s) && !utf8.RuneStart(s[offset]) {
		offset++
	}
	return offset
}

func outputWindow(stream string, base int64, request *int64, limit int64) (text string, start, next int64, clipped, lost bool) {
	if request == nil {
		from := runeBoundary(stream, max(0, len(stream)-mcpPage))
		next = base + int64(len(stream))
		if from > 0 {
			next = base
		}
		return stream[from:], base + int64(from), next, from > 0, false
	}
	from := runeBoundary(stream, int(min(int64(len(stream)), max(0, *request-base))))
	to := runeBoundary(stream, min(len(stream), from+int(limit)))
	if to-from > mcpPage {
		to = from + mcpPage
		for !utf8.RuneStart(stream[to]) {
			to--
		}
	}
	return stream[from:to], base + int64(from), base + int64(to), to < len(stream), *request < base
}
