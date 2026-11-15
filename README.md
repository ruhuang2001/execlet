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
- **Operations:** structured logs, request IDs, process liveness at `/healthz`,
  and Docker readiness at `/readyz`.

Documentation on `main` describes the current source. Published binaries can
lag behind it; use the documentation at your release tag for that contract.
Build from this checkout to use all capabilities listed above. New SDK fields
may be silently ignored by older servers; see [versioning](docs/versioning.md).

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

## Scope

The server uses Docker container isolation and keeps its state in memory.
It does not provide hardened isolation or multi-tenant access controls. Server
restarts lose execution history; owner-label cleanup reclaims its containers,
best effort when Docker is unavailable. Archive uploads require coordinated
writers and are not transactional once extraction begins. See [design and
limitations](docs/design.md), the [archive contract](docs/api.md#tar-transfer),
and [unimplemented future directions](docs/future.md).

## Documentation

- [Quick start](docs/quickstart.md)
- [Architecture and execution flows](docs/design.md)
- [CLI reference](docs/cli.md)
- [Go SDK reference](docs/sdk.md)
- [Python SDK reference](docs/sdk-python.md)
- [TypeScript SDK reference](docs/sdk-typescript.md)
- [HTTP API reference](docs/api.md)
- [Versioning and releases](docs/versioning.md)
- [Contributing](CONTRIBUTING.md)

## License

[MIT](LICENSE)
