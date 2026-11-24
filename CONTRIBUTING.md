# Contributing

## Prerequisites

- macOS or Linux, Go 1.27+, and Make. Windows and WSL2 are not supported.
- For runtime checks: Docker CLI, a reachable Docker Unix socket, and curl.
- For SDK and full coverage checks: Node 22+ and Python 3.10+. CI uses Python
  3.10 and 3.15; select a suitable local interpreter with `PYTHON`. Scripts
  that call `python3` directly also require that executable on `PATH`.
- `make lint` needs golangci-lint and Python 3.10+ with venv and pip. The first run installs the comment parsers. Install GoReleaser for release verification.

Build the daemon and CLI with `make build`. See the [quick start](docs/quickstart.md)
for Docker socket selection and running the service.

## Checks

Run commands from the repository root. Use fresh artifact directories. Before
implementing a behavior, name the wrong implementation it must reject and pick
the cheapest layer that can observe it: a Go test for state machines and wire
contracts, real Docker for runtime semantics. Add the failure to the suite's
row in the [failure models](docs/testing/README.md). Existing checks remain gates:

~~~sh
make race
make vet
make lint
make typescript
~~~

| Area | Command | Evidence |
|---|---|---|
| Lifecycle, execution, env, concurrency, CLI | `bash scripts/e2e.sh artifacts/e2e` | Responses, logs, replay/cancellation outcomes and cleanup |
| Files, archives, three SDKs and CLI | `PYTHON=python3.15 bash scripts/e2e-files.sh artifacts/files-e2e` | Metadata, archive validation, SDK outputs, source and cleanup |
| One-shot stdin | `bash scripts/e2e-stdin.sh artifacts/stdin-e2e` | Go/raw HTTP/CLI limits, exact bytes, privacy and cleanup |
| Request IDs and readiness | `bash scripts/e2e-observability.sh artifacts/observability-e2e` | Real Docker fault-proxy responses, logs and SSE |
| Python SDK | `PYTHON=python3.15 bash scripts/e2e-python.sh artifacts/python-e2e` | Installed wheel and real Docker fault scenarios |
| TypeScript SDK | `bash scripts/e2e-typescript.sh artifacts/typescript-e2e` | Types, browser bundle, installed tarball and Docker scenarios |
| E2B adapter subset | `PYTHON=python3.15 bash scripts/e2b-compatibility/installed.sh artifacts/e2b-installed "$SANDBOX_E2B_IMAGE" --profile background` | Installed wheel/tarball without E2B runtime, 40 cases per language, reference comparison and cleanup |
| Deployment operations | `python3 scripts/e2e-operations.py --server bin/sandboxd --image "$SANDBOX_E2B_IMAGE" --out artifacts/operations` | Atomic budgets, network controls, restart/storage/runtime faults, metrics and latency samples |

`PYTHON=python3.15` is an example; use an installed Python 3.10+ interpreter.
The Python runner builds and installs a wheel in an isolated virtual environment.
Set `SDK_INSTALLED=1` only when testing a wheel already installed in that
interpreter. To build distributions separately:

~~~sh
python3 -m pip install build
make python-build
~~~

The primary E2E server uses port 18081 by default; set `E2E_PORT` if needed.
Other runners choose their own ports and owner labels. Their cleanup targets
only their own containers. Do not use global Docker cleanup while other work runs.
The file runner prepares its pinned image before scenarios, retaining pull and
inspect diagnostics; setup retries do not retry scenario requests.
The E2B subset has its own [setup and acceptance requirements](scripts/e2b-compatibility/README.md),
including a compatible image digest, Python 3.11+ and Node 22.19+. Its source-derived
reference does not require an E2B Cloud account or key.

### MCP acceptance and measurements

- **Protocol and workspace regression:** `make build`, then `python3 scripts/e2e-mcp.py`. This checks stdio tools, sync/recovery and cleanup with the official MCP client.
- **Release archives and latency:** Set `ARCHIVE` to the full package for your OS and architecture. These checks exercise the binaries in the archive, not those in `bin/`.

  ```sh
  go build -o bin/mcp-client ./cmd/sandboxctl/testdata/mcp-client
  python3 scripts/e2e-mcp-workflow.py "$ARCHIVE" bin/mcp-client --out artifacts/mcp-python.json
  python3 scripts/e2e-mcp-workflow.py "$ARCHIVE" bin/mcp-client --go --out artifacts/mcp-go.json
  ```

  Each run creates a fresh sandbox and measures initial, unchanged and single-file syncs. `timing_us` separates host selection/hash/staging (`scan`), guest updates and manifest commit (`apply`), archive HTTP writes within `apply` (`archive_upload`), and time inside the sync handler (`total`). Image build time is excluded. The first Go run starts with an empty build cache. CI runs one sample per workload and does not gate on latency.
- **Real Agent (optional):** With an active local Amp login, run `make build`, `docker build -t tiny-sandbox-mcp:local examples/mcp`, and `python3 scripts/e2e-amp.py --out artifacts/amp`. This uses a private Amp run and records the repair conversation, network mode and cleanup evidence.

### Code comments

Run `make comment-lint` to check changes against the merge base with `origin/main`. Use `COMMENT_BASE=<revision>` to select another base. The checker uses Python 3.10+ and installs its parsers into a local virtual environment. The same rule runs in `make lint` and CI. Existing comments can be removed incrementally. New or rewritten prose comments fail the check. Keep design rationale in an ADR and open issues for implementation compromises. The [delete-comment skill](.agents/skills/delete-comment/SKILL.md) describes the audit.

### Coverage and CI

~~~sh
PYTHON=python3.15 E2E_COVERAGE=1 bash scripts/e2e.sh artifacts/e2e
mkdir -p artifacts/cov-unit artifacts/operations-coverage artifacts/combined-e2e-coverage
go test -race -covermode=atomic -coverpkg=./... ./... -args -test.gocoverdir="$PWD/artifacts/cov-unit"
go build -cover -covermode=atomic -coverpkg=./... -o bin/sandboxd-coverage ./cmd/sandboxd
GOCOVERDIR="$PWD/artifacts/operations-coverage" python3 scripts/e2e-operations.py \
  --server bin/sandboxd-coverage --image "$SANDBOX_E2B_IMAGE" --out artifacts/operations
go tool covdata merge -i=artifacts/e2e/coverage,artifacts/operations-coverage -o=artifacts/combined-e2e-coverage
bash scripts/coverage.sh artifacts/cov-unit artifacts/combined-e2e-coverage 70
~~~

With coverage enabled, the primary E2E script also runs instrumented file,
archive and memory-HTTP probes into the same covdata directory. Operations
acceptance adds the policy/recovery paths through an instrumented daemon. Set
`SANDBOX_E2B_IMAGE` to the local image digest prepared by the E2B harness. The current
70% gate counts covered profile blocks; it is not the statement-weighted
percentage shown by `go tool cover -func`. Do not compare those metrics as if
they were interchangeable.

The `coverage` job merges profiles from `test`, `e2e`, the operations suite in
`e2b-compatibility`, `feature-e2e` and `runtime-e2e`. The
[CI workflow](.github/workflows/ci.yml) defines the complete job matrix.
The build job checks release snapshots, including offline MCP acceptance.
[Versioning](docs/versioning.md) covers tagged releases.

`scripts/e2e.sh` also runs the real Docker backend tests
(`go test -tags docker ./internal/dockerbackend -run '^TestReal'`).
`TestRealNonRootPrivateStdin` additionally needs `SANDBOX_E2B_IMAGE`.

## Contracts and review

Use the [design](docs/design.md) for architecture and the [HTTP API](docs/api.md),
[OpenAPI](api/openapi.yaml), [CLI](docs/cli.md), [Go](docs/sdk.md),
[Python](docs/sdk-python.md), and [TypeScript](docs/sdk-typescript.md) references
for public behavior. Update all affected surfaces together. Keep shared limits
and lifecycle semantics in the HTTP contract; SDK guides should explain their
own signatures, return values and recovery differences.

Use `internal/wirejson` for the existing HTTP, SSE and Docker JSON contracts:
it runs `encoding/json/v2` with v1 compatibility options so migration does not
change nulls, omitted values, escaping or field matching. Use the v2 defaults
for strict internal formats such as the policy and journal, not the wire options.

Document implemented behavior, including failure and compatibility limits.
Keep unimplemented ideas in [future directions](docs/future.md). Validate examples
and links, and report only checks that actually ran. Before merging, test against
the current base branch: separately passing changes can still conflict through
shared handler signatures, schema inventories or SDK request models.

## Commits

Subjects use `type(scope): summary`, at most 72 characters. Scope is required,
lowercase kebab-case; the summary starts lowercase. Supported types are `feat`,
`fix`, `style`, `docs`, `refactor`, `test`, `chore`, `build`, and `ci`. Use `!`
before the colon for breaking changes.

~~~text
feat(sdk): resume streams from the last event id
docs(api): clarify concurrent execution outcomes
~~~

Run `make install-hooks` once to enable the tracked commit-message hook.
`make check-commit` validates the latest commit.
