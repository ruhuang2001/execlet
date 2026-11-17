# E2B compatibility acceptance

This runs the acceptance cases in [the plan](../../docs/e2b-compatibility.md).
It does not implement the adapter; the cases fail or report `not_run` until
`tiny_sandbox.e2b` and `@tiny-sandbox/sdk/e2b` exist. A passing run is evidence
for the cases it covers and nothing wider.

## Setup

`bash scripts/e2b-compatibility/setup.sh` installs the pinned references (`e2b==2.64.0` in both
languages), compiles `suite.ts`, and calls no cloud endpoint. It needs Python
3.11+ and Node 22.19+; the pinned dependencies declare `node >=22.19.0`, so an
older Node fails setup instead of failing later at runtime. Put a supported Node
on `PATH` for setup; the runner also accepts `NODE=`. Use `PYTHON=` for the
virtualenv base.

## Running

Run from the repository root. The required reference is the pinned SDK source
recorded in `reference.json`, not an E2B Cloud account. No E2B key is needed:

```sh
bash scripts/e2b-compatibility/run.sh tiny artifacts/e2b-tiny \
  --image sha256:<local-compatibility-image>
scripts/e2b-compatibility/.venv/bin/python scripts/e2b-compatibility/runner.py \
  compare artifacts/e2b-tiny/report.json
```

`--image` must name immutable content. There is no fallback to the native
Alpine/root default: the subset promises bash, a non-root `user`, `/home/user`
and a guest Python, and that is a property of the image, not of the server.

`--languages` and `--cases` narrow a run for diagnosis. A narrowed run is
recorded as such and can never satisfy `compare`, which requires all 19 cases
(parity targets and local guards) in both languages, the matching suite digest,
and confirmed cleanup.
`--list` prints the manifest without installing or calling anything.

`--module <import path>` points the tiny provider at another adapter module.
It exists for harness validation and experiments; the default is
`tiny_sandbox.e2b` (`@tiny-sandbox/sdk/e2b` in TypeScript).
For a combined run using custom modules, pass the Python name via `--module`
and the TypeScript module name or file URL via `CONTRACT_TS_MODULE`. Worker
identities in the report record what actually ran. A throwaway probe passing
does not mean the proposed published adapter exists or is compatible.

An optional live drift check is separate from `compare`, billable, and requires
explicit approval through `--allow-cloud` and an `E2B_API_KEY` environment variable:

```sh
bash scripts/e2b-compatibility/run.sh e2b artifacts/e2b-live --allow-cloud
```

No live drift check is required for the source-derived gate.

## Evidence

Each run creates a fresh directory, refusing an existing one:

| Path | Contents |
|---|---|
| `report.json` | Per-language, per-case status, checks, identity, cleanup, rewritten after every case |
| `<lang>-<case>/case.json` | That case's own verdict and every check with its evidence |
| `<lang>-<case>/worker-*.jsonl` | Requests and responses through the real SDK; large payloads are hashed |
| `<lang>-<case>/proxy.jsonl` | HTTP fault-proxy trace: paths, injected faults, SSE frames |
| `<lang>-<case>/sandboxd.log`, `docker.sock` | Private server log and the Docker proxy the server used |
| `<lang>-<case>/cleanup.json` | Independent observer result per owned ID, plus the owner sweep |

Statuses are `passed`, `failed`, and `not_run`. `not_run` means a prerequisite
was absent (no key, no `--allow-cloud`, no adapter module, no image); it is
never a pass. `compare` reads one tiny-provider report and exits non-zero unless
it is complete, matches the current suite digest, proves every required check
in both languages, and confirms cleanup. A check must be `true` or an object
with `passed: true`; a missing verdict is not success.

`reference.json` declares required checks as strings. An object with `any` lists
alternative outcomes, at least one of which must be present and passing. Its
optional `language` limits the expectation to that language. A06's millisecond
conversion is TypeScript-only; A13 accepts either complete event output or an
explicit event-cap error. All eight A12 option rejections remain independently
required. Counts include language-specific expectations, not unexecuted branches.

## Boundaries

Parity cases exercise source-derived E2B application semantics through the
selected module against a private real `sandboxd`. Local cases exercise tiny's
limits and fault recovery and never claim E2B server behavior. The optional
cloud mode runs parity cases with the official SDK. No mock stands in for a
missing SDK. Sandbox IDs are persisted before use, cleanup runs in a fresh process, and
the Docker guard only ever removes containers carrying a run-minted
`tiny-sandbox.owner` label.

`selfcheck.py` verifies the harness itself: manifest coverage, fixture
asymmetry, digest stability, `not_run` for missing prerequisites, and that the
comparison gate rejects failed, partial, mismatched, stale, and unproven
reports. It proves the gate works; it proves nothing about E2B or the adapter.
