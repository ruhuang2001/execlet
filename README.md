# execlet

Run code for AI agents in local Docker containers. Use `execletctl` from a
terminal, integrate with the Go, Python, or TypeScript SDK, or call the HTTP API.

**Isolation:** Execlet uses Docker's default runtime unless a deployment policy
selects another. On a supported Linux host, use `runtime: "runsc"` with the
same API for gVisor isolation. Review the [runtime guarantees](docs/operations.md#runtime-verification).
Do not use a shared daemon as a security boundary between hostile tenants.

## Quick start

On macOS or Linux (amd64 or arm64), start Docker, then install the latest
release and run the daemon in your first terminal. No Go, local Python, or
model API key is needed.

```sh
curl -fsSL https://raw.githubusercontent.com/ruhuang2001/execlet/main/scripts/install.sh | sh
cd "$HOME/execlet-quickstart"
export DOCKER_HOST="${DOCKER_HOST:-$(docker context inspect --format '{{.Endpoints.docker.Host}}')}"
./execletd
```

In a second terminal, run Python in a fresh sandbox and request cleanup:

```sh
cd "$HOME/execlet-quickstart"
SANDBOX_DOMAIN=http://127.0.0.1:8080 ./execletctl run --rm -- code 'print(2 ** 10)'
```

Expected stdout: `1024`. The first run may need to pull a container image.
Stop the daemon with Ctrl+C when done. See the [full quick start](docs/quickstart.md)
for Docker socket troubleshooting, streaming output, and a timeout example.

The installer installs only `execletd` and `execletctl` from the [latest release](https://github.com/ruhuang2001/execlet/releases/latest).
For the offline pytest demo, [unpack an archive that includes it or build from source](examples/mcp/README.md#try-the-packaged-pytest-demo).
Use the docs at the release tag for the released contract; the links below describe the current checkout.

## What you can do

- Create, pause, resume, and remove sandboxes with resource and lifetime limits.
- Run commands with deadlines, cancellation, and bounded concurrent execution.
  Stream stdout and stderr and reconnect using the last event ID.
- Read and write files, list directories, and transfer validated tar archives.
- Open or reconnect to an interactive terminal with `execletctl shell`.
- Sync a workspace into an offline sandbox from an MCP client with `execletctl mcp`.

## Documentation

| If you want to… | Start here |
| --- | --- |
| Use the CLI | [CLI reference](docs/cli.md) |
| Integrate code execution | [Go](docs/sdk.md), [Python](docs/sdk-python.md), [TypeScript](docs/sdk-typescript.md), [HTTP API](docs/api.md), [OpenAPI](api/openapi.yaml) |
| Give an agent a Python tool | [`run_python` example](examples/agent-tool/README.md) |
| Connect an MCP client | [MCP setup and example](examples/mcp/README.md) |
| Migrate an E2B basic Sandbox application | [Local migration example](examples/e2b/README.md) and [background command contract](docs/e2b-background-commands.md). This is a subset, not E2B protocol compatibility. |
| Deploy and operate the daemon | [Policy, network modes, recovery, and metrics](docs/operations.md). State is in memory by default; opt-in recovery does not restore running processes. |
| Contribute or release | [Contributing](CONTRIBUTING.md), [Architecture](docs/design.md), [Versioning and releases](docs/versioning.md) |

## License

[MIT](LICENSE)
