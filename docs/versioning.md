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

The TypeScript SDK in sandbox-ts has an independent npm package version. This
repository currently builds and validates its tarball without publishing it.
Its [compatibility contract](sdk-typescript.md#compatibility-and-go-parity)
includes exported types, codes, outcome discriminants, retry defaults and
iterator cleanup. Wire additions must remain usable by existing consumers.

## Version numbers

Release tags use `vMAJOR.MINOR.PATCH`.

The OpenAPI `info.version` follows the release version. The `/v1` HTTP path
prefix is the API namespace, not a claim that the project has reached v1.0.0.

- **patch** fixes behaviour without changing the public contract;
- **minor** adds a backward-compatible operation, field, or flag;
- **major** may remove or reinterpret public API.

Before `v1.0.0`, breaking changes are allowed but must be called out explicitly in the change description.

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

Removing a field, changing a unit, or reinterpreting an existing value is a major change.

Clients must tolerate added response fields and preserve unknown string values.
The Python SDK retains extra fields in `extra` and represents missing additive
resource fields as `None`, rather than pretending a server applied a default.
Its execution conveniences surface unknown termination reasons as errors.

## Python distribution

The Python distribution is named `tiny-sandbox` and imported as `tiny_sandbox`.
Its package version is declared in `sandbox-python/pyproject.toml`. Keep it
aligned with the project release when preparing a release; the minimum
supported interpreter is Python 3.10. CI builds and verifies wheel/source
distributions. It does not publish to PyPI or
add Python assets to the binary release workflow. Install from the checkout
with `python3 -m pip install ./sandbox-python`.

## Release checklist

Requires Go 1.27+, Docker CLI/Unix socket, curl, Node 22+, Python 3.10+,
golangci-lint, and GoReleaser. Ensure `python3` is a supported interpreter on
`PATH`; `PYTHON` selects the interpreter for the Python and file SDK runners.
Use fresh artifact directories.

```sh
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
bash scripts/coverage.sh artifacts/cov-unit artifacts/e2e/coverage 70
python3 -m pip install build
make python-build
make python-e2e
```

The coverage steps follow CI: the primary E2E run includes file/archive
scenarios, and `coverage.sh` merges both profiles for the 70% profile-block
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
`info.version` and applicable package versions, then create a stable
`vMAJOR.MINOR.PATCH` tag at that commit and push it. Tags
containing the Release workflow trigger publication automatically:

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

Rolling back means checking out the previous tag and its binaries. It does not roll back sandbox state, and it does not undo commands that already ran. Server state is not persisted across restarts. Deployment orchestration,
canarying, and automated rollback are outside this repository's scope.

## Configuration

The [CI workflow](../.github/workflows/ci.yml), [Release workflow](../.github/workflows/release.yml),
[GoReleaser configuration](../.goreleaser.yaml),
[E2E script](../scripts/e2e.sh), [Python E2E script](../scripts/e2e-python.sh),
and [coverage merger](../scripts/coverage.sh)
are the executable sources for the checks described here.
