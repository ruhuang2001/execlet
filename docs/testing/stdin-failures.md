# One-shot stdin failure model

The request field is UTF-8 text, at most 1 MiB after JSON decoding. Omitted and
empty mean immediate EOF. The command receives these bytes once, with EOF at
the end; there is no interactive stream. A replay includes stdin identity and
does not stage or execute again. Execution responses do not contain stdin.

Real Docker E2E must cover:
- Exact Unicode, NUL, newline, quote, dollar and backtick preservation; no shell
  interpretation, and input isolation between successive executions.
- Empty/omitted EOF, a full 1 MiB escaped payload, and rejection of 1 MiB+1
  before process creation. Limit serialized JSON separately from decoded text.
- Same-key same-input replay succeeds; changed input conflicts without a second
  execution. Command/env/cwd continue working with stdin.
- Normal exit, missing command, unusable cwd, cancellation and timeout leave no
  staged input. The wrapper opens then unlinks before executing the command; a
  bounded cleanup path handles failures before the wrapper runs.
- Go/Python/TypeScript request types and retry copies preserve stdin; HTTP, SDK
  and CLI agree on output and errors. CLI reads no more than limit+1 bytes.
- JSON/get/list/SSE snapshots never echo input unless the program prints it.

Evidence: go-live.log (assertions, byte lengths and zero staged paths), daemon
logs, healthz.json, CLI output, source-revision.txt, and result.json with before
and after container cleanup counts in artifacts/stdin-e2e. Repeat with
scripts/e2e-stdin.sh. Python/TypeScript runners retain their own wire evidence.

Review coverage extension (before harness changes):
- Inspect untyped raw start/get/list/SSE bytes for a unique unprinted marker and
  any stdin field; typed SDK decoding alone cannot establish wire privacy.
- Assert decoded one MiB success and +1 rejection, execution JSON above eight
  MiB and ordinary JSON above one MiB rejection, and command aggregate above
  one MiB rejection. Persist statuses and lengths, not oversized payload dumps.
- Python and TypeScript each exercise omitted and explicit-empty input with
  bounded execution time and exited/0 outcomes, plus oversized SDK input.
- CLI byte-for-byte output comparison includes NUL, Unicode and trailing
  newlines. Invalid UTF-8 and oversized files fail before sandbox creation;
  invalid execution flags must fail before reading a blocking input stream.
- Assert cleanup immediately after every normal, missing-command, unusable-cwd,
  cancellation and timeout outcome; timeout exit_code may legitimately be null.
- Harness cleanup continues after individual removal failures and always
  records before/after owned-container state and the real process result.

No malicious staging-replacement result is claimed by this gate; it does not
currently intercept the staging handshake. After a confirmed exit, PID and
staged stdin files are removed together. A failed removal is retried without
waiting for the next command. If inspection is inconclusive, retry inspection
before unlinking files a live command may still need. If Docker still has no
record of the exec after a minute, leave its files for container removal
without treating the missing record as proof of exit. If the start itself is
unconfirmed and has no observer, retire the container instead of replaying it.

The raw HTTP/CLI gate retains privacy-start/get/list JSON, privacy-sse.sse,
raw request status/byte-count/hash records, raw-results.json, exact CLI input
and output for cmp, and source hashes. Failure reports name the failed property
without dumping a large request or execution payload. Per-case Go cleanup
checks and the Python/TypeScript EOF/oversize scenarios run against real Docker.
