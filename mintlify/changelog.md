---
title: "Changelog"
description: "Release history, starting from the execlet rename."
---

## Unreleased

### Renamed from tiny-sandbox

The project is now **execlet**. The rename happened before the SDKs were
published to any registry, so no published package name is being abandoned.

| Before | After |
| --- | --- |
| `sandboxd` | `execletd` |
| `sandboxctl` | `execletctl` |
| `github.com/ruhuang2001/tiny-sandbox/sandbox-go` | `github.com/ruhuang2001/execlet/sandbox-go` |
| PyPI `tiny-sandbox` | `execlet` |
| npm `@tiny-sandbox/sdk` | `@execlet/sdk` |

<Warning>
The module path change is a **breaking** change for any Go code that imported
the old path. The old releases keep working; the old module path does not
continue.
</Warning>

## History

Earlier releases are preserved under the `tiny-sandbox` name:

| Version | Date | Notes |
| --- | --- | --- |
| v0.0.3 | 2026-11-24 | `PUT .../files` returns 200 with a `FileEntry`; E2B command timeouts enforce a remote deadline |
| v0.0.2 | 2026-11-18 | Python and TypeScript SDKs |
| v0.0.1 | 2026-11-09 | First release |

### Upgrading from v0.0.2 to v0.0.3

`PUT /v1/sandboxes/{sandbox_id}/files` now returns **HTTP 200** with a JSON
`FileEntry` instead of 204 with an empty body. Update callers that check for
204. The Go, Python, and TypeScript clients all accept either response.

E2B `commands.run` timeouts now enforce a remote command deadline for both
foreground and background runs. `commands.connect` timeouts still bound only
local observation. That correction requires a server advertising
`e2b.background.v1`.

## Version numbers

Release tags are `vMAJOR.MINOR.PATCH`, shared by the Go module, both binaries,
and the SDKs. The `/v1` HTTP path prefix is the API namespace, **not** a claim
that the project has reached 1.0.

| Change | Version |
| --- | --- |
| Fix without a contract change | patch |
| Backward-compatible operation, field, or flag | minor |
| Removes or reinterprets public API | major |

Before v1.0.0, breaking changes are allowed but must be called out explicitly.

## What counts as the contract

Wider than the Go API:

- CLI flags, environment variables, and exit codes
- JSON request and response fields
- SDK method signatures, fields, and error types
- Go sentinel values matched by `errors.Is`, Python `Error.code` values
- Timeout units and their meaning
- SSE event types, ID semantics, and `Last-Event-ID` behavior

Additive changes are preferred: the server ignores unknown request fields, so a
newer client can talk to an older server, but the client must not assume the
field took effect. Removing a field, changing a unit, or reinterpreting a value
is a major change.

Clients must tolerate added response fields and preserve unknown string values.
The Python SDK keeps extras in `extra`; unknown termination reasons surface as
errors rather than being quietly mapped to a known one.
