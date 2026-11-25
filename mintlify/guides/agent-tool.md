---
title: "Give an agent a run_python tool"
description: "A tool adapter between a model's function call and a sandboxed execution."
---

Expose `run_python` to a model without giving it direct access to your daemon.
Your application dispatches the tool call and sends the result back to the model:

```text
model ──► tool call ──► YOUR ADAPTER ──► execlet ──► Docker
  ▲                                                     │
  └────────── tool result (stdout, reason) ◄────────────┘
```

The [`examples/agent-tool`](https://github.com/ruhuang2001/execlet/tree/main/examples/agent-tool)
directory implements the adapter with the [Go SDK](/sdk/go). It does not
include a model client or send messages to a model.

## Run the example

From the checkout root, print the tool definition, then run real Python against
the [quickstart daemon](/quickstart#start-the-daemon):

```sh
go run ./examples/agent-tool/cmd/demo -definition
SANDBOX_DOMAIN="${SANDBOX_DOMAIN:-http://127.0.0.1:8080}" \
  go run ./examples/agent-tool/cmd/demo -code 'print(sum([10, 20, 30]))'
```

The second command should return the following fields. Without `SANDBOX_DOMAIN`,
the demo uses an in-memory stub that does not run Python.

```json
{"is_error": false, "outcome": "exited", "exit_code": 0, "stdout": "60\n"}
```

## Wire it into your agent

1. Create the adapter with `agenttool.New(client)` and use
   `agenttool.ToolDefinition()` to declare the tool in your model's format.
2. Decode each call into `agenttool.Input`, validate it, and pass it to
   `tool.Run(ctx, input)`. `code` is required; `timeout_seconds` is optional
   and defaults to 60. It sets the new sandbox's lifetime, not CPU time.
   There is no stdin field and the example does not clamp the requested
   lifetime. Set a maximum in your application.
3. Use `agenttool.ResultJSON(result)` to serialize the result. Return it with
   the original call ID. Check `is_error` and `output_truncated`, and cap
   what you send to the model to fit its context budget.

`Run` uses a fresh Python 3.12 Alpine sandbox with uv, pip and 256 MiB of memory. It
leaves the network mode to the daemon; use a [policy](/operations/policy)
with `allowed_networks: ["none"]` if the workload must run offline. Keep
image, network, and resource settings under application control. The example
requests cleanup with a separate timeout but ignores cleanup errors. In an
application, log and reconcile unconfirmed removals.

To reuse files, create a sandbox yourself and call
`tool.RunIn(ctx, sandboxID, input)`. Each call still starts a new Python
process. Here a positive `timeout_seconds` limits the command, not the
sandbox lifetime, and cleanup is the caller's responsibility.

## Handle failures

Branch on the adapter's `outcome`, not just its `exit_code`:

| Outcome | Next step |
| --- | --- |
| `exited` | Use the output; the process exited with status 0 |
| `nonzero_exit` | Inspect stderr and correct the code |
| `timeout` | Simplify the task or adjust the deadline within your budget |
| `oom` | Reduce memory use or process data in smaller batches |
| `canceled` | Respect the cancellation; do not retry automatically |
| `evicted` / `unavailable` | Check daemon and sandbox state before starting more work |
| `unknown` | The outcome or cancellation could not be confirmed; reconcile any side effects before retrying |
| `invalid_input` | Supply nonempty code |

For side-effecting work, keep the execution ID at the SDK layer so you can
inspect it later; the tool result does not include it. Test a deadline, a
nonzero exit, and output truncation against a real daemon, and check for
leftover containers after each run.
