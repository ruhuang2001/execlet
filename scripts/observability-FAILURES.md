# Observability and readiness failure model

Written before implementation. Verify through an authenticated daemon, a real
Docker socket proxy, HTTP requests, SSE, JSON logs, and retained artifacts.

- Liveness must remain public and successful when the Docker probe fails.
- Readiness must remain public, call Docker's unversioned /_ping, require HTTP
  200 and body OK, and return 503 within its two-second probe deadline on a
  refused connection, stalled response, malformed body, or error status.
- Every handled HTTP response must carry a request ID: retain a single 1–128
  character ASCII alphanumeric/dot/underscore/hyphen ID; replace missing,
  duplicate, invalid, or excessive IDs with a standard UUID.
- Access/error/lifecycle records must correlate through request_id; execution
  completion must retain the start request's context after HTTP returns.
- Logs must be structured JSON and contain method, route template, status,
  duration, and byte counts without commands, authorization, environment,
  query values, file paths, unrecognized request paths, or raw runtime errors.
- Middleware must retain SSE flush capability: an early output event must
  arrive before a delayed command completes, and replay must still complete.
- Daemon shutdown and startup cleanup must remain observable and scoped to
  its unique owner label; E2E teardown must leave no owned containers behind.

Repeatable evidence: scripts/e2e-observability.sh writes responses, request IDs,
SSE timing, daemon JSON logs, proxy failures, report.json, and cleanup.json.

Review follow-up failure (recorded before repair): eviction can win terminal
finalization before the runner, leaving only a start log. Delay real Docker exec
inspection in the private proxy, externally remove the owned container, and
require exactly one correlated completion record matching the stored outcome.

Additional review failure cases (before repair): Docker /_ping redirects must
not be accepted as healthy; a stalled proxy must outlast the deadline assertion;
HTTP panic unwinding must not record an implicit 200. Every captured response,
including direct SSE, needs its correlation header. Startup wait exhaustion
must identify readiness failure, and bind/shutdown errors must retain their
safe infrastructure cause rather than only a generic message.
