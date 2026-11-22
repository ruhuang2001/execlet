# ADR 0004. Evidence for builds, compatibility and runtime behavior

Accepted for the release and acceptance harnesses.

## Build and release evidence

Commit design and contract documents. Regenerate build outputs, coverage profiles and run evidence for the revision under test. Combining unit and real-backend coverage captures paths that only one suite reaches. Both use atomic counters so Go can merge their profiles. The gate counts covered profile blocks. A package's statement-weighted percentage measures a different quantity.

A package published with OIDC provenance must identify the exact tagged source SHA that passed acceptance. The release workflow verifies the dispatch ref, resolved tag and commit before it builds packages.

## Compatibility evidence

The compatibility reference comes from the pinned E2B SDK declarations and implementation. A cloud run can measure drift independently. Compile the shared TypeScript application against both the reference and installed local declarations, including negative overload checks. Execute the local worker with just the installed tarball. Keep template and image selection on create. Workers translate spelling and time units so both providers run the same assertions. Error families retain runtime diagnostics. Missing provider packages yield a skipped observation, while broken dependencies fail the run. A child's persisted report supplies the scenario outcome. A fabricated report can verify the comparison gate only.

Record hashes for large deterministic outputs. Construct nonfinite values inside the worker because JSON cannot carry them, and preserve Unicode when sending shell-embedded fixtures. Separate stdout and stderr can arrive in either order. The stream observer acknowledges frames before the guest sends its next write. Separate writes exercise split UTF-8 sequences while the assertion compares the resulting data. The termination fixture deliberately ignores TERM to distinguish graceful cancellation from escalation. Memory pressure runs inside the verified fixture limit.

The fault proxy handles Docker's negotiated version prefix, streams uploads, and allows idle attached executions to finish. A watchdog bounds each case and leaves time for cleanup. Arm faults after observing readiness, direct independent CLI observations around the daemon's private proxy, and tolerate a container disappearing between list and inspect. Empty the case's owner group before switching daemons. Stream truncation and a local wait deadline can leave a command running. Observe its own marker or completion, then isolate the next probe. A file side effect can precede execution reaping. The accepted-start-loss probe waits for the lost command to finish before reusing its slot.

## Runtime evidence

Each run scopes cleanup and leak checks to its own daemon owner. Use the exact container identity for fault injection. Verify shutdown before starting a replacement daemon that could sweep leftovers. Keep an event stream open across graceful shutdown and bound the drain time. Verify crash recovery against an independently observed orphan. Join background CLI processes during cleanup and retain primary and cleanup errors together.

Direct the concurrency runner at its own daemon. A Docker attach acknowledgement can precede the first process instruction, so observe progress before testing pause or eviction. Exercise history count and byte pressure separately. A concurrent sweep can finish before cancellation acquires the execution. Bound escalation scenarios for the complete signal retry and removal window. The observability proxy uses one request per connection so each fault change applies at a known boundary.

Exercise the CLI's SIGINT path by signaling the CLI process. Validate input before creating a sandbox or reading stdin. Inspect the sandbox state to distinguish the execution deadline from expiry of the sandbox. Read the environment with a process that preserves entry names. Resolve working directories physically, including symlinks and missing path components, and distinguish a wrapper startup failure from an application exit.

Verify hardening from inside the container. Read cgroup limits at their host-appropriate paths and require observed PID enforcement before applying load. Check network modes using the interface list so the result is independent of external egress. Compare with the deployment default. Include real file-route runs in the combined coverage evidence.

Run fork and descriptor-pressure checks in isolated Python processes so the threaded proxy and runner keep their own interpreter state and descriptor limits. Exercise an installed file utility that rejects an option separately from a missing utility. Alternate short-command order across fresh containers to spread first-command cost, and discard a full warmup lifecycle before computing latency percentiles.
