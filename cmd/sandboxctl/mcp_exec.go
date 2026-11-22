package main

import (
	"context"
	"errors"
	"fmt"
	"os"
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
	ex, err := s.client.StartExecution(ctx, in.SandboxID, sandbox.ExecuteRequest{Command: in.Command, Cwd: cwd, TimeoutSeconds: in.Timeout, IdempotencyKey: in.Key})
	if err != nil {
		return toolOutput{}, err
	}
	wait, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for ex.State == "running" {
		select {
		case <-wait.Done():
			if ctx.Err() != nil {
				return toolOutput{}, ctx.Err()
			}
			return executionResult(ex, nil), nil
		case <-tick.C:
			next, err := s.client.GetExecution(wait, in.SandboxID, ex.ID)
			if wait.Err() != nil && ctx.Err() == nil {
				return executionResult(ex, nil), nil
			}
			if err != nil {
				return toolOutput{}, err
			}
			ex = next
		}
	}
	return executionResult(ex, nil), nil
}

func (s *sandboxTools) getTool(ctx context.Context, in getInput) (toolOutput, error) {
	if in.Offset != nil && *in.Offset < 0 {
		return toolOutput{}, errors.New("invalid_request: offset must be non-negative")
	}
	ex, err := s.client.GetExecution(ctx, in.SandboxID, in.ExecutionID)
	if err != nil {
		return toolOutput{}, err
	}
	return executionResult(ex, in.Offset), nil
}

func (s *sandboxTools) cancelTool(ctx context.Context, in executionInput) (toolOutput, error) {
	ex, err := s.client.CancelExecution(ctx, in.SandboxID, in.ExecutionID)
	if err != nil {
		return toolOutput{}, err
	}
	return toolOutput{"Execution " + ex.ID + " is " + ex.State, map[string]any{"state": ex.State, "termination_reason": ex.TerminationReason}}, nil
}

const mcpPage = 64 << 10

func executionResult(ex *sandbox.Execution, offset *int) toolOutput {
	out := map[string]any{"execution_id": ex.ID, "state": ex.State}
	summary := "Execution " + ex.ID + " is " + ex.State
	if ex.State == "running" && offset == nil {
		return toolOutput{summary, out}
	}
	if ex.ExitCode != nil {
		summary += fmt.Sprintf(" (exit %d)", *ex.ExitCode)
	}
	length := max(len(ex.Stdout), len(ex.Stderr))
	start, end := max(0, length-mcpPage), length
	truncated := start > 0
	next := length
	if offset != nil {
		start = min(*offset, length)
		end = start + min(length-start, mcpPage)
		next = end
		truncated = end < length
	} else if truncated {
		next = 0
	}
	window := func(stream string) string {
		from, to := start, end
		if offset == nil {
			from = max(0, len(stream)-mcpPage)
		}
		return stream[runeBoundary(stream, from):runeBoundary(stream, to)]
	}
	out["stdout"], out["stderr"] = window(ex.Stdout), window(ex.Stderr)
	out["exit_code"], out["termination_reason"] = ex.ExitCode, ex.TerminationReason
	out["output_truncated"], out["next_offset"] = truncated || ex.OutputTruncated, next
	return toolOutput{summary, out}
}

func runeBoundary(s string, offset int) int {
	offset = min(offset, len(s))
	for offset < len(s) && !utf8.RuneStart(s[offset]) {
		offset++
	}
	return offset
}
