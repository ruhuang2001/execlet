---
title: "Build from source"
description: "Pin a commit, try unreleased features, or work on execlet itself."
---

The installer is the right path for using execlet. Build from source when you
need a commit the release does not contain, a platform binary you control, or a
modifiable daemon.

## Requirements

| Tool | Version | Needed for |
| --- | --- | --- |
| Go | 1.27+ | daemon, CLI, Go SDK |
| Docker Engine + CLI | with a Unix socket | running anything |
| Node | 22.19+ | TypeScript SDK build and tests |
| Python | 3.11+ (3.10+ for the native SDK) | Python SDK, and the e2e harness |

<Note>
The 3.11 floor comes from the E2B reference harness. The native Python SDK
itself supports 3.10.
</Note>

## Build

```sh
git clone https://github.com/ruhuang2001/tiny-sandbox
cd tiny-sandbox
make build
```

`make build` also cross-compiles the in-container guest agent for linux/amd64
and linux/arm64 and embeds it, so the daemon carries its own helper and the
target host needs no toolchain. The binaries land in `bin/`.

## Verify before trusting a checkout

```sh
go build ./...
go vet ./...
go test -race ./...
golangci-lint run ./...
```

Then, against a real Docker daemon. `feature-e2e` covers MCP, guest transport,
terminals, egress, templates, and prewarm; `runtime-e2e` covers admission and
limits under real runc and runsc:

```sh
make feature-e2e
make runtime-e2e
make python-e2e
make typescript-e2e
```

These are the same suites CI gates releases on. A green `go test` alone does not
prove the Docker backend works; `feature-e2e` and `runtime-e2e` are what verify
the daemon against a real engine.

## Run your build

```sh
export DOCKER_HOST="${DOCKER_HOST:-$(docker context inspect --format '{{.Endpoints.docker.Host}}')}"
./bin/execletd
```

In another terminal:

```sh
export SANDBOX_DOMAIN=http://127.0.0.1:8080
./bin/execletctl doctor
```

## Use a checkout from another project

Go:

```sh
go get github.com/ruhuang2001/tiny-sandbox/sandbox-go@<commit>
```

Python:

```sh
python3 -m pip install ./sandbox-python
```

TypeScript:

```sh
cd sandbox-ts && npm ci && npm pack
npm install /path/to/execlet-sdk-<version>.tgz
```

<Warning>
`@latest` selects a published module version, which may lag the source. When
you test an unreleased server feature, pin the client to the same commit as the
daemon. Mixed revisions are not supported, and a newer client against an older
server is expected to fail capability checks rather than silently degrade.
</Warning>
