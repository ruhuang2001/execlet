# File operation failure model

Written before implementation. Verify through a live authenticated daemon and real Docker; retain JSON responses, commands, server logs, SDK results and owner-scoped cleanup evidence. No new unit tests.

## Directory listing

- Include hidden names, whitespace, Unicode, empty directories and symlinks; only immediate children appear. Metadata describes the entry without following child symlinks.
- Real-Docker empty directories return an empty array. Missing paths return 404; files return 400. Paused sandboxes resume; killed sandboxes reject access.
- Deterministic bytewise name order; keyset pages have no duplicates and the last page omits next_cursor. Concurrent mutations do not imply snapshot consistency.
- Invalid limits, malformed or mismatched cursors, traversal, relative/NUL/overlong paths and duplicate query values return 400.
- Bound archive bytes scanned, entries scanned, request time and retained metadata; exceeding byte/entry limits returns 413, while the 30-second runtime deadline returns 502 backend_error.
- HTTP, Go/Python/TypeScript SDKs and CLI preserve encoding, error codes and page metadata.

- Hardlink sizes resolve from targets anywhere in the scanned tree; symlink size is target-path bytes. Memory listings normalize paths and scope scan limits to the requested subtree.
