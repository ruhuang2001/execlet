# Versioning and release

The Go module, `sandboxctl`, and `sandboxd` share one version number. Ordinary
CI builds GoReleaser snapshots as workflow artifacts. The separate Release
workflow validates a release tag through the same CI before building and
publishing versioned binaries.

The documentation and OpenAPI on `main` describe that checkout, including
unreleased additions. The installer and Go's `@latest` version selection use
published releases; they do not promise the current `main` feature set. Read
the documentation from the selected tag, or use matching source revisions for
the daemon, CLI and SDK. `info.version` alone is not feature negotiation.

Supported release targets are Linux and macOS (`linux` and `darwin`), each on
amd64 and arm64. Each of the four `.tar.gz` archives contains `sandboxd` and
`sandboxctl`. Windows is not supported; Windows binaries and ZIP archives are
not published. WSL2 is not a tested or supported environment.

The TypeScript SDK in sandbox-ts has an npm package version aligned with the
selected project release. CI builds and validates its tarball; publication is
a separate, manually approved workflow, not a side effect of tests.
Its [compatibility contract](sdk-typescript.md#compatibility-and-go-parity)
includes exported types, codes, outcome discriminants, retry defaults and
iterator cleanup. Wire additions must remain usable by existing consumers.

## Unreleased breaking change

`PUT /v1/sandboxes/{sandbox_id}/files` now returns HTTP 200 with a JSON
`FileEntry`. Earlier releases returned HTTP 204 with an empty body. Update
callers that check for 204. Go `WriteFileInfo` returns nil metadata for legacy 204 responses, Python returns `None`, and TypeScript returns `undefined`. Go `WriteFile` accepts either successful response. E2B adapters return the destination name and path on legacy daemons and include the observed type when the response supplies metadata.

## Version numbers

Release tags use `vMAJOR.MINOR.PATCH`.

The OpenAPI `info.version` follows the release version. The `/v1` HTTP path
prefix is the API namespace, not a claim that the project has reached v1.0.0.

- **patch** fixes behaviour without changing the public contract;
- **minor** adds a backward-compatible operation, field, or flag;
- **major** may remove or reinterpret public API.

Before `v1.0.0`, breaking changes are allowed but must be called out explicitly in the change description.
For this initial-development release, `0.0.2` deliberately groups the additive
SDK, execution, file and E2B-subset work rather than advancing to `0.1.0`. This
is an explicit exception to the patch-only rule, not a claim that the release
contains only fixes. OpenAPI and both Python/npm package versions are `0.0.2`;
updating these declarations does not publish a release or a package.

Current source adds background handles and corrects `commands.run` timeouts
from local waiting to a remote command deadline. This must ship under a new
version, with the behavior change documented, not replace published 0.0.2.
Package declarations remain unchanged until that release is selected.

## What counts as the contract

The compatibility surface is wider than the Go API:

- CLI flags, environment variables, and exit codes;
- JSON request and response fields;
- SDK method signatures, request/response fields, and error types;
- Go sentinel values matched by `errors.Is` and Python `Error.code` values;
- timeout units and their meaning;
- SSE event types, event ID semantics, and `Last-Event-ID` behaviour.

[api.md](api.md) is the prose HTTP contract and [api/openapi.yaml](../api/openapi.yaml) is its machine-readable form, embedded by `api/contract.go`. Existing checks compare method/path sets across the prose, specification, and router; they do not prove that every schema or behavioral detail matches. Review those details when changing the contract.

## Additive changes

Prefer a new optional field or a new operation over changing an existing one. The server ignores unknown request fields, so a newer client can talk to an older server; the old server simply does not act on the new field. The client must therefore not assume a field took effect, and must document which contract version it needs. Old clients keep working against a new server because responses only gain fields.

Current SDKs check authenticated `/v1/capabilities` before nonempty env/cwd/stdin,
explicit concurrency, managed deadlines, directory listings and archive transfers.
Missing support returns `incompatible_server` before the operation; basic
legacy calls, including ordinary file reads and writes, skip discovery.
The E2B facade requires `e2b.background.v1` even for foreground run because its
deadline semantics changed. Go and TypeScript cache flags per client;
Python reads afresh. Mixed-version load balancers and downgrade within
a session are unsupported. Capabilities do not certify guest image tooling.

Removing a field, changing a unit, or reinterpreting an existing value is a major change.

Clients must tolerate added response fields and preserve unknown string values.
The Python SDK retains extra fields in `extra` and represents missing additive
resource fields as `None`, rather than pretending a server applied a default.
Its execution conveniences surface unknown termination reasons as errors.

## Python distribution

The Python distribution is named `tiny-sandbox` and imported as `tiny_sandbox`.
Its package version is declared in `sandbox-python/pyproject.toml`. Keep it
aligned with the project release when preparing a release; the minimum
supported interpreter is Python 3.10. CI builds wheel/source distributions and
tests installed wheels. Ordinary CI does not publish them or add them to the
binary Release. Until an authorized package publication, install from this
checkout with `python3 -m pip install ./sandbox-python`, or from its tested
CI wheel. The npm tarball is likewise installable directly with `npm install
./tiny-sandbox-sdk-VERSION.tgz`; do not assume the package name exists in a registry.

## Release checklist

Requires Go 1.27+, Docker CLI/Unix socket, curl, Node 22.19+, Python 3.11+,
golangci-lint, and GoReleaser. Ensure `python3` is a supported interpreter on
`PATH`; `PYTHON` selects the interpreter for the Python and file SDK runners.
The higher interpreter requirement is for the E2B reference harness; the native
Python SDK still supports 3.10. Use fresh artifact directories.

```sh
make agent
go build ./...
go vet ./...
go test -race ./...
golangci-lint run ./...
make typescript
bash scripts/e2e-typescript.sh artifacts/typescript-e2e
goreleaser check
goreleaser release --snapshot --clean
E2E_COVERAGE=1 bash scripts/e2e.sh artifacts/e2e
bash scripts/e2e-stdin.sh artifacts/stdin-e2e
bash scripts/e2e-observability.sh artifacts/observability-e2e
mkdir -p artifacts/cov-unit
go test -race -covermode=atomic -coverpkg=./... ./... \
  -args -test.gocoverdir="$PWD/artifacts/cov-unit"
python3 -m pip install build
make python-build
make python-e2e
bash scripts/e2b-compatibility/setup.sh
python3 scripts/e2b-compatibility/selfcheck.py
docker build -t tiny-sandbox-e2b:local examples/e2b
IMAGE=$(docker image inspect tiny-sandbox-e2b:local --format '{{.Id}}')
bash scripts/e2b-compatibility/installed.sh artifacts/e2b-installed "$IMAGE" --profile background
mkdir -p artifacts/operations-coverage artifacts/combined-e2e-coverage
go build -cover -covermode=atomic -coverpkg=./... -o bin/sandboxd-coverage ./cmd/sandboxd
GOCOVERDIR="$PWD/artifacts/operations-coverage" python3 scripts/e2e-operations.py \
  --server bin/sandboxd-coverage --image "$IMAGE" --out artifacts/operations
go tool covdata merge -i=artifacts/e2e/coverage,artifacts/operations-coverage -o=artifacts/combined-e2e-coverage
bash scripts/coverage.sh artifacts/cov-unit artifacts/combined-e2e-coverage 70
```

The `e2b-compatibility` CI job builds the image, selects its immutable digest,
and tests the installed wheel and npm tarball: 40 cases in each language plus
the reference comparison, operations fault suite and independent cleanup.
Historical revisions through v0.0.2 retain their foreground-only profile.
It uploads reports, logs,
image metadata and package files even on failure, without uploading the test
virtualenv or consumer dependencies. No E2B Cloud credentials or publication
are involved. The Release workflow reuses this job through `validate`, so a
failed or incomplete comparison blocks publication. Only an explicitly selected
historical `v0.0.1` commit may lack this suite; missing it on current or new
release revisions is an error.

The coverage steps follow CI: the primary E2E run includes file/archive
scenarios, operations acceptance contributes instrumented daemon coverage,
and `coverage.sh` merges with unit coverage for the 70% profile-block
coverage gate. This differs from statement-weighted coverage; see
[contributing](../CONTRIBUTING.md#coverage-and-ci). Missing container images
require registry access. The script starts its own server on port
18081 by default; set `E2E_PORT` if that port is occupied.

Inspect the four platform archives in `dist/` and verify `checksums.txt`
(`sha256sum -c checksums.txt`, or `shasum -a 256 -c checksums.txt` on macOS,
from that directory). `--snapshot` creates local artifacts without publishing.
Publishing a release is a separate action from this verification checklist.

## Publish through GitHub Actions

Prepare and merge the intended release commit on `main`, including OpenAPI's
`info.version` and applicable package versions, then create the intended
`vMAJOR.MINOR.PATCH` tag at that commit and push it. The Release workflow
publishes a normal GitHub Release by default; prerelease status must be set
explicitly and is not inferred from `0.x` numbering. Tags containing the
Release workflow trigger publication automatically:

```sh
git tag -a v0.0.2 -m 'v0.0.2'
git push origin v0.0.2
```

For an existing tag, including one created before the workflow was added,
dispatch from `main` without moving the tag:

```sh
gh workflow run release.yml --ref main -f tag=v0.0.1
```

The workflow resolves the tag to an immutable commit on `main`, runs every CI
job against that commit, and only then grants the publishing job
`contents: write`. It builds four non-snapshot archives, verifies their
checksums and native version output, uploads to a draft, downloads the
attachments again, and checks them against the CI build before publishing.
The release notes include a link to the responsible Actions run.

Existing release notes are retained. A published Release is never overwritten.
If a failed or earlier manual attempt left same-name assets in a draft,
inspect and back them up before explicitly allowing replacement:

```sh
gh workflow run release.yml --ref main \
  -f tag=v0.0.1 -f replace_draft_assets=true
```

That option deletes and replaces same-name draft attachments; it does not
delete the draft or move the tag. It defaults to false. The CI-built archives
and checksums are also retained as an Actions artifact for seven days.

After publication, download the four archives and `checksums.txt` from the
Release, verify all hashes, and check the native binaries' version output
(`sandboxctl version` and `sandboxd -version`). Do not upload local binaries
as a substitute for this workflow.

## Rollback

Rolling back means checking out the previous tag and its binaries. It does not
undo commands that already ran or roll back durable state. With `-state-dir`,
stop the daemon and retain a consistent backup before upgrading; old binaries
may not understand the journal. Never clear state just to bypass validation.
Without durable mode, restart loses history and reclaims owned containers. Deployment orchestration,
canarying, and automated rollback are outside this repository's scope.

## Publish SDK packages separately

`.github/workflows/sdk-publish.yml` is manual and defaults to `publish=false`.
Dispatch it **at the selected tag**, using that same value for its `tag` input.
It verifies the immutable commit is on `main`, versions match across OpenAPI,
Python, npm and its lockfile, and the version is newer than 0.0.2. All reusable
CI jobs must pass before publication can start. Ordinary tag pushes do not
publish these SDK packages; they still trigger the separate binary Release.

An owner must first configure the public package names and trusted publishers:
PyPI `tiny-sandbox` and npm `@tiny-sandbox/sdk`, repository
`ruhuang2001/tiny-sandbox`, workflow `sdk-publish.yml`, and environments `pypi`
and `npm` respectively. Protect those GitHub environments with approvals and
release-tag restrictions. Confirm scope/name ownership and any first-package
bootstrap before enabling publication; this workflow does not create accounts
or permissions. No registry configuration was performed by this change.
For npm, permit direct `npm publish` in the trusted publisher configuration;
a stage-only publisher cannot run this workflow. A newly configured npm
publisher must complete its first publication within two days or be recreated.

The publishing jobs use OIDC, never stored publishing tokens. npm uses Node 24
and checks for npm >=11.5.1; its repository metadata must match the public
repository for provenance. PyPI uses `pypa/gh-action-pypi-publish` with its
custom `packages-dir`. Both download `sdk-distributions` from the same CI run
and publish the exact wheel/tarball that passed installed acceptance, without
rebuilding in a privileged job. PyPI publishes the tested wheel only.

After explicit release approval, set `publish=true`. If just one registry
succeeds, inspect it first, then select only the unfinished `registry` (`pypi`
or `npm`). Duplicate versions fail rather than being overwritten or silently
skipped. After publication verify package hashes, install into clean consumers
using the registry names and matching version, and repeat the acceptance gate.
Until that happens, local package tests do not prove public registry delivery.

References: `https://docs.npmjs.com/trusted-publishers/`,
`https://github.com/pypa/gh-action-pypi-publish`, and
`https://github.com/actions/upload-artifact` (reviewed 2026-11-18).

## Configuration

The [CI workflow](../.github/workflows/ci.yml), [Release workflow](../.github/workflows/release.yml),
[GoReleaser configuration](../.goreleaser.yaml),
[E2E script](../scripts/e2e.sh), [Python E2E script](../scripts/e2e-python.sh),
and [coverage merger](../scripts/coverage.sh)
are the executable sources for the checks described here.
