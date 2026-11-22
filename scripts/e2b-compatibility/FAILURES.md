# Acceptance runner failure model

This runner implements the plan in `docs/e2b-compatibility.md`, not the adapter.
The reference is the published e2b 2.64.0 source in both languages, recorded in
`reference.json`; an E2B key is not a prerequisite for the local gate. Native SDKs are never substituted
for a missing compatibility module. No mock can satisfy a parity case.

## Failures the harness must detect

- An unavailable SDK, key, Docker daemon or compatibility image is `not_run`,
  not pass. Wrong installed SDK versions and broken imports fail setup.
- One failed assertion, cleanup, or child process fails the run. A selection
  that excludes required cases is partial, even if all selected cases pass.
- A worker that hangs is bounded by a parent watchdog; owned IDs are persisted
  immediately after creation and cleanup runs in a fresh process. Lost create
  responses remain uncertain; use finite sandbox lifetimes, never auto-retry.
- Only run-specific sandboxes may be removed. The local controller uses its
  own Docker owner label and refuses arbitrary sandbox IDs. No global cleanup.
- A result missing required checks, an absent worker result, or a stale/reused
  evidence directory cannot become pass. Reports are written before dispatch
  and after each case, including setup failure. A final PASS sentinel is only
  produced by the full comparison gate, not a single provider.
- Provider differences in exit/error behavior, bytes, lifecycle and side effects
  fail. Only opaque IDs, times, chunks and unspecified list order are normalized.
- Report comparison requires the same fixture digest, SDK baseline, selected
  language set, all A01-A16 groups and W01-W03 workflows, and confirmed cleanup.
- Secrets are never passed as CLI arguments or included in configuration
  snapshots. Tiny workers have E2B credentials removed from their environment.
- Source inspection, syntax checks, and harness self-checks are reported as
  such; none claim that E2B cloud or a not-yet-implemented adapter passed.

## Controls

Parity assertions use asymmetric values (exit 37, different stdout/stderr,
base/override environment values, 1007 entries crossing a 1000-entry page).
Timeout probes write a startup marker before waiting; expiry probes observe
both sides of the deadline without using connect as a liveness read.

Local guards reuse the existing HTTP fault proxy in front of a private real
sandboxd. Docker removal faults are injected through a private Unix proxy.
A16 checks authentication, restart, and additive unknown fields against the
current server; it does not yet prove behavior with an older server or an
unmatched SDK/server pair. Every injected fault must be observed before its
case can pass.

## Delivery checks

The production adapters must also cover gaps a disposable probe can hide:

- A finite command deadline must still deliver incremental callbacks. Expiring
  during an accepted-but-lost start must retain its key and never send cancel.
  Ordinary callback exceptions must preserve identity, including SDK exceptions.
- Explicit false stdin/background and true retain-output are accepted; true
  stdin/background, false retain-output, invalid durations and unknown options
  fail before requests with side effects. Timeout zero disables only waiting.
- Repeated kill during failed removal must remain an error, not false. Python
  context cleanup errors must surface and retain any application exception.
- Runtime exit evidence and guest metadata must not be invented. Truncation,
  OOM, eviction and uncertain starts remain diagnostic errors, not exit zero.
- Installed wheels and npm tarballs must expose the real compatibility entry
  points without the E2B package, source paths or disposable probe modules.
- Native cancellation behavior, stream recovery, public types and browser
  packaging remain regression gates after sharing transport with the facade.

Check Python compilation, TypeScript against the pinned SDK declarations,
--list and missing-prerequisite exits, and comparison rejection of failed,
partial, mismatched and missing reports. Exercise guest fixtures in real
Docker when available. Live reference calls require --allow-cloud and an
E2B_API_KEY; installation and --list never create cloud resources.

## Concurrent local cases

A local matrix can finish cases out of order. Each case must retain a unique owner, output directory, worker process group and daemon endpoint. A restart or old-server case must stay reachable through its existing fault proxy. The parent records each completed row atomically in selection order. Case deadlines include subprocess startup and start only when a slot is available. Interrupting the runner must stop scheduling, reap active process groups and record their cleanup. Live cloud runs use one slot.
