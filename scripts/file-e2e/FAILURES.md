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

## Tar transfer

Failure model written before archive implementation on its preceding branch.

- Round-trip nested directories, empty files, binary content, permissions, Unicode and safe internal links without host extraction.
- Validate every header and payload before mutation; invalid later members must leave earlier members unwritten. Invalid upload to a paused sandbox must not resume it.
- Reject malformed/truncated tar/end markers, trailing nonzero data, absolute/traversal names and targets, duplicate/conflicting entries, device/FIFO/sparse members and unsupported PAX metadata.
- Cap encoded bytes (32 MiB), total regular bytes (16 MiB), and entries (4096): 413 for limits, 400 for malformed/unsupported tar.
- Reject existing destination/ancestor symlinks and archive-created link ancestors. Concurrent in-container writers remain outside the atomicity boundary.
- Check all three SDKs plus CLI upload/download; verify safe links and hardlink inode equivalence inside Docker. Preserve tar fixtures, response JSON and scoped cleanup evidence.

- CLI local save failure must preserve an existing archive; simulate a file-size limit during an actual Docker download and verify no temporary file remains.
