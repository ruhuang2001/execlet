# tiny-sandbox v0.0.3

This release adds an MCP workspace workflow, interactive terminals, and more
control over how Docker sandboxes are provisioned and operated.

## Highlights

- **MCP workspace workflow:** `sandboxctl mcp` syncs selected host workspace
  files into a sandbox and lets clients run commands and read paged output.
  Sandboxes are offline by default, and workspace sync can resume incrementally.
- **Guest agent and terminals:** `sandboxctl shell` opens or reconnects to a
  PTY. The Linux guest agent also provides file `stat`, metadata on writes,
  and managed command execution. Release binaries embed both Linux guest
  architectures; use `make build` for the equivalent source build.
- **Deployment controls:** opt-in budgets, image and runtime allowlists,
  HTTPS domain-filtered egress, named filesystem templates, and prewarmed
  capacity. Filesystem templates do not snapshot live process memory.
- **E2B background commands:** the Python and TypeScript adapters can start
  commands in the background, reconnect by sandbox ID and guest PID, and
  wait, disconnect, or request confirmed termination. This is application-source
  compatibility for a subset, not E2B HTTP protocol compatibility.
- **Operations:** authenticated Prometheus metrics and opt-in local restart
  recovery. Recovery can preserve completed results and reconcile idle containers;
  interrupted commands are marked unknown, and their containers are removed.

## Upgrade from v0.0.2

- **HTTP file writes:** `PUT /v1/sandboxes/{sandbox_id}/files` now returns
  `200 OK` with a JSON `FileEntry`, instead of `204 No Content`. Update callers
  that require an exact `204`. The current SDKs still accept legacy `204`
  responses without metadata.
- **E2B command timeouts:** foreground and background `commands.run` timeouts
  now enforce a remote command deadline, not just a local waiting budget.
  `commands.connect` timeouts still only limit local observation. The corrected
  command behavior requires a daemon advertising `e2b.background.v1`; upgrade
  the daemon and adapters together to use it.

**Isolation:** Docker container isolation is not a security boundary for
untrusted multi-tenant workloads. Keep the daemon private or behind an
authenticated TLS endpoint. Filtered networking restricts approved HTTPS
destinations; it does not provide tenant isolation.

## Downloads and SDKs

The GitHub Release contains `sandboxd` and `sandboxctl` for Linux and macOS
(amd64 and arm64). Verify the archives against `checksums.txt`. The Python and
npm packages are versioned at `0.0.3`, but registry publication is separate
from this binary release.

- [Quick start](https://github.com/ruhuang2001/tiny-sandbox/blob/v0.0.3/docs/quickstart.md)
- [MCP setup and example](https://github.com/ruhuang2001/tiny-sandbox/blob/v0.0.3/examples/mcp/README.md)
- [Deployment and recovery](https://github.com/ruhuang2001/tiny-sandbox/blob/v0.0.3/docs/operations.md)
- [HTTP API](https://github.com/ruhuang2001/tiny-sandbox/blob/v0.0.3/docs/api.md) and [E2B command contract](https://github.com/ruhuang2001/tiny-sandbox/blob/v0.0.3/docs/e2b-background-commands.md)
