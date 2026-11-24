# Background command failure model and acceptance

The gate must prove that a command outlives its client but not its remote
deadline, can be reattached without re-execution, and cannot kill a different
process after PID reuse. See the [background command contract](../e2b-background-commands.md).

**Status: runnable against current installed adapters; intentionally red against v0.0.2.**
Use `installed.sh ... --profile background`; see the
[runner instructions](../../scripts/e2b-compatibility/README.md#background-command-profile).
The overlay adds B01–B20/W04 and replaces only the specified A/W expectations,
giving 40 cases × 2 languages. It does not add passing rows to the v0.0.2
manifest. The baseline remains SDK 2.64.0 plus the separately pinned runtime
revision in the contract; no E2B key is needed. Missing APIs, type failures and
unexercised assertions cannot count as successful acceptance.

## Failure model

| Failure | Wrong implementation this must expose | Independent evidence |
|---|---|---|
| Client dies after accepted start | Remote work dies with its HTTP connection, or a retry starts it twice | Guest counter and gate-controlled artifact, real process identity, proxy trace |
| Wait and run deadlines confused | Reconnect kills the command; disconnect disables its remote timeout; reconnect extends it | Recorded deadlines, exact command marker, death evidence and a timeout-zero control |
| Fast exit or PID reuse | Fake/missing PID, stale handle signals a replacement, completed attach hangs | Guest PID file, terminal execution, separate replacement marker |
| SSE loss or concurrent completion | Duplicated callbacks, dropped suffix, wrong full-output prefix | Numbered per-stream data, attach watermark, injected network cut, independent expected bytes |
| Failed signal/removal | HTTP 200 or a local killed flag is treated as confirmed termination | Runtime inspection and continuing/stopped heartbeat, not the response alone |
| Limits, restart or unsupported server | Empty success after lost history; silently ignored timeout field; unbounded cache | Raw HTTP, release-accurate old server, retention boundary, owner cleanup |

## Fixture rules

- Run each B case through both installed adapters. Split language-specific
  subassertions explicitly; neither language can satisfy the other's row.
  Keep native cancellation and other existing regression gates.
- Use a private daemon, run-specific owner label and immutable compatibility
  image. The guest runs as `user`. Never change host PID sysctls, kill unrelated
  processes or globally prune Docker to manufacture a test.
- A gate-controlled Python child records its own PID, a start counter and
  `READY`, emits different numbered stdout/stderr including Chinese and emoji,
  then waits for a file gate before writing `AFTER` and a binary artifact.
  Use file API operations for gates while the single execution slot is occupied.
  Record OS PIDs for both controller processes to prove a fresh-client test.
- Wait for READY before timeout/kill controls. Do not rely on a fixed sleep
  to prove admission. A missing AFTER alone never proves termination: the
  child may simply be delayed. Check its runtime liveness and use a healthy
  timeout-zero sibling fixture where the same gate actually produces AFTER.
- Record monotonic times and the configured deadline/enforcement tolerance.
  Separate startup timing from execution timing. The provisional real-Docker
  enforcement budget is 3 seconds after a command deadline on a healthy
  runtime; backend failure must produce uncertainty, not a timing pass.
  Verify 1250 ms exactly on the wire; use well-separated runtime controls
  rather than a fragile 1-second-versus-2-second wall-clock assertion.
- Inject faults using the existing local HTTP/Docker proxy. Scope them to
  the case's IDs; save which faults actually fired. A requested fault that
  never triggered makes the subassertion fail.

B08 must exercise both the server mapping and the runtime signal race: delay
a signal after identity resolution, let that process exit, then present a
replacement incarnation. Merely retaining the old execution ID does not prove
the backend cannot signal a reused numeric PID/group. B06 also calls wait and
kill after disconnect: wait reports `disconnected` unless a result was already
observed, while kill can still target the original execution. B11 must retain
the first accepted attach boundary across SSE retries, rather than performing
a second PID lookup that could skip output or select a replacement.

The opt-in `e2b_background_acceptance` Go test drives the real Docker backend
against the existing fake Engine to model PID reuse at signal dispatch. Its
active-process control must work and its replacement must remain untouched.
It is not forced Linux PID reuse. The fixture models the current runtime helper
protocol and fails closed on unknown helpers; the lifetime-safe implementation
must extend that boundary model alongside its own tests. Real Docker checks
remain required for stale handles, sandbox scoping and group termination.

## Cases

`Reference` means an assertion derived from the pinned source. `Local` means
an explicit tiny-sandbox extension or safety limit. `Mixed` rows must record
their reference and local subassertions separately; a local pass is not a
live E2B comparison.

| ID | Kind | Discriminating scenario | Required observations |
|---|---|---|---|
| B01 | Reference | Start gate-controlled `exec python3 ...` with background true and timeout 0; separately run `true` and `exit 37` | Handle arrives before gate opens, pid equals the guest PID, and counter is 1. Fast success/failure both return handles; wait yields result/command-exit family with real status. Foreground return type unchanged. |
| B02 | Mixed | Persist sandbox ID + PID; disconnect; exit controller A; controller B connects while the child is blocked | Different host processes resolve the same managed execution without client cache. No additional execution start or TTL patch from commands.connect. Gate release creates one artifact; counter stays 1. Sandbox.connect's separate TTL extension is recorded. |
| B03 | Reference | Start timeout 1.25 s / 1250 ms, wait for READY, detach; keep AFTER gated until well past the deadline; use timeout 0 control | Wire duration is 1250 ms. First command terminates without AFTER despite absent observers; zero-timeout control lives and writes AFTER when released. Foreground and background both enforce remote timeout. No fabricated exit code. |
| B04 | Reference | Start with generous remote deadline; connect with 1250 ms observation timeout; then reconnect with 0 | First wait times out locally; exact child still lives and later writes AFTER. No cancel/kill/TTL mutation from connect or wait expiry. Second connection observes completion without a second start. |
| B05 | Mixed | Consume part of run/connect observation budget before calling wait; reattach near the original remote deadline with a longer local budget | Wait does not reset the original connection clock; new connect gets only its own observation clock. Remote execution deadline is unchanged and expires without extension. Omitted run timeout normalizes to 60000 ms; zero disables only the specified clocks. |
| B06 | Reference | Two distinct consumer handles; disconnect one twice while command is silent; TS callback calls disconnect and another variant leaves a callback pending | Disconnect returns without waiting for new output or for its own callback to settle; no subsequent callback dispatch on that handle. Other handle continues. Python covers idle/suspended disconnect, not unsupported cross-thread interruption. Remote child remains live. |
| B07 | Mixed | Install SIGTERM handler that writes TERM; record READY; call handle.kill; repeat; repeat with commands.kill(pid) | TERM is absent, group stops and successful kill is true only after confirmation. Already-finished/repeated kill is false. Sandbox remains usable on the normal path. Native cancel still takes the TERM path. Group stop and confirmation are local assertions. |
| B08 | Local | Hold old handle, retire command, arrange managed PID reuse; also use nonexistent PID and another sandbox | Old handle never acts on replacement; raw PID lookup selects only requested sandbox's current managed incarnation. Missing command uses not-found/false as appropriate. Test PID-reuse bookkeeping deterministically at the backend boundary, plus real Docker stale-handle/sandbox separation; do not claim forced real PID reuse if not achieved. |
| B09 | Mixed | Connect after `exit 37`; separately let completion race between attach snapshot and SSE subscription; repeat terminal waits | Retained terminal outcome returned promptly, never waits after the final event forever. New completed attach has empty observation output, real exit 37 and command-exit exception. Repeated waits cause no network/callback replay. |
| B10 | Mixed | Emit PRE and confirm it is recorded; connect; release gate to emit POST with asymmetric stdout/stderr | New handle returns POST only; native get-execution still contains PRE+POST within retention. Attach watermark and terminal snapshot cannot add PRE back. Counts are per stream, not chunk count or global stdout/stderr order. |
| B11 | Local | Drop same-handle SSE after a delivered numbered chunk, then allow replay and completion | Each delivered chunk appears once and in per-stream order, with complete suffix and matching result. Retry uses Last-Event-ID and remaining local budget. No remote start or cancel. Run once for a start handle and once for an attached handle. |
| B12 | Local | Drop/hold an accepted start response; retry same invocation within retained window; separately exhaust recovery | Counter is 1, PID/execution mapping stays stable, and remote deadline is not reset. Exhaustion retains key and available ID; no fake handle or new-key replay. Server-side command timeout still works without a returned handle. Expired retention is never treated as proof a retry is safe. |
| B13 | Mixed | Raise a unique callback exception/rejection before completion; also fail a TS callback before anyone calls wait | Original exception/value surfaces; no retry, cancel, or unhandled rejection. Child continues until its own deadline/gate. Fresh connect works; failed old handle does not silently restart observation. TS callbacks remain serial. |
| B14 | Local | Output exactly 1 MiB versus one byte over; separately exceed 4096 events; truncate before reattach; prune a retained completed execution | Boundary-sized complete result succeeds; overflow is output_truncated with partial evidence, never full success. Reattach cannot clear truncation. Pruned command is not-found. Active records survive pruning; handles do not pin completed records indefinitely. |
| B15 | Local | One-slot sandbox has one detached running command; attempt second start; attach/kill first; multi-slot native fixture has a sibling | Busy request starts nothing; attach and kill work despite the occupied slot. Slot is released only after real completion. Normal kill leaves sibling live; container-removal fallback reports collateral termination explicitly. |
| B16 | Mixed | Sandbox lifetime shorter than command lifetime; native pause control; daemon restart under stable owner | Handle operations do not renew TTL; sandbox expiry terminates child. Adapter commands.connect rejects non-running sandbox instead of auto-resuming. Restart invalidates old identities and performs owner cleanup, never silently creates work. Cleanup failure stays visible. |
| B17 | Local | Deny signal, delay terminal evidence, lose kill acknowledgement, and fail container-removal fallback | Never return true from HTTP acceptance or state flag alone. Expose uncertainty and ID; retry keeps immutable identity. Independent observer records whether child/group/siblings remain, then removes only owned containers. |
| B18 | Local | Use actual v0.0.2 server without capability; also missing flag on new server and wrong credentials | Corrected run/background/connect/kill reject before command mutation. No fallback to ignored timeout_ms or graceful cancel. Auth stays auth, not unsupported or not-found. Native old-client behavior remains usable. |
| B19 | Local | Invalid PID (0, negative, boolean, fractional, unsafe TS integer); invalid duration; unknown option; stdin true/text; retain false; Python background callbacks; conflicting wire timeout fields | Reject before any new process or file mutation; guard marker unchanged. Supported defaults and Python fractional-seconds conversion pass positive controls. Unsupported methods do not silently return empty success. No credentials/env values appear in metadata or proxy evidence. |
| B20 | Mixed | Installed-package type/import checks; two TS waits; Python repeated terminal waits; real exit, remote timeout and local abnormal-outcome controls | Literal true/false/dynamic boolean types match; no E2B package at runtime. TS live fields and callback placement differ correctly from Python wait callbacks. Waits share/cache output without duplicate callbacks. OOM/eviction/unknown and cancellation retain local diagnostic families, not normal exit 0. |

## W04: detached build and artifact handoff

Run a Python-to-Python and TS-to-TS workflow, plus Python-to-TS and TS-to-Python
handoffs. Use a local deterministic build fixture with no external network:

1. Controller A creates a sandbox with sufficient lifetime (no Python context
   manager that would delete it on exit), uploads source/input and starts a
   gate-controlled artifact build with background true and a documented timeout.
2. Save only sandbox ID and PID as the migration application's task reference;
   evidence may additionally record native execution ID. Confirm READY,
   disconnect, and let A terminate. Do not call sandbox.kill here.
3. Controller B reconnects to the sandbox, then the running command. After the
   attach acknowledgement, release the gate through the file API. Observe
   progressive POST output, wait for the actual exit and fetch the artifact.
4. Compare artifact bytes and digest against the fixture's independently
   specified expected output. Verify one build by the counter. Do not derive
   expected bytes from the artifact being tested.
5. Repeat with a deliberate nonzero build, a timed-out local observation and
   explicit kill. Distinguish ordinary failure, local wait failure and remote
   termination. A separate late-attach variant finishes before B connects and
   still returns the retained exit plus downloadable artifact, without invented
   historical callback output.
6. Explicitly delete the sandbox in finally and independently prove zero owned
   containers. A failed cleanup makes the workflow fail even if the build passed.

## Integrating the existing gate

During implementation, preserve an archived v0.0.2 report and record the
semantic change rather than editing old results to appear green:

- **A05:** replace the false run-timeout-continuation parity assertion with
  remote deadline enforcement; move local-only wait continuation to B04.
- **W03:** retain progressive foreground output and explicit cleanup; its
  timed-out run must not imply remote work continues past its command deadline.
- **A14:** accepted-start loss still increments once, but only a surviving
  deadline permits a later marker. Use distinct local request loss and remote
  timeout controls; preserve uncertainty assertions.
- **A12:** background true becomes supported; replace its blanket rejection
  with malformed-background and still-deferred-option controls. All other
  rejection assertions remain independently required.
- **A16:** add an actual old-server/missing-capability case rather than naming
  untested version-pairing behavior in a passing case title.

All other A/W assertions remain gates. `background-reference.json` overlays
`cases.json` and `reference.json` without rewriting the released baseline;
both language workers, selfcheck and the suite digest include the new profile.
`compare` must reject a
missing language, failed/not-run case, mismatched baseline, absent fault
evidence, partial selection or unconfirmed cleanup. The existing 19-case gate
does not certify this design merely because it remains green.

Evidence must include source/image/package revisions, guest identity, process
and request IDs, per-handle attach watermarks, clocks, callback transcripts,
raw terminal states, exact side-effect/artifact checks, fault logs and owned-
resource cleanup. Hash large payloads; redact credentials and unprinted env.
Report source-inspection checks separately from executable local assertions.
Any future live cloud comparison requires separate authorization and records
its actual runtime/template; it is not a prerequisite for local implementation.
