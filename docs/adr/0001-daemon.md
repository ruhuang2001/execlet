# 0001. Daemon execution and recovery

Accepted. These decisions govern the HTTP service and its container backend. The API reference defines request fields, limits and response shapes. [HTTP contract](../api.md)

## HTTP contract

The route table pairs each operation with its handler method. Construction fails on a missing handler because a mux can report a matching pattern even when the fallback cannot serve that operation. Contract checks compare the registered operations, the embedded OpenAPI document and the endpoint headings in the API reference. Liveness stays available during runtime failures. Readiness requires evidence from the runtime before admitting traffic to a ready instance. Health probes stay public when bearer authentication protects the API.

The HTTP and Docker JSON codecs use Go's legacy wire options to preserve field-name matching, acceptance of additive fields, nulls, omission rules, HTML escaping and UTF-8 replacement. Administrative policy and recovery files use strict decoding. An optional JSON body may be empty. A supplied body must contain one complete value. File uploads read far enough to distinguish excess bytes from a complete file at the limit.

## Container backend contract

The daemon calls backend methods concurrently. A backend returns the running container's ID from create, applies the requested PID limit at creation, and addresses later operations by that exact ID. A stable container name is a recovery hint. Docker removal, pause and unpause map a missing container to `ErrNotFound`. The liveness probe reports a missing container as a state. Pause and resume cover every process in the container.

Pass execution argv intact and apply the requested environment entries over the image environment. An empty working directory uses the image default. If a requested directory is unusable, return an observable process with a diagnostic on stderr and a nonzero exit. Recording a process preserves the execution history and cancellation path. Signals target the execution’s process group. Docker’s ordinary `Wait` waits for the output pump to finish, then inspects the tracked exec until it stops. It returns that exec’s exit status, or -1 if Docker cannot supply it. Completion establishes the state of that exec. Descendants that have closed or redirected their output can outlive it. [Backend interface](../../internal/daemon/backend.go)

## Execution identity

The backend receives argv as arguments. It layers per-command environment overrides over sandbox defaults, and carries one-shot stdin separately. A missing working directory should produce stderr and an exit status on the execution record. Checking existence before launch would add a round trip and race with filesystem changes. Request validation checks the shape and size of the input before sandbox lookup. The API echoes a requested working directory for diagnostics and uses null for the image default.

A replay identifies one invocation. The comparison includes argv, cwd, the execution environment, stdin and the managed millisecond timeout. Names are sorted, and the keyed fingerprint incorporates the number and length of the parts. This gives the same environment in a different map order the same identity and distinguishes different argument boundaries. History retains fingerprints and an environment entry count because raw input may contain credentials. A process-local key serves memory-only histories. Durable stores keep their key in the private journal so comparisons survive restart. Access to that key defeats the protection against offline guessing. Environment limits cover both entry sizes and aggregate bytes, and argv accounting charges each separator to bound empty arguments. Decoded stdin and its larger escaped JSON representation have separate limits.

Sandbox state is checked before replay. Once a start is accepted, its process must survive the HTTP request that launched it. The start handshake has its own bounded context. If the runtime returns a process alongside an error, the daemon still observes it. With an uncertain start and no process observer, allowing another invocation risks executing user code twice. The daemon retains the uncertain invocation and retires its sandbox. Managed signals use an unreaped process lifetime because a number alone can refer to a later incarnation. An in-guest supervisor owns managed deadlines so a second timer cannot overwrite an already settled exit.

## Completion and attachment

When an operation needs multiple locks it takes the sandbox lock, then the execution lock, then the event-log lock. Completion removes the active execution and resolves sandbox loss while holding the sandbox lock. A polling client can start another command immediately after observing completion. This ordering makes the next start see the freed slot or the terminal sandbox. Concurrent eviction and process exit can both reach completion, so a single writer publishes the terminal event.

PID attachment searches the retained executions newest first to handle PID reuse. Output pumps advance independently of the execution lock. The attach operation captures retained output and its event watermark together under the log lock while holding the execution lock. Reconnecting after that watermark therefore starts at the chunk following the snapshot.

## Termination evidence

The first recorded cancellation or timeout intent explains signals the daemon sends. Without intent, an increase in the container's cumulative OOM counter supplies evidence only when the sandbox has one configured execution slot. A start retries its baseline read once within a shared deadline. Missing counter evidence cannot establish OOM. With multiple slots, an unexplained 137 in a surviving container yields unknown because the counter covers siblings too.

A stopped container can briefly report running, and a running container can briefly appear missing. On an ambiguous 137 the daemon samples the exact container, using a count derived from the nominal window and interval. Once the minimum number of attempts has elapsed, two successive successful observations of stopped can settle the verdict early. Other paths use the last successful observation. If every probe fails, the daemon treats the sandbox as lost. Logging the observed sequence and elapsed time makes a disputed verdict diagnosable. A failed process wait leaves the exit code unknown and changes an ordinary exited verdict to unknown.

## Cancellation and deadlines

Ordinary cancellation resumes a paused sandbox before signaling the process group. The TERM grace period and the subsequent KILL confirmation wait proceed independently of the caller's connection, since a disconnect could otherwise trigger premature destruction of the sandbox. Failure to confirm completion leads to a container removal attempt and a final bounded wait. The ordinary cancellation API returns the latest execution snapshot, which may still be running. A managed kill that requires container removal reports a backend error so callers can see the collateral effect on siblings.

Lifetime kills count through pauses. A lifetime pause policy arms a fresh interval on resume so the sandbox can make progress. Replacing the lifetime updates this policy. Extending it preserves a later existing deadline and treats zero as a no-op, so reconnecting cannot inadvertently clear the deadline. Open streams and active commands suppress idle expiry. Authenticated activity, execution completion and closing a stream update the activity clock.

A command's own deadline is absolute. The reaper checks due sandbox deadlines first, and claims due execution deadlines only while the sandbox is running. Claiming each deadline once and running its escalation separately prevents signal waits from stalling every sandbox. A pause racing a claimed timeout may require a resume to deliver signals. A runtime error after a pause request can arrive after the container actually froze, so the daemon reconciles liveness before repeating that transition.

## Stream and retained output

Replay and snapshots share one append-only event log. A lazy index builds stdout and stderr from newly appended events, so repeated reads of a completed execution reuse the same text. Summaries avoid materializing either stream. A terminal event acquires its output from that same index when delivered, avoiding duplicate output in history and the persisted terminal record.

Each JSON frame encodes its text separately. A pump carries an incomplete UTF-8 tail into the next read and cuts bounded chunks on rune boundaries. A truncated sequence at EOF receives the wire codec's replacement character. Byte and event budgets protect against both large writes and many small writes. The log retains the prefix that fits, emits one truncation marker containing the byte limit, and allows the process to continue. An open stream has a per-sandbox resource cost and suppresses idle expiry, so the daemon limits simultaneous streams.

A subscriber advances over dense event IDs, rejects a cursor ahead of the current log, and closes as soon as it delivers the terminal batch. A separate ping writer allows a terminal event to arrive immediately. Both writers serialize the write and flush together to protect the HTTP buffer. Replays batch frames to reduce flushes. Stream shutdown joins the ping writer before HTTP can reuse the response writer. A shutdown signal wakes streams because HTTP graceful shutdown waits for handlers to return.

## Admission and cleanup

Admission reserves a sandbox slot before container creation. A pending delete and failed create can race, so releasing that slot must happen once. A container that appears after deletion still requires removal. The ordinary live-slot count tracks the API lifecycle. With deployment policy enabled, aggregate reservations reflect declared resource limits and continue through unconfirmed removal. Recovery verifies and reserves eligible containers before serving traffic under that policy.

A task limit accounts for threads created by runtimes and by the guest helpers themselves. The minimum leaves room to launch those helpers. Bridge networking supports package installation. The allowed network modes keep sandbox processes in their own namespace. A policy can further restrict networks and provide a finite CPU default.

A killed API record can outlive a failed removal. Repeated delete reports a backend error while removal remains pending, and background retries keep the container discoverable independently of the record. A missing container confirms cleanup. The retry set has a separate lock. One bounded pass runs at a time so slow removals cannot occupy the reaper for an entire queue. Timeout and eviction checks skip busy sandbox locks. Batch eviction takes its list of candidate IDs before inspecting the runtime so containers created during inspection cannot appear missing. An unsuccessful batch inspection supplies no evidence of eviction. Ephemeral shutdown limits concurrent removal calls and gives the sweep its own budget.

## History and recovery

The daemon limits live admission, terminal sandbox history, per-execution history and aggregate retained output separately. Per-command output bounds multiply across sandboxes. Pruning removes eligible old records as a whole and clears their idempotency keys, so an old key may start a new command after its history expires. Active executions and the most recently finished result in each sandbox stay available to clients. This protection and lock contention can keep the aggregate above its target. Removing an entry clears its backing-array reference so the collector can reclaim it.

The optional local journal holds a single-writer kernel lock and validates its version, owner and stored records before reconciliation. Recovery uses exact owned container IDs. A journal write failure during execution completion produces an unknown outcome. Interrupted live output is incomplete, so replay from a pre-crash nonzero cursor is rejected. A surviving container alone supplies no proof of command completion. Recovery records the terminal sandbox state before finishing uncertain executions, removes unsafe containers and owned orphans, and never replays commands. With policy enforcement, a paused container cannot run the kernel verification needed for adoption.

A manifest drops a reference before its corresponding history file is removed, so an interruption can leave an extra file without invalidating the acknowledged live history. Closing the journal excludes late writes before unlocking. A durable shutdown preserves eligible idle sandboxes and drains active work. On a later restart, remaining live intents follow the same conservative recovery path. Idle activity marks a record dirty only when the clock affects recovery, to avoid rewriting unchanged records every beat.

## File operations

The daemon validates an entire bounded archive before preparing the destination sandbox. It validates runtime-produced bytes before returning them too. Encoded size, extracted payload and member metadata have independent bounds, and callers receive detached copies of validated data. Extraction can partially apply on a runtime failure. Callers coordinate competing writers when they need consistent results.

A listing retains one extra name past its requested page so it can report whether more children exist without holding the entire directory. Raw ownership IDs survive missing account names. A listing preserves symlinks and represents special filesystem objects explicitly. The in-memory backend preserves existing hard-link contents when a path is replaced.

## Observability

Metrics label requests by fixed operation template and outcome to bound cardinality and keep paths, commands and credentials out of labels. Routing redirects can produce caller-supplied paths, so request logs resolve an allowed route template first. Request IDs accept a bounded safe alphabet. Error logs record safe codes because runtime messages can contain sensitive input. HTTP instrumentation exposes flushing only when the underlying writer supports it.

## Deterministic test backend

The HTTP and daemon tests share a backend that runs Go functions, records detached argument and environment slices, and can simulate missing containers or failed signals. This lets a test distinguish signal delivery from container removal. Gating a create exposes the pending state to a concurrent reader. Tests can advance the sandbox clock, wait for an asynchronous removal pass or invoke a retention pass directly. Polling for completion without a sleep exercises the window in which a client may issue its next request.
