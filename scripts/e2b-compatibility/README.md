# E2B compatibility acceptance

This runs the acceptance cases in [the plan](../../docs/e2b-compatibility.md).
The production adapters are `tiny_sandbox.e2b` and `@tiny-sandbox/sdk/e2b` in
the SDK packages. A passing run is evidence for the cases it covers and nothing
wider; a missing module still reports `not_run`, never a pass.

## Setup

`bash scripts/e2b-compatibility/setup.sh` installs the pinned references (`e2b==2.64.0` in both
languages), compiles `suite.ts`, and calls no cloud endpoint. It needs Python
3.11+ and Node 22.19+; the pinned dependencies declare `node >=22.19.0`, so an
older Node fails setup instead of failing later at runtime. Put a supported Node
on `PATH` for setup; the runner also accepts `NODE=`. Use `PYTHON=` for the
virtualenv base.

## Running

Prefer the installed-package gate from the repository root:

```sh
bash scripts/e2b-compatibility/setup.sh
npm --prefix sandbox-ts ci
docker build -t tiny-sandbox-e2b:local examples/e2b
IMAGE=$(docker image inspect tiny-sandbox-e2b:local --format '{{.Id}}')
PYTHON=python3.15 bash scripts/e2b-compatibility/installed.sh artifacts/e2b-installed "$IMAGE" --profile background
```

Use an installed Python 3.11+ interpreter. Keep the output path short on macOS
because each case creates a Unix socket. This script builds and installs the
wheel/tarball in isolated consumers, checks their real entry points and strict
TypeScript declarations, records image/package versions, runs all 80 rows, and
calls compare. It verifies neither consumer has the E2B runtime installed.
The reference SDK is used only for source/declaration checks.

The output includes `wheels/`, `npm/`, installation logs, resolved import paths,
`installed-types.log`, `image.json`, `guest.txt`, `run/report.json` and
`compare.log`. No registry publication or E2B Cloud call occurs.

CI runs this installed-package gate as `e2b-compatibility` on pushes and pull
requests, and the reusable CI also gates Release publication. It builds the
local image from `examples/e2b`, pins the resulting digest, runs `selfcheck.py`,
and requires the full 80-row background comparison plus a zero-container check.
It also runs the deployment policy/recovery/fault suite and retains the tested
distributions for the separate, opt-in SDK publication workflow. Failed
runs still upload evidence and package files, excluding the virtualenv and
consumer dependency tree. The suite may be absent only for an explicitly
selected historical `v0.0.1` commit, not for current or new release revisions.

Run from the repository root. The required reference is the pinned SDK source
recorded in `reference.json`, not an E2B Cloud account. If testing already
installed packages directly, no E2B key is needed:

```sh
bash scripts/e2b-compatibility/run.sh tiny artifacts/e2b-tiny \
  --image sha256:<local-compatibility-image> --profile background
scripts/e2b-compatibility/.venv/bin/python scripts/e2b-compatibility/runner.py \
  compare artifacts/e2b-tiny/report.json --profile background
```

`--image` must name immutable content. There is no fallback to the native
Alpine/root default: the subset promises bash, a non-root `user`, `/home/user`
and a guest Python, and that is a property of the image, not of the server.

`--languages` and `--cases` narrow a run for diagnosis. A narrowed run is
recorded as such and can never satisfy `compare`, which requires all 40 background
cases (19 for historical baseline) in both languages, the matching suite digest,
and confirmed cleanup.
`--list` prints the manifest without installing or calling anything.

`--module <import path>` points the tiny provider at another adapter module.
It exists for harness validation and experiments; the default is
`tiny_sandbox.e2b` (`@tiny-sandbox/sdk/e2b` in TypeScript).
For a combined run using custom modules, pass the Python name via `--module`
and the TypeScript module name or file URL via `CONTRACT_TS_MODULE`.
`CONTRACT_TS_WORKER` selects the compiled worker location for isolated package
resolution; `installed.sh` copies the same compiled application into its npm
consumer and clears module overrides. Worker identities record what actually
ran. A throwaway probe passing does not establish installed-package support.

An optional live drift check is separate from `compare`, billable, and requires
explicit approval through `--allow-cloud` and an `E2B_API_KEY` environment variable:

```sh
bash scripts/e2b-compatibility/run.sh e2b artifacts/e2b-live --allow-cloud
```

No live drift check is required for the source-derived gate.

## Background-command profile

The [background contract](../../docs/e2b-background-commands.md) has a separate
profile used by current CI. It invokes the real installed adapters, not a
probe implementation. It remains intentionally failing against v0.0.2, which
has no background handles or capability gate.
No E2B key or E2B runtime dependency is needed, and cloud mode is refused for
this profile even with `--allow-cloud`.

```sh
IMAGE=$(docker image inspect tiny-sandbox-e2b:local --format '{{.Id}}')
PYTHON=python3.15 bash scripts/e2b-compatibility/installed.sh artifacts/e2b-bg "$IMAGE" \
  --profile background
```

This runs **40 cases × 2 languages**: the original 19, with A05/A12/A14/A16/W03
expectations overlaid, plus B01–B20 and W04. The default remains the original
19-case profile. A failed installed-type check is recorded in
`background-types.log`/`.json` and does not prevent runtime cases from running.
The script always attempts `compare`, including when runtime assertions fail.
For separate comparison, pass the same profile explicitly:

```sh
python3 scripts/e2b-compatibility/runner.py compare artifacts/e2b-bg/run/report.json \
  --profile background
python3 scripts/e2b-compatibility/runner.py --list --profile background
```

`background-reference.json` pins the runtime reference and the actual v0.0.2
server revision built by A16/B18. It requires every applicable assertion and
triggered-fault evidence, not just a case marked passed. Missing assertions,
wrong profile/revision, stale digest, partial selections and unconfirmed cleanup
all fail comparison. `selfcheck.py` mutation-tests those rejection paths.

The fixtures exercise real guest PID/start ticks, file gates, asymmetric Unicode
streams, binary artifacts, fresh Python/Node controllers, SSE interruption,
accepted-response loss, exact output/event limits, completed-record retention,
Docker failure injection and independent owner-scoped cleanup. B08 additionally
runs an opt-in backend test with a modeled Engine signal-dispatch race:

```sh
go test -tags=e2b_background_acceptance ./internal/dockerbackend \
  -run '^TestBackgroundSignalLifetime$' -count=1 -v
```

That test models the Engine signal-dispatch boundary: a numeric-only signal
can select a replacement incarnation; the current lifetime-bound protocol
must reject it. It does not force real-kernel PID reuse or prove isolation from
hostile same-UID code. The B08 scenario runs it as part of current CI; default
Go tests alone do not include it.

An absent API stops that case at its first failure. Later scenario code is
**not exercised**, even though it is runnable once prerequisites exist. Do not
interpret a full dispatch or passing harness selfcheck as 80 behavioral passes.
Keep actual run results and cleanup evidence separate from source-derived
expectations. Production methods now exist, but package publication and a new
release remain separate actions. Do not run the historical baseline against
new code and interpret its old local-timeout expectations as regressions.

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
explicit event-cap error. Python cleanup cause chaining is language-specific.
All A12 option rejections remain independently required. The gate requires
294 expectations (147 per language), not unexecuted alternative branches.
Truncation requires the diagnostic code and an affirmative truncation flag;
finding the word in a field name is not evidence. Independent output-limit
probes clean up before starting another sandbox, since truncation does not kill
the remote execution.

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
