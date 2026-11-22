---
name: delete-comment
description: Audit and remove Tiny Sandbox code comments, migrate design decisions into ADRs, track implementation workarounds, and verify the comment ratchet.
---

Run from the repository root. Use the requested paths, or the whole repository if no section is given. Capture the starting Git revision and working-tree status. Prepare the scanner with `make comment-lint-env`. Run `.venv/comment-lint/bin/python scripts/comment-lint/check.py --inventory` to collect locations and text before editing. Add `--revision REV` for a committed snapshot. Review executable snippets in strings or examples that the host-language parser treats as data.

Read each comment alongside its implementation, callers and relevant contract. Record its original path, line range, category and disposition. Preserve interpreter, compiler and linter directives and user-visible documentation strings. Check code examples and rendered API docs when deleting surrounding prose.

| Category | Action |
|---|---|
| Tautology | Remove wording already expressed by the code. |
| Stale | Verify the current behavior and remove the obsolete claim. |
| Excuse | Identify the observable compromise, check the issue tracker for an existing match, file the requested follow-up when needed and record its issue URL. Remove the comment. |
| Decision Record | Move the current choice, reason and consequence into the relevant ADR under `docs/adr/`, then delete the source comment. |

Run `make comment-lint comment-lint-e2e` and the affected build or E2E entry points. The lint E2E writes `artifacts/comment-lint/e2e.json`. Save the detailed classification alongside it as `artifacts/comment-lint/ledger.tsv`. Set `COMMENT_BASE` to the intended merge base when necessary. The ratchet uses each existing comment's exact text and path from that revision. A deletion lowers the allowance on the next merged revision. For the savings estimate, count distinct physical lines occupied only by removable comments. Report trailing comments, retained directives and net diff size separately. Include the inventory or range ledger with the handoff.
