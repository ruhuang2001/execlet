package agenttool

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ruhuang2001/execlet/internal/guestimage"
	"github.com/ruhuang2001/execlet/internal/wirejson"
	sandbox "github.com/ruhuang2001/execlet/sandbox-go"
)

const ToolName = "run_python"

type Definition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

func ToolDefinition() Definition {
	return Definition{
		Name: ToolName,
		Description: "Run a Python 3 program in an isolated sandbox and return its output. " +
			"The sandbox has no network access assumptions and a memory limit; " +
			"a program that exceeds it is reported as oom, not as a normal failure.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"code": map[string]any{
					"type":        "string",
					"description": "The Python source to run. Printed output is returned to you.",
				},
				"timeout_seconds": map[string]any{
					"type":        "integer",
					"description": "Lifetime of the sandbox in seconds. Default 60.",
					"minimum":     1,
				},
			},
			"required": []string{"code"},
		},
	}
}

type Input struct {
	Code           string `json:"code"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

type Result struct {
	IsError         bool   `json:"is_error"`
	Outcome         string `json:"outcome"`
	ExitCode        *int   `json:"exit_code,omitempty"`
	Stdout          string `json:"stdout,omitempty"`
	Stderr          string `json:"stderr,omitempty"`
	OutputTruncated bool   `json:"output_truncated,omitempty"`
	Message         string `json:"message,omitempty"`
	Hint            string `json:"hint,omitempty"`
}

type Tool struct {
	client *sandbox.Client
	image  string
	memory int
}

func New(client *sandbox.Client) *Tool {
	return &Tool{client: client, image: guestimage.Python, memory: 256}
}

func (t *Tool) Run(ctx context.Context, input Input) Result {
	if input.Code == "" {
		return Result{IsError: true, Outcome: "invalid_input", Message: "code is required"}
	}
	timeout := input.TimeoutSeconds
	if timeout <= 0 {
		timeout = 60
	}

	sb, err := t.client.CreateSandbox(ctx, sandbox.CreateSandboxRequest{
		Image:          &t.image,
		MemoryMB:       &t.memory,
		TimeoutSeconds: &timeout,
	})
	if err != nil {
		return failure(err)
	}
	defer func() {
		killCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_, _ = t.client.KillSandbox(killCtx, sb.ID)
	}()

	return t.run(ctx, sb.ID, input.Code, nil)
}

func (t *Tool) RunIn(ctx context.Context, sandboxID string, input Input) Result {
	if input.Code == "" {
		return Result{IsError: true, Outcome: "invalid_input", Message: "code is required"}
	}
	var execTimeout *int
	if input.TimeoutSeconds > 0 {
		timeout := input.TimeoutSeconds
		execTimeout = &timeout
	}
	return t.run(ctx, sandboxID, input.Code, execTimeout)
}

func (t *Tool) run(ctx context.Context, sandboxID, code string, timeoutSeconds *int) Result {
	started, err := t.client.StartExecution(ctx, sandboxID, sandbox.ExecuteRequest{
		Command:        []string{"python3", "-c", code},
		TimeoutSeconds: timeoutSeconds,
	})
	if err != nil {
		result := failure(err)
		var sdkErr *sandbox.Error
		if errors.As(err, &sdkErr) && sdkErr.Execution != nil {
			fillOutput(&result, sdkErr.Execution)
		}
		return result
	}
	ex, err := t.client.StreamExecution(ctx, sandboxID, started.ID, nil)
	if err != nil {
		result := failure(err)
		if ex != nil {
			fillOutput(&result, ex)
		}
		return result
	}
	if ex != nil && (ex.TerminationReason == nil || *ex.TerminationReason != sandbox.ReasonExited || ex.ExitCode == nil || *ex.ExitCode != 0) {
		result := outcomeFailure(ex)
		fillOutput(&result, ex)
		return result
	}
	result := Result{Outcome: "exited"}
	if ex != nil {
		fillOutput(&result, ex)
	}
	return result
}

func outcomeFailure(ex *sandbox.Execution) Result {
	if ex.TerminationReason != nil {
		switch *ex.TerminationReason {
		case sandbox.ReasonOOM:
			return failure(sandbox.ErrOOM)
		case sandbox.ReasonTimeout:
			return failure(sandbox.ErrTimeout)
		case sandbox.ReasonCanceled:
			return failure(sandbox.ErrCanceled)
		case sandbox.ReasonEvicted:
			return failure(sandbox.ErrEvicted)
		case sandbox.ReasonUnknown:
			return failure(sandbox.ErrUnknown)
		case sandbox.ReasonExited:
			return Result{IsError: true, Outcome: "nonzero_exit", Message: "the program exited with a nonzero status"}
		}
	}
	return failure(sandbox.ErrUnknown)
}

func fillOutput(result *Result, ex *sandbox.Execution) {
	result.Stdout = ex.Stdout
	result.Stderr = ex.Stderr
	result.ExitCode = ex.ExitCode
	result.OutputTruncated = ex.OutputTruncated
}

func failure(err error) Result {
	var ex *sandbox.Execution
	var sdkErr *sandbox.Error
	if errors.As(err, &sdkErr) {
		ex = sdkErr.Execution
	}
	switch {
	case errors.Is(err, sandbox.ErrOOM):
		return Result{
			IsError: true, Outcome: "oom",
			Message: "the program was killed by the kernel for exceeding the memory limit",
			Hint:    "reduce memory use or process the data in chunks",
		}
	case errors.Is(err, sandbox.ErrTimeout):
		return Result{
			IsError: true, Outcome: "timeout",
			Message: "the run timed out before the program finished",
			Hint:    "raise timeout_seconds or make the program faster",
		}
	case errors.Is(err, sandbox.ErrCanceled):
		return Result{
			IsError: true, Outcome: "canceled",
			Message: "the run was canceled before it finished",
		}
	case errors.Is(err, sandbox.ErrEvicted):
		return Result{
			IsError: true, Outcome: "evicted",
			Message: "the sandbox container was stopped outside the server",
			Hint:    "retry the call; the sandbox did not fail on its own",
		}
	case errors.Is(err, sandbox.ErrNonZeroExit):
		result := Result{
			IsError: true, Outcome: "nonzero_exit",
			Message: "the program exited with a nonzero status",
		}
		if ex != nil {
			fillOutput(&result, ex)
		}
		return result
	case errors.Is(err, sandbox.ErrUnknown), errors.Is(err, sandbox.ErrCancelUnconfirmed):
		return Result{
			IsError: true, Outcome: "unknown",
			Message: "the execution outcome could not be confirmed",
			Hint:    "inspect sandbox state before retrying side-effecting code",
		}
	case errors.Is(err, sandbox.ErrBusy):
		return Result{
			IsError: true, Outcome: "unavailable",
			Message: "the sandbox was already running another program",
			Hint:    "wait, or create a separate sandbox",
		}
	default:
		return Result{
			IsError: true, Outcome: "unavailable",
			Message: "the sandbox service could not run the program",
			Hint:    "retry once; if it persists, the service is unavailable",
		}
	}
}

func ResultJSON(result Result) (string, error) {
	data, err := wirejson.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("agenttool: encode result: %w", err)
	}
	return string(data), nil
}
