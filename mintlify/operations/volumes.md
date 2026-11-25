---
title: "Persistent volumes"
description: "Keep files between sandboxes with bounded local storage."
---

Use a volume when files must outlive the sandbox that writes them. A volume
is local to one daemon. It does not preserve processes, executions, or the
sandbox's ordinary filesystem.

## Enable volumes on the Linux Docker host

Run the daemon with the privileges to allocate and mount loop-backed ext4
filesystems, a local rootful Docker socket in the same mount namespace,
`mount`, `mkfs.ext4`, and the embedded guest agent (`-guest-agent auto`).
Set `-state-dir` to a private directory on local storage and set a stable,
unique `-owner`. Do not enable this on a macOS Docker client, a remote Docker
socket, or a rootless engine.

Add both fields to a valid [admission policy](/operations/policy):

```json
{
  "max_volumes": 4,
  "max_volume_bytes": 67108864
}
```

`max_volumes` is a deployment count limit (1–256), and `max_volume_bytes` is
the largest volume a caller can provision (16 MiB–1 TiB, in whole MiB). The
daemon preallocates each volume's requested size under
`<state-dir>/volumes/<volume-id>/`. Filesystem metadata uses some of that
space. Lowering a limit prevents new allocations above it; it does not
resize or discard existing volumes. Watch host disk space separately.

## Create, mount, and reuse

1. Check `GET /v1/capabilities` for `volumes.v1`. The native SDKs do this
   automatically before a volume operation or a sandbox create with mounts.
2. Send `POST /v1/volumes` with `{"size_bytes":16777216}` and keep the returned
   `id`. Do not automatically retry an ambiguous create: list volumes first.
3. Include the ID in `mounts` when you call `POST /v1/sandboxes`. Supply an
   image admitted by your policy. This example uses `alpine:3.20`:

   ```json
   {
     "image": "alpine:3.20",
     "network": "none",
     "mounts": [{ "volume_id": "vol_REPLACE_WITH_RETURNED_ID", "path": "/mnt/work" }]
   }
   ```

4. Write under `/mnt/work` from a command or through the sandbox file API.
   You can create up to eight mounts per sandbox. Targets must be distinct,
   non-overlapping paths below `/mnt` (up to 4096 ASCII bytes). Another
   filesystem mounted beneath a volume prevents admission or adoption. You
   cannot add or change a mount later.
5. Delete the sandbox, then attach the volume to a new one. The files are
   still there. Set `"read_only": true` on the new mount to deny writes.
6. When the data is no longer needed, delete the detached volume with
   `DELETE /v1/volumes/{volume_id}`. `204` confirms storage removal.
   If the response is lost, the SDK returns a transport error. Repeat the
   deletion explicitly to check: `204` confirms removal and `404` means the
   ID is absent. GET alone cannot confirm cleanup; it only shows ready volumes.

A volume can be attached to just one sandbox, even in read-only mode.
Pause and resume keep the attachment. During daemon recovery, an eligible
idle container keeps its mount; if a sandbox is retired, its volume remains
available after removal. Files preserve numeric ownership, not a mapping
between the user names of different images. Filesystem templates exclude
mounted volume contents.

## What failure looks like

- `503 unavailable`: Check volume policy, state storage, and host mount
  prerequisites. `volumes.v1` indicates configured support, not storage
  health. Execlet retries cleanup of a failed allocation in the background;
  it stays hidden and counts against the limit until cleanup is confirmed.
  Retry a volume deletion if storage cleanup was unconfirmed.
- `429 rate_limited`: Delete a detached volume before creating another.
- `409 volume_in_use`: For a live attachment, delete the sandbox and wait for
  confirmed removal. If creation is still pending, see the recovery case below.
- `409 volume_read_only`: The file API refused a write. A command sees an
  ordinary filesystem error.
- `507 volume_quota_exceeded`: Free space before retrying. A failed single-file
  upload leaves the previous file intact. Multi-entry archives and writes
  inside commands are not transactional.

If Docker loses the outcome of a sandbox create, the volume stays reserved,
even after execletd restarts. An empty Docker lookup can precede a late
container creation. Execlet waits for the matching container and confirmed
removal. If no container ever appears, the hold has no automatic expiry;
ask the host operator to investigate. Do not edit the journal or unmount
the volume to clear the hold.

There is no volume resize, shared mount, cross-host replication, or per-tenant
access control. Treat the state directory as sensitive application data and
use consistent offline backups with the daemon stopped and writers drained.

## Verify on your host

From the source checkout, build the binaries and the small guest image as
your regular user. Run the volume suite on the dedicated Linux host with the
same prerequisites as the daemon. Omit `--runtime` to exercise both registered
runtimes.

```sh
make build runtime-image
sudo env "PATH=$PATH" TMPDIR=/var/tmp python3 scripts/e2e-volumes.py --runtime runc
```

Inspect `artifacts/volumes/report.json` for reuse, isolation, write failures,
restart recovery, and backing-file cleanup.
