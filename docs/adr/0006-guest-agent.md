# Guest control in Docker sandboxes

Accepted for local Docker deployments.

## Delivery and transport

The release build embeds static Linux amd64 and arm64 executables. Build from a checkout with `make build`. The backend inspects the selected image before creation and pins the resulting ID in the create request. After a pull it repeats the inspection. Missing architecture metadata stages the host-architecture binary and keeps the sandbox in unavailable mode. It writes the matching executable to `/.tiny-sandbox/guest` using the archive endpoint before starting the container. The image keeps its normal user and the sandbox's long-running sleep command.

The backend asks Docker to execute an identity probe as the image user, including supplementary groups, working directory and HOME. A detached root exec then starts the guest daemon. A persistent root Docker exec relays framed requests over a root-owned Unix socket. The hello response confirms the protocol version and readiness. Each frame contains a type byte, a four-byte big-endian payload length and up to 1 MiB of JSON. Every new relay negotiates the guest version before sending application requests. External Docker pauses reject guest requests with invalid_state before the host writes to the retained channel. The public file API resumes a sandbox previously paused through sandboxd. The retained relay also carries cancellation when the container has reached its process limit. The non-TTY Docker stream is demultiplexed before decoding application frames.

The existing Docker client already negotiates API versions and handles exec status, archives, image pulls and cgroups. Adding detached exec and HTTP Upgrade keeps a small dependency surface and avoids a host socket mount. The guest build uses the Linux PTY operations from creack/pty and x/sys. The guest and host share their frame definition.

## Permissions and workloads

In auto mode the private control tmpfs starts with mode 0700. The root daemon owns the mode-0600 socket. Workload children adopt the image UID, GID and supplementary groups before changing directory or resolving the executable. With a non-root image, the workload cannot inspect the socket, alter the control directory or signal the supervisor. Root workloads retain their image privileges and set `protected` to false. These guarantees apply to guest workloads inside the container. The daemon API itself controls the sandbox and requires trusted callers.

The guest uses lstat for file metadata and reads directory names with a bounded scan. A large directory page can span several individually bounded list frames. It owns managed process sessions and pins the leader until other session members exit. Kill stops live members, checks their stopped states, sends SIGKILL and waits for terminal state. PTYs retain a bounded replay window with byte offsets. Only the running host daemon uses guest execution records. Interrupted commands discovered during host recovery stay unknown and cause removal of their container.

## Fallback and measurement

`-guest-agent=auto` reports `agent_mode: guest` after negotiation. An unsupported architecture, a missing payload or a failed startup reports `unavailable` and logs the cause. `-guest-agent=off` reports `legacy`. Both fallback modes use the previous metadata and Python supervisor paths. Filtered networking and terminals require a working guest. A running command whose guest disappears is retained as unknown with truncated output, and its sandbox is retired.

Run `python3 scripts/e2e-guest.py` after `make build`. The report records single-entry stat, paginated lists and individual creation times for both modes, including a stripped non-root Alpine image. Run `python3 scripts/e2e-guest-faults.py` for control-directory permissions, root behavior, managed cancellation under the PID limit and guest loss under memory pressure.

For the operations baseline with the deployment policy enabled, use the pinned image ID from `docker image inspect tiny-sandbox-guest:fixture --format '{{.Id}}'` with `scripts/e2e-operations.py --server bin/sandboxd --image IMAGE_ID --out OUTPUT --samples 30 --creation-only --guest-agent MODE`. Run once with `off` and once with `auto`, using a new output directory each time.

Run `python3 scripts/e2e-guest-boundaries.py` to compare large metadata pages, inspect guest ELF selection on AMD64 and ARM64, and exercise external and public pauses. Run `python3 scripts/guest-transport-probe.py` to inject a lost command-start acknowledgement, an oversized frame, missing image architecture and a version mismatch into real Docker traffic. Each script writes a JSON report and checks cleanup.
