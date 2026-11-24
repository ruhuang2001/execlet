# Run repository tests from an MCP client

Build the local binaries and the offline Python test image. Start the daemon in a separate terminal.

```sh
make build
docker build -t tiny-sandbox-mcp:local examples/mcp
bin/sandboxd
```

In a second terminal, generate the Cursor configuration for the repository whose files you want to test:

```sh
bin/sandboxctl doctor mcp --workspace /path/to/project
```

Copy the JSON into that project's `.cursor/mcp.json` (merge the `tiny-sandbox` entry if the file already exists). The check runs pytest's version command in a temporary offline sandbox. It does not upload the workspace or check your project's dependencies. If the daemon requires authentication, supply `SANDBOX_API_KEY` in the MCP client's environment; the generated configuration never copies that key.

The image runs as UID 1000 and includes Python 3, pytest, Git and CA certificates. Its packages are installed at image build time. The MCP server creates offline sandboxes by default. Repeat `--image` to allow more images. The first is the default. Add `--allow-network filtered` for a daemon configured with filtered egress or `--allow-network bridge` to permit a bridged sandbox.

Try this request in a project with a failing pytest test.

> Create an offline sandbox, sync this workspace, and run `python3 -m pytest -q`. Read the failure and fix the source in my local workspace. Sync the changes, run the same tests, show both results, and destroy the sandbox.

`sync_workspace` uploads to `/tmp/tiny-sandbox-workspace`. Commands default to that directory after the first sync. Before that they use the image working directory. Host edits drive the next sync. Reset replaces the contents of the sandbox workspace with a fresh host copy. Large workspaces upload in bounded batches and each individual file must fit within 16 MiB. After Cursor restarts, use the existing sandbox ID with the same workspace, daemon, credentials and allowed images and networks. Incremental sync resumes from the saved manifest. A tool call renews the sandbox’s lifetime, which defaults to 1,800 seconds. Set `--sandbox-ttl` to change it. After a disconnect the daemon reclaims remaining sandboxes when their lifetime expires.

Run `python3 scripts/e2e-mcp.py` to exercise the same stdio tools with the official client. The script tests a deadline-conversion regression in a temporary checkout of the Python SDK. It also fetches pinned jsonschema-go and MCP Go SDK repositories on the host and verifies their files inside an offline sandbox. Tool calls, pytest output, renewed deadlines and cleanup evidence are recorded in `artifacts/mcp-e2e/acceptance.json`.

### Amp acceptance

Build the daemon and MCP server with `make build` and the pytest image with `docker build -t tiny-sandbox-mcp:local examples/mcp`. With an active local Amp login, run `python3 scripts/e2e-amp.py --out artifacts/amp`. The harness creates a private Amp run and temporary settings and checkout. Amp invokes the MCP tools, observes a real failing repository test, edits the host source, synchronizes again, reruns pytest and destroys its sandbox. The output directory contains the tool transcript, model-visible pytest output, checked Docker network mode and cleanup results.
