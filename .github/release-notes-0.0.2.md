# tiny-sandbox v0.0.2

Second public release of tiny-sandbox. This release adds bounded execution,
file, observability and SDK capabilities, plus a tested E2B basic Sandbox
application-source compatibility subset.

## Included

- `sandboxd`, `sandboxctl`, the Go SDK and HTTP/OpenAPI contract.
- Resource-limited Docker sandboxes with environment, working-directory and
  one-shot stdin support; bounded concurrent executions and replayable output.
- Paginated directory listings, bounded tar transfers, request IDs, structured
  logs and Docker readiness checks.
- Python and TypeScript SDK support, plus `tiny_sandbox.e2b` and
  `@tiny-sandbox/sdk/e2b` adapters for the documented E2B basic Sandbox subset.
- Linux and macOS binaries for amd64 and arm64. Each archive contains
  `sandboxd` and `sandboxctl`; verify downloads against `checksums.txt`.

## E2B adapter scope

The adapters were tested from installed Python wheels and npm tarballs against
the local service using 19 cases per language and 294 source-derived checks.
They do not require an E2B Cloud account or key. Use the included image recipe
and compatible server revision. This is application-source compatibility for
the documented subset, not full E2B or HTTP protocol compatibility; PTY,
interactive stdin, background handles and Code Interpreter are not included.

## Security and operational limits

**This project is not hardened isolation and is not suitable for untrusted
multi-tenant workloads.** Execution uses Docker containers; the server has no
multi-tenant access controls. Sandbox and execution state is held in memory and
is lost when the server restarts. The E2B adapter's command timeout stops the
client waiting; it does not stop the remote process. Review the
[design limitations](https://github.com/ruhuang2001/tiny-sandbox/blob/v0.0.2/docs/design.md),
[HTTP contract](https://github.com/ruhuang2001/tiny-sandbox/blob/v0.0.2/docs/api.md)
and [E2B subset](https://github.com/ruhuang2001/tiny-sandbox/blob/v0.0.2/docs/e2b-compatibility.md)
before deployment.

## SDK packages

Python and npm package versions are aligned at `0.0.2`. Their package
distributions are built and tested from this source tree; this GitHub Release
publishes only the four platform binary archives and checksums.
