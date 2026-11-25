---
title: "Go SDK"
description: "Package sandbox: typed lifecycle, execution, and file methods with context-first signatures."
---

## Use the local SDK

The SDK is package `sandbox` in the main module. Set `EXECLET_SRC` to the
absolute path of the [checkout](/installation#use-the-sdks-from-a-checkout)
that built your daemon. From a new directory for this example:

```sh
go mod init example.com/execlet-demo
go mod edit -require=github.com/ruhuang2001/execlet@v0.0.0
go mod edit -replace="github.com/ruhuang2001/execlet=$EXECLET_SRC"
```

The replacement is local; `v0.0.0` is a placeholder, not a published tag.

## Run a command and stream it

Start the daemon as shown in the [quickstart](/quickstart#start-the-daemon).
Save this program as `main.go`:

```go
package main

import (
    "context"
    "errors"
    "fmt"
    "log"
    "time"

    "github.com/ruhuang2001/execlet/sandbox-go"
)

func main() {
    if err := run(context.Background()); err != nil {
        log.Fatal(err)
    }
}

func run(ctx context.Context) (result error) {
    client, err := sandbox.NewClient(sandbox.Config{})
    if err != nil {
        return err
    }
    sb, err := client.CreateSandbox(ctx, sandbox.CreateSandboxRequest{
        Image:          new("python:3.12-alpine"),
        Network:        new(sandbox.NetworkNone),
        TimeoutSeconds: new(120),
    })
    if err != nil {
        return err
    }
    defer func() {
        cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
        defer cancel()
        _, err := client.KillSandbox(cleanup, sb.ID)
        result = errors.Join(result, err)
    }()

    _, err = client.ExecStream(ctx, sb.ID, []string{"python3", "-c", "print(2 ** 10)"}, func(event sandbox.Event) error {
        if event.Type == sandbox.EventStdout {
            _, err := fmt.Print(event.Data)
            return err
        }
        return nil
    })
    return err
}
```

```sh
go mod tidy
go run .
```

It prints `1024`. Empty `Domain` and `APIKey` fall back to `SANDBOX_DOMAIN`
and `SANDBOX_API_KEY`. Without a configured domain the client uses
`http://localhost:8080`. A nil `HTTPClient` selects a client with no global
timeout; use contexts for deadlines instead of cutting off all event streams.

`Exec` and `ExecStream` start work and return an error for a failed command.
For a custom request, call `StartExecution` with an `ExecuteRequest`, then
`StreamExecution` with the returned ID. `StreamExecution` reconnects using
`Last-Event-ID` and returns the final snapshot even for a failed command.
Its `ExitCode` and `TerminationReason` are pointers; check for nil before
dereferencing. A callback error closes observation only. Canceling the
context asks the SDK to cancel remotely; check for `ErrCancelUnconfirmed`
before assuming the command stopped.

## Errors

```go
switch {
case errors.Is(err, sandbox.ErrBusy):
    log.Print("execution slots are full")
case errors.Is(err, sandbox.ErrTimeout):
    log.Print("the command deadline expired")
default:
    if sdkErr, ok := errors.AsType[*sandbox.Error](err); ok {
        log.Printf("%s (retryable: %t)", sdkErr.Code, sdkErr.Retryable())
    }
}
```

`ErrBusy` means wait for a slot to free. `Retryable()` classifies the error;
it does not make the operation safe to replay.

## Concurrency and resources

Optional request fields are pointers, so unset and explicit zero stay distinct.

| Field | Default | Range |
| --- | --- | --- |
| `MaxConcurrentExecutions` | 1 | 1–8 |
| `MemoryMB` | 512 | ≥ 16 |
| `PidsLimit` | 1024 tasks | ≥ 64 |
| `NanoCPUs` | `0` (unlimited) | `1e9` = one CPU |
| `Network` | `sandbox.NetworkBridge` without a policy | `NetworkBridge`, `NetworkNone`, `NetworkFiltered` (requires policy) |
| `Mounts` | None | Up to eight [persistent volumes](/operations/volumes), specified as `[]sandbox.VolumeMount` |

These are fixed at creation and returned on `Sandbox`.

## Execution options

| Field | Rule |
| --- | --- |
| `Env` | ≤ 64 entries; additive to the image's; not inherited by later executions |
| `Cwd` | absolute, ≤ 4096 bytes; empty for the image's working directory |
| `Stdin` | UTF-8, ≤ 1 MiB, one-shot |
| `TimeoutSeconds` | native remote deadline; unset or `0` means no command deadline |
| `TimeoutMS` | managed remote deadline; mutually exclusive with `TimeoutSeconds` and with nonempty `Stdin` |

The SDK checks capabilities before sending requests that use additive features
such as `Runtime`, nonempty `Env`, `Cwd` or `Stdin`, and `TimeoutMS`. Missing
support gives `incompatible_server` before the affected operation is sent.

## Files

`ReadFile`, `WriteFile`, `StatFile`, and `ListFiles` cover ordinary access.
Use `WriteFileInfo` to get metadata from a successful file write.
`ReadArchive` and `WriteArchive` transfer validated tar archives when you need
many files or want to preserve symlinks, modes, and timestamps. Archive uploads
make **one attempt**: extraction is not safely repeatable.

## Volumes

When the daemon has [persistent volumes](/operations/volumes) enabled,
`CreateVolume`, `GetVolume`, `ListVolumes`, and `DeleteVolume` manage them.
Attach a volume by setting `Mounts` on the create request:

```go
vol, err := client.CreateVolume(ctx, sandbox.CreateVolumeRequest{SizeBytes: 16 << 20})
if err != nil {
    return err
}
sb, err := client.CreateSandbox(ctx, sandbox.CreateSandboxRequest{
    Image:  "alpine:3.20",
    Mounts: []sandbox.VolumeMount{{VolumeID: vol.ID, Path: "/mnt/work"}},
})
```

The SDK requires the `volumes.v1` capability before any volume operation or a
create with mounts. It rejects a size that is not a whole MiB between 16 MiB
and 1 TiB, more than eight mounts, a repeated volume, and paths that overlap or
are not canonical paths below `/mnt` before sending the request. Set
`ReadOnly: true` on a mount to deny writes. `GetVolume` and `ListVolumes`
only return ready volumes.

## Retry rules worth remembering

- `MaxRetries: 0` selects three retries after the initial attempt; a positive
  value sets the limit, and a negative value disables retries.
- Retry candidates: transport errors before a complete response, truncated
  bodies, and 429/500/502/503/504.
- Backoff: `min(2s, 200ms * 2^attempt)`, sampled uniformly from half the cap to
  the cap.
- A positive integer `Retry-After` overrides the delay on ordinary requests.
  SSE reconnects ignore it.
- **Sandbox creation is never retried automatically.** `StartExecution` uses
  the supplied `IdempotencyKey` or generates one and keeps it across retries.
- Stream reconnects consume the same retry budget; received output does not
  reset it.
- `CreateVolume` is not retried. `DeleteVolume` retries only an explicit HTTP
  503. If its response is lost, it returns the transport error without
  replaying. Repeat the delete to reconcile: `204` confirms cleanup and `404`
  means the ID is absent.
