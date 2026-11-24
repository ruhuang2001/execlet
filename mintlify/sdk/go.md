---
title: "Go SDK"
description: "Package sandbox: typed lifecycle, execution, and file methods with context-first signatures."
---

```sh
go get github.com/ruhuang2001/tiny-sandbox/sandbox-go@latest
```

The package is `sandbox`, and it is part of the main module:

```go
import sandbox "github.com/ruhuang2001/tiny-sandbox/sandbox-go"
```

## Connect

```go
client, err := sandbox.NewClient(sandbox.Config{
    Domain: os.Getenv("SANDBOX_DOMAIN"),
    APIKey: os.Getenv("SANDBOX_API_KEY"),
})
if err != nil {
    log.Fatal(err)
}
```

A nil `HTTPClient` selects a client **without a global timeout**, so event
streams can stay open. Set one only if you also set a per-request deadline.

`MaxRetries`: `0` selects three retries after the initial attempt, a positive
value sets the limit, and a negative value disables retries entirely.

## Run a command and stream it

```go
sb, err := client.CreateSandbox(ctx, sandbox.CreateSandboxRequest{
    Image:         "python:3.12-alpine",
    TimeoutSeconds: ptr(120),
})
if err != nil { return err }
defer client.KillSandbox(ctx, sb.ID)

exe, err := client.StartExecution(ctx, sb.ID, sandbox.ExecuteRequest{
    Command: []string{"python3", "-c", "print(2 ** 10)"},
})
if err != nil { return err }

final, err := client.StreamExecution(ctx, sb.ID, exe.ID, func(e sandbox.Event) error {
    switch e.Type {
    case sandbox.EventStdout:
        fmt.Print(e.Data)
    case sandbox.EventTruncated:
        log.Printf("output truncated after %d bytes", e.LimitBytes)
    }
    return nil
})
```

`StreamExecution` reconnects on its own using `Last-Event-ID` and returns the
final execution. It returns an error if the callback returns one, and it does
**not** cancel the remote execution in that case — deciding whether to cancel is
yours.

A returned `*Execution` is a snapshot with `State`, `ExitCode`,
`TerminationReason`, and the retained `Stdout`/`Stderr`.

## Errors

```go
var apiErr *sandbox.Error
if errors.As(err, &apiErr) {
    if apiErr.Code == sandbox.CodeBusy {
        // 409 busy: slots are full. Retry after a pause.
    }
    if apiErr.Retryable() {
        // 429, 500, 502, 503, or a transport failure before a full response.
    }
}
```

Sentinel values match with `errors.Is`, so branch on codes rather than strings.

## Concurrency and resources

Optional request fields are pointers, so unset and explicit zero stay distinct.

| Field | Default | Range |
| --- | --- | --- |
| `MaxConcurrentExecutions` | 1 | 1–8 |
| `MemoryMB` | 512 | ≥ 16 |
| `PidsLimit` | 1024 tasks | ≥ 64 |
| `NanoCPUs` | `0` (unlimited) | `1e9` = one CPU |
| `Network` | `sandbox.NetworkBridge` | `NetworkBridge`, `NetworkNone` |

These are fixed at creation and returned on `Sandbox`.

## Execution options

| Field | Rule |
| --- | --- |
| `Env` | ≤ 64 entries; additive to the image's; not inherited by later executions |
| `Cwd` | absolute, ≤ 4096 bytes; empty for the image's working directory |
| `Stdin` | UTF-8, ≤ 1 MiB, one-shot |
| `TimeoutMS` | managed remote deadline; exclusive with nonempty `Stdin` |

Nonempty `Env`, `Cwd`, or `Stdin` triggers a capability preflight. A server that
does not advertise support returns `incompatible_server` **before** anything is
created — an old server silently ignoring a field is worse than an error.

## Files

`ReadFile`, `WriteFile`, `StatFile`, and `ListFiles` cover ordinary access.
`ReadArchive` and `WriteArchive` transfer validated tar archives when you need
many files or want to preserve symlinks, modes, and timestamps. Archive uploads
make **one attempt**: extraction is not safely repeatable.

## Retry rules worth remembering

- Retry candidates: transport errors before a complete response, truncated
  bodies, and 429/500/502/503/504.
- Backoff: `min(2s, 200ms * 2^attempt)`, sampled uniformly from half the cap to
  the cap.
- A positive integer `Retry-After` overrides the delay on ordinary requests.
  SSE reconnects ignore it.
- **Sandbox creation is never retried automatically.** Neither is starting an
  execution unless you supply an `IdempotencyKey`.
- Stream reconnects consume the same retry budget; received output does not
  reset it.
