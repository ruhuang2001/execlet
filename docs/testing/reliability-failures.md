# Execution reliability failures

These regressions cover the four failures reproduced on source `1774cf4`.
They do not change the historical v0.0.2 contract or claim stronger isolation.

## Container name conflicts

Verify that a matching name with a different owner, a different sandbox ID or a missing legacy ID label preserves the original container. A live matching container also keeps its identity. Policy compensation must preserve the same protected fixtures and release its reservation. An exited fixture with both expected labels can be replaced. A lost create acknowledgement requires verified cleanup even when the caller received no Docker ID. Record the fixture and replacement IDs from the Docker Engine and check cleanup by ID.

## Runtime: real Docker behind a private fault proxy

- Normal keyed start and replay execute one side effect and return one identity.
- Forward a user command to Docker, observe its marker, then lose the start
  response. Retrying the same key must never execute the command again. Retain
  the uncertain execution as `unknown`, with no exit code and incomplete output.
  If no process handle can be recovered, remove the sandbox conservatively;
  report collateral loss, not successful command cancellation.
- Hold the start response after Docker has acted. The handshake must return
  within its five-second budget plus bounded cleanup; queries and lifecycle
  cleanup must recover without waiting for the proxy to release the response.
  Another sandbox must remain usable. Never retry the Docker start itself.
- Keep a normal command's output stream open longer than the handshake budget.
  It must finish with all output after its initiating HTTP request has ended.
- Repeat uncertain starts with durable state enabled and with managed commands;
  preserve existing PID-handshake behavior when an observable process exists.
- Failed removal must stay visible and retryable, including resource reservations.
  All verification containers are owned by this run and independently removed.
- Directory listing under the non-root compatibility image must use the bounded
  Docker archive for an unreadable directory or an incompatible `stat`. A
  readable directory with normal tools must not request an archive. Check both
  the authenticated result and the Docker request, since an empty result alone
  cannot distinguish fallback from a shell glob that silently skipped entries.

## Go SDK: public HTTP contract

- Callback errors (ordinary, typed SDK, and wrapped SDK errors) stop observation
  at once, preserve `errors.Is`, never reconnect, and never cancel remote work.
  A callback that also ends the local context still returns its callback error.
- Automatic cancellation through StreamExecution, ExecStream and canceled-start
  recovery must verify the execution identity, terminal state and reason. A 200
  response with running/missing/mismatched outcome is `cancel_unconfirmed`,
  retaining the requested identity and start key when available.
- Confirmed terminal cancellation still reports `ErrCanceled`; transport failure
  remains unconfirmed. ExecStream must not relabel uncertainty as cancellation.

Run the public SDK regressions with `go test -race ./sandbox-go`. Runtime cases
belong to `scripts/e2e-operations.py`, which is exercised in installed E2B CI.
Full acceptance also includes native Docker/CLI, files, stdin, observability,
installed Python/TypeScript SDKs and the 40-case-per-language background profile.

## Concurrency acceptance transport and diagnostics

`scripts/e2e-concurrency/run.py` connects directly to the supplied daemon URL,
ignoring environment and system HTTP proxies. It sends an `X-Request-ID` for
each request and never retries a failed request. This isolates local acceptance
from ambient proxy failures; it does not establish the cause of a historical
connection reset. SDK proxy behavior is unchanged.

`concurrency.json` retains successful and failed checks, every known owned
sandbox ID, the original exception type/message, and each cleanup outcome.
Transport and response-decoding failures include the method, path and request
ID in `request_errors`, for correlation with `sandboxd.log`. Unexpected HTTP
statuses include the same identity in the assertion error. Request bodies and
headers are not added to the transport diagnostics.

Cleanup attempts each known sandbox once even if an earlier DELETE fails. A
failure does not replace the original exception; cleanup-only failures also
set `passed: false` and exit nonzero. The outer E2E runner still owns the final
daemon shutdown and owner-scoped container sweep.

Run `python3 -B scripts/e2e-concurrency/test_runner.py -v` to exercise the runner
against a rejecting proxy and a private HTTP fixture. The fixture resets the
primary request and rejects the first cleanup, while later cleanups succeed.
`scripts/e2e.sh` runs this regression before the unchanged real-Docker
concurrency scenarios. The socket fixture verifies the runner's diagnostics,
not Docker execution or the cause of the original intermittent reset.

## Cleanup and default admission

A runtime can reject every container removal while still accepting new creates.
If a delete frees its slot at the API transition to killed, the surviving
containers and their retry IDs can exceed the daemon's 64-slot bound. The
same risk applies when creation returns a container ID with an error, or when
a pending sandbox is deleted before Docker reports its container ID. Concurrent
retries and repeated deletes must release each reservation at most once.

The operations suite runs without a deployment policy, fills the store, and
uses the Docker proxy to refuse all removals. It checks rejected concurrent
creates, the cleanup backlog and the actual owned container set. After the proxy
recovers it checks that each container disappears, the backlog clears and
admission succeeds. Run this case alone with `--cleanup-only`.
