# File operation failure model

Written before implementation. Verify through a live authenticated daemon and real Docker; retain JSON responses, commands, server logs, SDK results and owner-scoped cleanup evidence. No new unit tests.

## Directory listing

- Include hidden names, whitespace, Unicode, empty directories and symlinks; only immediate children appear. Metadata describes the entry without following child symlinks.
- Empty directories return an empty array. Missing paths return 404; files return 400. Paused sandboxes resume; killed sandboxes reject access.
- Deterministic bytewise name order; keyset pages have no duplicates and the last page omits next_cursor. Concurrent mutations do not imply snapshot consistency.
- Invalid limits, malformed or mismatched cursors, traversal, relative/NUL/overlong paths and duplicate query values return 400.
- Bound archive bytes scanned, entries scanned, request time and retained metadata; exceeding scan limits returns 413.
- HTTP, Go/Python/TypeScript SDKs and CLI preserve encoding, error codes and page metadata.

## Tar transfer

- Round-trip nested directories, empty files, binary content, permissions, Unicode and safe internal links without host extraction.
- Reject malformed/truncated tar and trailing nonzero bytes. Validate all headers and payloads before mutation; invalid later entries must leave earlier entries unwritten.
- Reject absolute/traversal names and link targets, duplicate/conflicting entries, device/FIFO/sparse entries and unsupported metadata. Bound encoded bytes, entry count and total regular bytes (413), invalid archives (400).
- Existing destination symlinks and link entries cannot redirect writes outside the destination. Document concurrent container mutations as outside hardened isolation and fail closed where detectable.
- Verify lifecycle, missing/file destinations, cancellation, SDK bytes and CLI transfers.
