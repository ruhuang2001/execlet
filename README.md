# tiny-sandbox

Docker-backed code execution for AI agents, with Go, Python, and TypeScript SDKs,
a command-line client, and HTTP API.

- **Lifecycle:** create, pause, resume, and remove sandboxes with resource limits
  and configurable lifetime and idle timeouts.
- **Execution:** argv commands, per-command environment and working directory,
  one-shot stdin, deadlines, process-group cancellation, and idempotent starts.
- **Output:** stdout/stderr over SSE, bounded retention, and reconnect/replay by
  event ID. Outcomes distinguish exits, cancellation, timeout, OOM, eviction,
  and unknown termination.
- **Files:** binary reads/writes, paginated directory listings, and validated tar
  upload/download.
- **Concurrency:** 1–8 execution slots per sandbox; default 1. Pause freezes all
  processes. Sandboxes configured for multiple execution slots report
  ambiguous exit 137 as `unknown`.
- **Background commands:** Python/TypeScript E2B-style handles with guest PID,
  fresh-client attachment, remote deadlines, disconnect and confirmed kill.
- **Interactive shells:** open or reconnect to a PTY with `sandboxctl shell`.
- **MCP clients:** sync a host workspace into an offline test environment with `sandboxctl mcp`. [Setup and example](examples/mcp/README.md)
- **Operations:** structured logs, request IDs, authenticated Prometheus metrics,
  readiness, opt-in deployment budgets/network restrictions and restart recovery.

Documentation on `main` describes the current source. Published binaries can
lag behind it; use the documentation at your release tag for that contract.
Build from this checkout to use all capabilities listed above. Current SDKs
check advertised capabilities before sending optional execution controls to
older servers; see [versioning](docs/versioning.md).

## Install

On macOS or Linux, paste this command to download the latest
release into `$HOME/tiny-sandbox-quickstart`. Docker is required to run it.

Supported platforms are macOS and Linux on amd64 and arm64. Windows is not
supported, and no Windows binaries are published. WSL2 is not a tested or
supported environment.

```sh
curl -fsSL https://raw.githubusercontent.com/ruhuang2001/tiny-sandbox/main/scripts/install.sh | sh
```

## Quick start

Follow the [Quick start tutorial](docs/quickstart.md) to download the latest
release, run Python locally in Docker, and try streaming output and timeouts.
No Go installation or model API key is needed.

## Build from source

Requires Go 1.27+, Make, and a reachable Docker daemon.

```sh
git clone https://github.com/ruhuang2001/tiny-sandbox.git
cd tiny-sandbox
make build
```

The Make target cross-compiles and embeds both Linux guest agents. A direct
`go install` of `sandboxd` from a clean module download builds the legacy
fallback. Use `make build` or a release archive for guest commands, terminals
and filtered networking.

Select the Docker Unix socket as shown in the [quick start](docs/quickstart.md#2-start-the-server-terminal-a),
then start `./bin/sandboxd` and use `./bin/sandboxctl` from a
second terminal. See the [CLI reference](docs/cli.md) for commands.

## Integrate

Use the [Go SDK](docs/sdk.md), [Python SDK](docs/sdk-python.md), or
[TypeScript SDK](docs/sdk-typescript.md) to manage sandboxes and execute
commands from your application, or connect directly through the
[HTTP API](docs/api.md) and [OpenAPI specification](api/openapi.yaml).

The SDK references include installation instructions. A Go `@latest` dependency
selects a published version; use a matching commit when developing against
unreleased server features.

Install the synchronous, standard-library-only Python SDK (Python 3.10+)
from this checkout:

```sh
python3 -m pip install ./sandbox-python
```

Import it as `tiny_sandbox`; the [Python reference](docs/sdk-python.md) includes
streaming, typed errors, cancellation, and sandbox context managers.

The [agent tool example](examples/agent-tool/README.md) shows how to expose
the SDK as a `run_python` tool with structured results and error handling.

Migrating E2B basic Sandbox applications? The optional `tiny_sandbox.e2b` and
`@tiny-sandbox/sdk/e2b` entry points implement a [bounded compatibility subset](docs/e2b-compatibility.md),
with a [local image and migration example](examples/e2b/README.md). No E2B
Cloud account or key is required. Build the adapters and server from the same
checkout; this is not full E2B or HTTP-protocol compatibility.

## Scope

The server uses Docker container isolation, not hardened isolation or tenant
authorization. State is in memory by default. Opt-in `-state-dir` preserves
completed results and reconciles idle containers; interrupted commands become
`unknown` and their containers are removed, never replayed. This is not process
memory restoration or E2B persistence parity.

Opt-in `-policy` enforces aggregate reservations, image and runtime allowlists, named templates, and prewarmed capacity.
Network modes include offline `none`, HTTPS domain-filtered `filtered`, and unrestricted `bridge`. The guest agent runs managed commands as the image user. Non-root images expose
`protected: true` and keep the control socket and supervisor private. Root images
permit same-UID access. Archive uploads require coordinated writers and are not
transactional once extraction begins. See [deployment and recovery](docs/operations.md),
[design](docs/design.md), the [archive contract](docs/api.md#tar-transfer), and
[future directions](docs/future.md).

## Documentation

- [Quick start](docs/quickstart.md)
- [Architecture and execution flows](docs/design.md)
- [CLI reference](docs/cli.md)
- [Go SDK reference](docs/sdk.md)
- [Python SDK reference](docs/sdk-python.md)
- [TypeScript SDK reference](docs/sdk-typescript.md)
- [E2B compatibility subset and acceptance](docs/e2b-compatibility.md)
- [Background-command contract](docs/e2b-background-commands.md)
- [Deployment policy, recovery and metrics](docs/operations.md)
- [End-to-end acceptance and measured startup times](docs/testing/four-directions.md)
- [HTTP API reference](docs/api.md)
- [Versioning and releases](docs/versioning.md)
- [Contributing](CONTRIBUTING.md)

## License

[MIT](LICENSE)
