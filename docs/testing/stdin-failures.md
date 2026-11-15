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
