---
title: "Release history"
description: "The Execlet rename and earlier releases under the tiny-sandbox name."
---

## Unreleased

### Renamed from tiny-sandbox

The project is now **Execlet**. The renamed SDKs are not available from a
registry. Build and install from the [same checkout](/installation#use-the-sdks-from-a-checkout)
as the daemon.

| Before | After |
| --- | --- |
| `sandboxd` | `execletd` |
| `sandboxctl` | `execletctl` |
| `github.com/ruhuang2001/tiny-sandbox/sandbox-go` | `github.com/ruhuang2001/execlet/sandbox-go` |
| Python distribution `tiny-sandbox`, import `tiny_sandbox` | distribution and import `execlet` |
| npm package `@tiny-sandbox/sdk` | `@execlet/sdk` |

The Go import path change is breaking. Keep existing builds pinned to their
original revisions until you update the imports.

Before changing daemons, [drain the old deployment](/operations/self-hosting#upgrading):
even a stopped container with the old owner label blocks startup. Update
[Prometheus rules](/operations/metrics) from `tiny_sandbox_*` to `execlet_*` and
finish [MCP sessions](/guides/mcp#how-sync-actually-behaves) with the old CLI;
neither metric names nor MCP ownership receipts have compatibility aliases.

## History

Earlier tags are preserved. **v0.0.3 is a historical release**: its archives
contain `sandboxd` and `sandboxctl`; they do not provide the renamed binaries
in the [quickstart](/quickstart).

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

The server ignores unknown request fields. Before using a feature added after
your daemon's build, check `/v1/capabilities` rather than assuming a successful
request applied the new field. Use matching daemon, CLI, and SDK revisions for
these examples. Removing a field, changing a unit, or reinterpreting a value
is a breaking change.

Clients must tolerate added response fields and preserve unknown string values.
The Python SDK keeps extras in `extra`; its execution helpers raise an error
with the original reason when the reason is unknown.
