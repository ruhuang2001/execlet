# Run repository tests from an MCP client

## Try the packaged pytest demo

The default installer only installs the binaries. For the demo, unpack the full `execlet_<version>_<os>_<arch>.tar.gz` asset from a [release](https://github.com/ruhuang2001/execlet/releases) or CI snapshot that contains `examples/mcp/demo`. The v0.0.3 archive does not contain it. If you don't have an archive with the demo, run `make build` from the current source checkout and use the `bin/` paths below instead of `./execletd` and `./execletctl`.

The demo starts with one failing test. With an archive that includes it, you only need Docker and an MCP client on the host.

From the unpacked directory, build the test image and start the daemon:

```sh
docker build -t execlet-mcp:local examples/mcp
export DOCKER_HOST="${DOCKER_HOST:-$(docker context inspect --format '{{.Endpoints.docker.Host}}')}"
./execletd
```

In a second terminal in the same directory, generate the Cursor configuration:

```sh
./execletctl doctor mcp --workspace examples/mcp/demo --collect
```

Open `examples/mcp/demo` in your client and copy the JSON into `.cursor/mcp.json` (merge the `execlet` entry if the file already exists). If the daemon requires authentication, supply `SANDBOX_API_KEY` in the MCP client's environment; the generated configuration never copies that key. Without `--collect`, the check only probes pytest in the image and doesn't upload files. With `--collect`, it syncs the workspace into a temporary offline sandbox and imports test modules, catching missing dependencies used during collection, not dependencies imported only while tests run.

The image runs as UID 1000 and includes Python 3, pytest, Git and CA certificates. Its packages are installed at image build time. The MCP server creates offline sandboxes by default. Repeat `--image` to allow more images. The first is the default. Add `--allow-network filtered` for a daemon configured with filtered egress or `--allow-network bridge` to permit a bridged sandbox.

Try this request:

> Create an offline sandbox, sync this workspace, and run `python3 -m pytest -q`. It should fail with `assert 21 == 14`. Fix `calculator.py` in my local workspace, sync again without reset, run the same test, and destroy the sandbox. Show both test results and the number of files in the second upload.

For your own pytest project, point `--workspace` to its directory. Install its locked dependencies when building the image, then pass `--image` to use it. Rebuild the image if the dependency lock changes.

## Try a real Go repository

From the execlet source checkout, prepare an image using just the pinned project's dependency files:

```sh
git clone --branch v0.4.3 --depth 1 https://github.com/google/jsonschema-go.git /tmp/jsonschema-go
test "$(git -C /tmp/jsonschema-go rev-parse HEAD)" = 8c4ab4f02ef64dcea5502e47a6113e8292944087
docker build -t execlet-mcp-go:local -f examples/mcp/go/Dockerfile /tmp/jsonschema-go
```

Run `./bin/execletctl doctor mcp --profile go --workspace /tmp/jsonschema-go --check-deps` and give the printed configuration to your MCP client. The build downloads and verifies dependencies from `go.mod` and `go.sum`. The container runs with `GOPROXY=off` and `GOTOOLCHAIN=local`; the source and tests arrive through MCP sync. To try the repair loop, change `do not form a tree` in `jsonschema/resolve.go` to `unexpected schema tree` on the host. Sync and run `go test -p=1 -count=1 -run '^TestSchemaStructure$' ./jsonschema`. Restore the original string, sync again, and rerun in the same sandbox. Only that source file needs to upload. If `go.mod` or `go.sum` changes, rebuild the image and recreate the sandbox before syncing and testing again.

## How sync works

`sync_workspace` uploads to `/tmp/execlet-workspace`. Commands default to that directory after the first sync. Before that they use the image working directory. Host edits drive the next sync. Reset replaces the contents of the sandbox workspace with a fresh host copy. Large workspaces upload in bounded batches and each individual file must fit within 16 MiB. After Cursor restarts, use the existing sandbox ID with the same workspace, daemon, credentials and allowed images and networks. Incremental sync resumes from the saved manifest. A tool call renews the sandbox’s lifetime, which defaults to 1,800 seconds. Set `--sandbox-ttl` to change it. After a disconnect the daemon reclaims remaining sandboxes when their lifetime expires.

An unchanged sync hashes the host files but skips guest work. Use `reset: true` to replace guest contents if they were edited inside the sandbox. See the [maintainer checks](../../CONTRIBUTING.md#mcp-acceptance-and-measurements) for regression, release archive, benchmark and Amp acceptance commands.

Reconnect does not span the rename: `execletctl` uses `~/.cache/execlet/mcp` instead of `~/.cache/tiny-sandbox/mcp`, and checks a different ownership scope even with the same workspace and credentials. Finish sessions and destroy old sandboxes with the old CLI; do not copy receipts. Regenerate client configuration with the new CLI and start a new sandbox.
