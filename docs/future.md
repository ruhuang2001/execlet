# Future directions

The current source covers lifecycle, execution, E2B background handles,
capability checks, opt-in aggregate policy, conservative restart recovery,
metrics and a repeatable warm-image latency/fault suite. SDK distributions are
built and installed in CI; registry publication still requires owner setup and
an explicit release. See [operations](operations.md) and [versioning](versioning.md).
The capabilities below are **not implemented**, not committed delivery plans.

| Capability | Next design step | Main cost or risk |
|---|---|---|
| Filtered outbound networking | Add an enforceable allowed-destination mode beyond current `none`/unrestricted `bridge` | DNS rebinding, host/private/metadata routes, IPv6 and fail-closed enforcement |
| Templates and snapshots | Immutable, versioned images; separate filesystem snapshots from memory checkpoints; record runtime compatibility | Storage, cold-start time, misleading "restore" semantics |
| Persistent volumes | Define mount ownership, quota, and cleanup independently of the sandbox lifetime | Data isolation, orphaned storage |
| Secret injection | Short-lived, narrowly scoped credentials; redaction in logs and server-side audit | Leakage through logs, environment variables, or untrusted code |
| Multi-tenancy and billing | Tenant-scoped scheduling, quotas, metering, and explicit resource termination | Noisy neighbours, authorization, billing correctness |
| Browser automation | Isolated browser images, network controls, artifact capture, session timeouts | Larger attack surface, heavier resource use |
| Additional SDK languages beyond Go, Python, and TypeScript | Generate plain requests from the stable OpenAPI core, then implement and verify retries, replay, errors and cancellation per language | Semantic drift between clients |
| Kubernetes or microVM backend | Implement the existing `daemon.Backend` interface and verify lifecycle and execution semantics against the new runtime | Pause, OOM, eviction, and network semantics differ |
| Live execution recovery and HA | Extend the current single-writer local journal without replaying interrupted commands | Durable live output, ownership fencing, process identity and multi-host reconciliation; not equivalent to memory checkpoints |
| Capacity and cold-image benchmarks | Extend current fresh/reused-container latency samples to image pulls and sustained load | Hardware-dependent results, journal fsync cost, tail latency and cleanup saturation |
| Persistent output storage | Quota-backed stdout/stderr and replay buffers, sharded fetches for large results, an explicit over-limit error | Preserve explicit truncation signals and retention limits across restarts |

The entry gate for any capability is an end-to-end scenario that exposes both its value and its failure mode. Matching method names do not prove that two runtime backends behave alike; a pause in particular must state whether it preserves memory, files, or both.
