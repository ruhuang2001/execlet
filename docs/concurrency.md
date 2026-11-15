# Concurrent executions: design and failure model

Status: design reviewed and implemented; real Docker and SDK E2E evidence is generated under artifacts/.

## Contract

A create-time integer max_concurrent_executions defaults to 1 and accepts 1–8.
It is immutable, appears in Sandbox responses, and is exposed in OpenAPI, CLI,
and all three SDKs. Omission preserves current behavior; explicit 0, negative,
and values above 8 are invalid_request. Admission at capacity returns busy.
An identical idempotency replay is checked before capacity and uses no slot.

Pause continues to freeze the entire container and every process. Resume resumes
all processes; starting or canceling in a paused sandbox still resumes it first.
A suggested "pause cancels all" policy conflicts with the existing published API
and would destroy resumable work, so it is intentionally not adopted.

## State, locking and retention

Replace sandbox.running with an execution-ID map. Sandbox mutex remains the
admission boundary across validation, backend start, and map insertion. Failed
backend starts consume no slot. Finish, map removal, activity update, and history
pruning happen atomically under the same mutex before exposing completion.
Lock order remains sandbox then execution; no execution lock crosses acquisition
of the sandbox lock. Slow backend admission still serializes starts briefly,
while admitted processes run concurrently.

Every active record is protected from history/output pruning. Pruning scans past
active records to find the oldest completed eligible record, preserving the
newest completed result. Count-based history pruning runs at admission and
completion so many overlapping finishes cannot leave excess history indefinitely.
Idle time accrues only when the active map is empty and no streams remain.
The memory backend tracks all processes and removes/signals every live process
on container removal; it no longer enforces its own single-process restriction.

## Deadlines and terminal outcomes

The reaper claims every due execution deadline exactly once while holding the
sandbox lock, then escalates each independently outside the lock. Paused time
still counts; a due execution is acted on after resume as today.
Cancel targets only the selected execution's process group. Delete, lifetime
kill, idle kill and shutdown record the appropriate intent on ALL active
executions before removing the container. First recorded intent continues to win.

A cancellation fallback that must remove the container marks unaffected siblings
canceled; a timeout fallback marks them timeout. The target retains its earlier
intent. This is honest container-wide collateral termination, documented as the
existing removal fallback extended to concurrent work. External eviction finishes
all active executions as evicted unless an earlier explicit intent applies.
Completion rechecks intent under the sandbox lock, so a classification obtained
before a racing delete cannot overwrite its recorded terminal reason.

## OOM attribution

A sandbox configured above one slot never uses container-wide OOM deltas for a
per-execution outcome, even when only one execution currently runs. Recorded
intent remains authoritative; confirmed container loss is evicted. Otherwise an
unexplained exit 137 is unknown; other exits are exited. Single-slot sandboxes
preserve current OOM behavior. The classifier receives an explicit attribution
policy rather than overloading a failed counter baseline. Go/Python/TypeScript
recognize unknown as a non-success outcome, preserving execution evidence.

## Failure matrix written before implementation

Real Docker E2E and saved reports must demonstrate:

- Default one-slot compatibility; limits 1/8 accepted, 0/-1/9 rejected; field
  survives create/get/list and all SDK request/response types.
- Two processes overlap and a third at capacity is busy; simultaneous starts
  never over-admit; completed/canceled slots become reusable without transient busy.
- Idempotent replay while full returns the original ID and does not consume slots.
- Cancel one leaves sibling alive; timeout one leaves sibling alive; two deadlines
  are both claimed, including deadlines passed during pause.
- Pause freezes both (no progress), resume restores both, delete/kill finishes both,
  external Docker eviction closes both streams, and container removal fallback
  terminates siblings with documented reasons.
- Active executions survive history and output pressure; oldest completed records
  can be pruned even with an older active record; retained keys follow their records.
- Idle does not fire while any execution or stream remains; it starts after the
  last completion. Lifetime timeout remains container-wide.
- Concurrent explicit exit 137 is unknown, not oom or success; ordinary exit 0
  succeeds, explicit cancellation remains canceled, single-slot OOM remains oom.
- Go, Python and TypeScript each demonstrate overlapping executions, isolated
  cancellation and unknown-outcome handling via public SDK entry points.
- Race-enabled existing suite (including TestExecutionHistoryIsPruned), vet,
  lint and all CI E2E gates pass. No new isolated unit tests after implementation.

Artifacts include probe commands, IDs, state/outcome snapshots, logs, and cleanup
checks. Each runner starts its own daemon on an isolated port and cleans only its
owner-labeled containers; no global Docker cleanup. Fault injection, if required
to force the fallback, wraps real Docker transport without replacing execution.

## PID-file cleanup failure model (before implementation)

The stress run found completed exec PID files fill the 1 MiB runtime tmpfs after
roughly 256 starts. Cleanup must unlink exactly the generated PID path only after
Docker confirms exit; never recursively remove the directory or erase a live
sibling's PID file. Serialize cleanup with signal helpers so a concurrent cancel
cannot read a removed or recycled PID. Cleanup must be bounded and best effort
if the container has already vanished. Verify more than 300 successful sequential
executions, a fresh execution afterward, and near-zero completed PID files while
an older sibling stays active. Existing cancellation/fallback probes must still pass.

## Combined stdin failure model

Two active executions must consume different staged stdin payloads without sharing
file paths or data. Canceling one must leave the other running with its original
input. Both completion paths retain their own stdin fingerprint and must remove
staging directories and completed PID files. A subsequent probe should find no
stdin directories and only its own PID file. The standalone stdin gate continues
to exercise EOF, size limits, binary/UTF-8 input and startup-failure cleanup.
