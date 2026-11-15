# Environment failure model (written before implementation)

- Exact limits must pass; one byte/entry above 64 entries, 64-byte keys,
  8192-byte values and 32768 total NAME=value bytes must fail. UTF-8 uses bytes.
- Env ordering and empty/omitted env must replay; changed values, names, count,
  and cwd must conflict. Ambiguous concatenations must not collide.
- Retained records must contain only a keyed digest and count, never raw env.
  A process-random HMAC key prevents offline guessing from the digest alone;
  code inspection verifies retention because HTTP cannot prove memory absence.
- Start/get/list/events must not echo secrets. Later execs must not inherit env.
- Shell metacharacters in values must remain data; no shell injection.
- Cleanup must delete only this run's sandbox and record its outcome.

Run E2E_PORT=<unused-port> bash scripts/e2e.sh artifacts/e2e. The real HTTP/Docker
probe writes env-contract.json; the existing suite records logs and owner-scoped
cleanup. Evidence excludes request bodies containing synthetic secrets.
