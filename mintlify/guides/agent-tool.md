---
title: "Give an agent a run_python tool"
description: "A tool adapter between a model's function call and a sandboxed execution."
---

An agent that writes code needs a tool that runs it. The shape is always the
same three layers, and only the middle one is your code:

```text
model ──► tool call ──► YOUR ADAPTER ──► execlet ──► Docker
  ▲                                                     │
  └────────── tool result (stdout, reason) ◄────────────┘
```

The model never talks to execlet. Your application owns the loop: it declares
the tool, dispatches the call, and turns the execution result into a message.

The [`examples/agent-tool`](https://github.com/ruhuang2001/tiny-sandbox/tree/main/examples/agent-tool)
directory contains a complete Go adapter you can copy.

## What the tool definition should expose

Keep it small. A `run_python` tool with one string argument is enough for most
agents, and every extra parameter is one more thing the model can get wrong.

| Field | Why it is there |
| --- | --- |
| `code` | The only required input |
| `stdin` | Optional; covers programs that read input |
| `timeout_seconds` | Optional; lets the agent ask for less time, never more than your ceiling |

Do not expose `image`, `network`, or `memory` to the model. Those are your
policy decisions, and a model that can widen its own sandbox is not a sandbox.

## The four decisions your adapter makes

1. **One sandbox per call, or a reused one?** Fresh is safer and slower;
   reused keeps installed packages and files. Start fresh.
2. **What happens on timeout?** Return the typed reason to the model, not a
   stack trace. "Your code exceeded 10 seconds" is actionable; `exit 137` is not.
3. **Who cleans up?** Defer a kill. An agent loop that forgets to kill leaves
   containers behind on every failure path.
4. **What goes back to the model?** stdout, stderr, the reason, and the exit
   code. Truncate stdout before sending it — a runaway print loop can produce
   megabytes of output that will cost you tokens and tell the model nothing.

## A complete adapter, in outline

The image, the deadline, and `NetworkNone` are the adapter's decisions. The
model supplies code and nothing else.

```go
func (t *Tool) Run(ctx context.Context, code string) (Result, error) {
    sb, err := t.client.CreateSandbox(ctx, sandbox.CreateSandboxRequest{
        Image:          "python:3.12-alpine",
        TimeoutSeconds: ptr(30),
        Network:        ptr(sandbox.NetworkNone),
    })
    if err != nil {
        return Result{}, err
    }
    defer func() {
        cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()
        _ = t.client.KillSandbox(cleanupCtx, sb.ID)
    }()

    var stdout, stderr strings.Builder
    final, err := t.client.StartExecution(ctx, sb.ID, sandbox.ExecuteRequest{
        Command:  []string{"python3", "-c", code},
        TimeoutMS: ptr(t.execDeadline.Milliseconds()),
    })
    if err != nil {
        return Result{}, err
    }
    final, err = t.client.StreamExecution(ctx, sb.ID, final.ID, func(e sandbox.Event) error {
        switch e.Type {
        case sandbox.EventStdout:
            if stdout.Len() < maxToolOutput {
                stdout.WriteString(e.Data)
            }
        case sandbox.EventStderr:
            if stderr.Len() < maxToolOutput {
                stderr.WriteString(e.Data)
            }
        }
        return nil
    })
    if err != nil {
        return Result{}, err
    }

    return Result{
        IsError: final.TerminationReason != sandbox.ReasonExited || final.ExitCode != 0,
        Stdout:  truncate(stdout.String(), maxToolOutput),
        Stderr:  truncate(stderr.String(), maxToolOutput),
        Reason:  final.TerminationReason,
        ExitCode: final.ExitCode,
    }, nil
}
```

The returned `Reason` is the server's typed classification. It is what lets the
model act on a failure instead of guessing from exit code 137.

## Why the reason field is the whole point

Most agent tools hand the model an exit code and a wall of stderr. The model
then guesses what went wrong and usually guesses "syntax error". Passing the
termination reason changes what the agent can do next:

| Reason | What a good agent does |
| --- | --- |
| `exited` with non-zero | Read stderr, fix the code |
| `timeout` | Simplify the task, or stop looping |
| `oom` | Use less memory, or stream results instead of building a list |
| `canceled` | The user interrupted; do not retry |
| `evicted` | Infrastructure; retry once |
| `unknown` | Log it and stop. Repeated retries here are an infinite loop |

## Verifying the adapter

Test it against the three cases that actually break adapters, not against
`print("hello")`:

```text
infinite loop           → must return timeout, not hang the host
print 100 MB             → must truncate, and the tool must still return
sys.exit(1)              → must be is_error, with stderr preserved
```

Then check the container count after each run. An adapter that leaks one
container per failed call looks fine in a demo and fills a host in an hour.
