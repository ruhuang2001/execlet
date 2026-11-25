# Quick start

Download the latest stable release, execute real Python, and try streaming
output and timeouts.
You need a running Docker daemon and the Docker CLI. No Git
clone, Go installation, local Python, or model API key is needed. First use
requires network access to GitHub and the container image registry.

This walkthrough supports macOS and Linux on amd64 and arm64, with a reachable
Docker Unix socket. Windows is not supported, and no Windows binaries are
published. WSL2 is not a tested or supported environment.

The installer downloads published binaries, while documentation on `main`
describes current source. Check `./execletctl version` and `./execletctl help`
for the installed build and use the matching release-tag documentation. To try
unreleased features, [build from source](#build-from-source).

## 1. Install

Download and install the latest release:

```sh
curl -fsSL https://raw.githubusercontent.com/ruhuang2001/execlet/main/scripts/install.sh | sh
```

The installer selects your platform and puts only `execletd` and `execletctl` in
`$HOME/execlet-quickstart`. No sudo is needed. It does not install examples.
For a historical release, skip the install step in the tagged quickstart. In
both terminals use the directory printed by this installer, not the old
`tiny-sandbox-quickstart` directory. Use `./execletctl` even when the installed
CLI prints a cleanup hint with the old `sandboxctl` name.
To try the offline pytest demo, follow the [MCP guide](../examples/mcp/README.md#try-the-packaged-pytest-demo).

## 2. Start the server (terminal A)

Start Docker, then run:

```sh
cd "$HOME/execlet-quickstart"
docker info
export DOCKER_HOST="${DOCKER_HOST:-$(docker context inspect --format '{{.Endpoints.docker.Host}}')}"
case "$DOCKER_HOST" in
  unix://*) ./execletd ;;
  *) echo "A local Docker Unix socket is required; current endpoint: $DOCKER_HOST" ;;
esac
```

Keep this terminal open. The server listens on `127.0.0.1:8080`.

The command uses your existing `DOCKER_HOST`, or discovers the endpoint from
your Docker context. It does not hardcode a user-specific socket path. If it
cannot connect, check `docker info`. You can also specify the Unix socket path
explicitly with `./execletd -docker /path/to/docker.sock`.

## 3. Run Python (terminal B)

Open another terminal and run:

```sh
cd "$HOME/execlet-quickstart"
export SANDBOX_DOMAIN=http://127.0.0.1:8080
./execletctl doctor
./execletctl run --rm -- code 'print(2 ** 10)'
```

`doctor` should finish with `Ready`. The Python command prints `1024`, with
an execution status on stderr containing `completed (exited), exit 0`.
Missing container images are pulled on first use, which needs registry access.

Try changing the Python expression and running it again:

```sh
./execletctl run --rm -- code 'print(sum([19, 29, 49]))'
```

Expected stdout: `97`. Each run creates a fresh sandbox.

`--rm` requests cleanup of the newly created sandbox after execution. Command
stdout goes to stdout; command stderr and execution status go to stderr. Set `SANDBOX_DOMAIN` to connect to another
server and `SANDBOX_API_KEY` when authentication is enabled.

## 4. Watch streaming output

This program prints `1`, `2`, and `3` about one second apart. `flush=True`
makes Python flush each line so you can see the output arrive:

```sh
./execletctl run --rm -- code 'import time
for step in range(1, 4):
    print(step, flush=True)
    time.sleep(1)'
```

## 5. Try an execution timeout

Give a ten-second program a two-second execution deadline:

```sh
./execletctl run --rm --exec-timeout 2 -- code 'import time; time.sleep(10)'
echo "Exit code: $?"
```

The command reports `timeout` and exits with status `1`. This is expected:
the execution deadline stopped the program.

## Finish and troubleshoot

The examples request cleanup with `--rm`. Use `./execletctl list` and
`./execletctl kill SANDBOX_ID` for any sandbox still running or paused. A
`killed` record does not prove Docker removal succeeded, and repeating `kill`
does not retry it. The reaper retries failed removal; startup cleanup with the
same owner label is the fallback. Check Docker directly when cleanup must be
confirmed. Stop the server with Ctrl+C in its terminal.

- If `docker info` fails, start Docker and check access to its daemon.
- If `doctor` fails, follow its printed hint and check `SANDBOX_DOMAIN`.
- If port 8080 is occupied, start `./execletd -addr 127.0.0.1:8081` and set
  `SANDBOX_DOMAIN=http://127.0.0.1:8081` in the client terminal. The container
  owner label follows `-addr` unless you pass `-owner`; pin it when the address
  changes between runs.
- If authentication is enabled, use the same `SANDBOX_API_KEY` in both terminals.

## Build from source

Requires Go 1.27+, Make, and a reachable Docker daemon.

```sh
git clone https://github.com/ruhuang2001/execlet.git
cd execlet
make build
export DOCKER_HOST="${DOCKER_HOST:-$(docker context inspect --format '{{.Endpoints.docker.Host}}')}"
cd bin
case "$DOCKER_HOST" in
  unix://*) ./execletd ;;
  *) echo "A local Docker Unix socket is required; current endpoint: $DOCKER_HOST" >&2 ;;
esac
```

`make build` embeds both Linux guest agents. A direct `go install` of
`execletd` from a clean module download builds the legacy fallback instead.
Use `make build` or a release that includes the guest agents for guest commands,
interactive terminals, and filtered networking.

Continue with the commands in quick start step 3 in another terminal. Use the
repository's `bin` directory instead of the download directory shown there.

## Next steps

- [CLI reference](cli.md): sandbox reuse, lifecycle controls, and logs.
- [Go SDK](sdk.md): integrate execution into your application.
- [Python SDK](sdk-python.md) and [TypeScript SDK](sdk-typescript.md).
- [HTTP API](api.md): per-execution input, file transfer, concurrency and limits.
- [Agent tool example](../examples/agent-tool/README.md): expose Python execution as an agent tool.
