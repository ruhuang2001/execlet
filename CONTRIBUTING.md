# Contributing

Requires macOS or Linux, Go 1.27 or newer, and a reachable Docker Unix socket.
Windows and WSL2 are not supported. Build the server and
CLI with `make build`; see the [quick start](README.md#quick-start) for usage.

## Local checks

```sh
make test     # unit tests
make race     # race detector
make vet      # go vet
make lint     # golangci-lint
```

```sh
E2E_COVERAGE=1 bash scripts/e2e.sh artifacts/e2e
mkdir -p artifacts/cov-unit
go test -race -covermode=atomic -coverpkg=./... ./... \
  -args -test.gocoverdir="$PWD/artifacts/cov-unit"
bash scripts/coverage.sh artifacts/cov-unit artifacts/e2e/coverage 70
goreleaser release --snapshot --clean       # four archives plus checksums
```

CI runs these jobs: `lint`, `test` (race plus coverage), `build` (every command,
`goreleaser check`, a snapshot, checksum verification), `e2e` (real Docker,
merged coverage at or above 70%, and an assertion that no containers remain),
`python-sdk` (Python 3.10 and 3.15, package builds and real-Docker E2E), and
`typescript` (strict consumer types, real-Docker fault-injection E2E,
browser-safe bundle, and an installed npm tarball).
See [versioning and release checks](docs/versioning.md) for release preparation.

The Python SDK requires Python 3.10+ and has no runtime dependencies. Its
behavioral gate uses real Docker and an HTTP fault proxy, with repeatable
evidence retained even on failure:

```sh
python3 -m pip install build
make python-build
make python-e2e
```

`python-e2e` creates a virtual environment and installs a wheel. To exercise an
already installed wheel, set `SDK_INSTALLED=1` and select its interpreter with
`PYTHON`. Write the failure model before implementation when testing an isolated
system. Prefer end-to-end coverage for new behavior; do not add unit tests after
writing the implementation. See the [Python SDK contract](docs/sdk-python.md)
and [failure model](sandbox-python/e2e/FAILURES.md).

The observability gate uses a private Unix-socket proxy around real Docker to
verify readiness failures, bounded probes, request IDs, structured log privacy,
and SSE flushing. It retains JSON reports, responses, logs, and cleanup evidence:

    bash scripts/e2e-observability.sh artifacts/observability-e2e

Its failure model is in scripts/observability-FAILURES.md. CI runs this gate in
the e2e job; faults affect only the private proxy and teardown uses a unique owner.

The Docker-backed Go tests are opt-in:

```sh
go test -tags docker ./internal/dockerbackend/ -run TestReal
```

## Commits

Subjects follow `type(scope): summary`; the scope is required.

- Types: `feat`, `fix`, `style`, `docs`, `refactor`, `test`, `chore`, `build`, `ci`
- Scope: lowercase kebab-case, for example `sdk`, `cli`, `daemon`, `docker`, `contracts`
- Summary: starts lowercase; the entire subject is at most 72 characters
- `!` before the colon marks a breaking change

```text
feat(sdk): resume streams from the last event id
fix(daemon): classify external stop as evicted, not exited
```

A tracked `commit-msg` hook rejects anything else once per clone:

```sh
make install-hooks   # point git at .githooks
make check-commit    # validate the latest commit
```

## Architecture and contracts

Read the [design](docs/design.md) before changing execution or lifecycle
behavior. The [HTTP API](docs/api.md), [Go SDK](docs/sdk.md),
[Python SDK](docs/sdk-python.md), and
[CLI](docs/cli.md) documents define the public contracts; update them with
any behavior changes.

For changes to execution, cancellation, streaming, or container lifecycle,
prefer end-to-end scenarios against real Docker. Keep repeatable evidence in
`artifacts/`, including commands, responses, logs, and cleanup results.
Only report checks that actually ran.

For TypeScript SDK changes, use Node 22+ and run:

~~~sh
make typescript
bash scripts/e2e-typescript.sh artifacts/typescript-e2e
~~~

The E2E runner starts an isolated authenticated daemon, injects faults through
a proxy around real responses, verifies the package, and retains repeatable
JSON/JSONL/log evidence plus container cleanup results. It does not add a
fake-only unit suite. Write failure scenarios before implementation and keep
the [SDK contract](docs/sdk-typescript.md) aligned with behavior.

File operation changes require Node 22+, Python 3.10+, and run the three-SDK and CLI real-Docker gate:

~~~sh
PYTHON=python3.15 bash scripts/e2e-files.sh artifacts/files-e2e
~~~

The files-e2e CI job runs this same command and uploads requests, SDK outputs,
server logs, the source diff, and owner-scoped container cleanup evidence.

With E2E_COVERAGE=1, scripts/e2e.sh also runs the real file scenarios with
instrumented daemon, CLI, Go client probe and memory HTTP probe binaries.
All counters feed the same coverage directory and unchanged 70% merged gate.
For a standalone instrumented file run, set E2E_COVERAGE=1 and optionally
FILES_COVER_DIR to an existing covdata directory. Node 22+ and Python 3.10+
are required for the coverage E2E command as well.
