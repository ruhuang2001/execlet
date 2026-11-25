---
title: "Quickstart"
description: "Build Execlet, start the daemon, and run Python in a fresh Docker sandbox."
---

Run `print(2 ** 10)` in a fresh sandbox and get `1024` back. You need the
current source checkout, Go 1.27+, Make, Docker running Linux containers, and
the Docker CLI. No local Python interpreter or model API key is needed.

macOS and Linux on amd64 or arm64 are supported, with a reachable Docker Unix
socket. Windows is not supported. WSL2 is not tested.

## Build the binaries

Run this in the root of the [checkout](/installation#build):

```sh
make build
```

This writes `bin/execletd` and `bin/execletctl`. The historical v0.0.3
archives contain the old binaries and are not used here.

## Start the daemon

From the checkout root, check Docker and start the daemon in your first terminal:

```sh
docker info
export DOCKER_HOST="${DOCKER_HOST:-$(docker context inspect --format '{{.Endpoints.docker.Host}}')}"
./bin/execletd
```

Keep this terminal open. The daemon listens on `127.0.0.1:8080`. If your
Docker context points at TCP or SSH, switch to a local Unix-socket context or
pass the socket's filesystem path with `./bin/execletd -docker /path/to/docker.sock`.

## Run Python

Open a second terminal in the same checkout:

```sh
export SANDBOX_DOMAIN=http://127.0.0.1:8080
./bin/execletctl doctor
./bin/execletctl run --rm --network none -- code 'print(2 ** 10)'
```

`doctor` should end with `Ready`. The Python command prints `1024` to stdout
and `completed (exited), exit 0` in the status line on stderr. The daemon pulls
`alpine:3.20` for `doctor` and a digest-pinned `ghcr.io/astral-sh/uv` Python 3.12 Alpine image for `code` if they are not
available locally. The pull requires registry access from Docker, even though
`--network none` leaves the Python sandbox offline.

Each `run` without `--sandbox` creates a fresh container. `--rm` requests its
removal after the command ends.

## Stream output and try a deadline

Watch Python flush one line per second:

```sh
./bin/execletctl run --rm --network none -- code 'import time
for i in range(1, 4):
    print(i, flush=True)
    time.sleep(1)'
```

Now give a ten-second program a two-second execution deadline:

```sh
./bin/execletctl run --rm --network none --exec-timeout 2 -- code 'import time; time.sleep(10)'
```

The CLI exits with status `1` and reports `timeout`. Branch on the
[termination reason](/concepts/errors), not the command's exit code alone.

## Finish and troubleshoot

Stop the daemon with Ctrl+C in its terminal when done. If the CLI reports
unconfirmed cleanup, check Docker; the daemon retries pending removals in the
background. A `killed` record alone is not proof the container is gone.

| Symptom | Check |
| --- | --- |
| `execletd` cannot reach Docker | Run `docker info` and check that `DOCKER_HOST` is a reachable `unix://` endpoint |
| Port 8080 is already in use | Start the daemon with `-addr 127.0.0.1:8081` and change `SANDBOX_DOMAIN` to match. Keep `-owner` stable if reusing a deployment |
| `doctor` reports an authentication failure | If the daemon uses `SANDBOX_API_KEY`, set the same value in the client terminal |
| The first run is slow or fails to pull an image | Check Docker's registry access or pre-pull the images |

## Next steps

- [CLI reference](/cli): reuse a sandbox, stream an existing execution, or transfer files.
- [Sandbox lifecycle](/concepts/lifecycle): see what pause, resume, and deletion preserve.
- [SDK installation](/installation#use-the-sdks-from-a-checkout): call the same daemon from your application.
