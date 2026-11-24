---
title: "Migrate from E2B"
description: "What maps cleanly, what changes, and the differences the adapter keeps explicit."
---

execlet ships an E2B facade for Go, Python, and TypeScript. It is a **subset**,
not protocol compatibility: it implements the basic Sandbox and commands API so
a local application can move off a hosted sandbox without a rewrite. It does not
claim cloud parity.

<Warning>
Matching method names do not prove matching behavior. The differences below are
deliberate and documented rather than papered over. Read them before migrating
anything that matters.
</Warning>

## What maps directly

| E2B | execlet |
| --- | --- |
| `Sandbox.create(template)` | `Sandbox.create(image)` — a local Docker image |
| `sandbox.commands.run(cmd)` | Same, foreground or background |
| `sandbox.files.write/read` | Same, 10 MiB per file |
| `sandbox.setTimeout` | Replaces lifetime; may shorten |
| `Sandbox.connect(id)` | Preserves workspace and env, extends lifetime without shortening |
| `sandbox.kill()` | Same |

Settings come from the facade's own options, **not** from E2B environment
variables. `endpoint` + `api_key` map to `SANDBOX_DOMAIN` + `SANDBOX_API_KEY`.

## What changes, and why

### Timeout means "stop waiting", not "stop the process"

This is the biggest semantic difference and the one most likely to bite you.

In E2B, a command timeout throws and you assume the work stopped. Here, a
command timeout stops **your observation**. The process may still be running,
still holding the sandbox's execution slot. A callback failure or output
truncation can do the same.

The `TimeoutException` / `TimeoutError` retains the execution identity, the
idempotency key, and partial output. **Do not blindly re-run an uncertain
command** — use the identity to check what happened first.

<Note>
Native client cancellation is different and keeps its original behavior: it
cancels the remote work. The softer "stop waiting" semantics belong to the E2B
facade, where that is what E2B callers expect.
</Note>

### No PTY, no code interpreter, no pause

`stdin=True`, background handles beyond the documented contract, PTY sessions,
the code interpreter, pause/resume, and recursive listing are outside the
facade. Use the native SDK when you need them — they exist, they are just not
part of the E2B-shaped surface.

### Files are local tarballs, not volumes

Per-file limit is 10 MiB; combined output is bounded to 1 MiB; directory
enumeration is bounded. `AdapterError.code == 'output_truncated'` is **not** an
ordinary exit — treat it as data loss, not as a program result.

There is no persistent volume and no VM-isolation claim. If your E2B workload
depends on a volume surviving the sandbox, that does not exist here yet.

### Cleanup failures are errors

A backend cleanup failure raises, including on a repeated kill call. This is
deliberate: a sandbox you believe is gone but is still running is worse than an
error.

## The migration checklist

```text
1. Build or pin a local image that contains your runtime and dependencies.
2. Point endpoint/api_key at your execlet daemon; leave E2B_* unset.
3. Replace template names with image references.
4. Move per-file data > 10 MiB out of the files API.
5. Audit every `except TimeoutException` / `catch (TimeoutError)`:
   decide whether you meant "stop waiting" or "stop the work".
6. Add explicit kill/cleanup on every error path.
7. Run the acceptance gate against YOUR image, not the reference one.
```

## Verify, do not assume

The installed-package acceptance gate runs against a real daemon, real Docker,
and an immutable image digest. Run it with your image:

```sh
bash scripts/e2b-compatibility/installed.sh artifacts/e2b-installed "$(docker image inspect my-image:local --format '{{.Id}}')" --profile background
```

Only matched builds are tested. There is no old-server capability negotiation
and no cloud parity certification, so verify with the exact daemon revision and
image you intend to run.
