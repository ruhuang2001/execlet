# 0002. Docker execution, file access, and the CLI

Status: Accepted

## Engine transport

The backend owns sandbox behavior. The Engine client owns Docker's wire format. The transport connects through a Unix socket and uses an HTTP host only to construct the URL. Dial negotiates the daemon's version before concurrent use. The unversioned ping accepts its caller's deadline. The socket client has no global request timeout because an attached output stream can last as long as the command.

An Engine error retains its status and a bounded message, including a plain-text body when Docker's JSON message is absent. Callers map status codes to service errors. A fake daemon decoding the same Go struct would accept the client's spelling mistakes. Wire-shape assertions therefore check Docker's literal field names and integral resource values independently.

## Container creation

Each sandbox owns a container that stays alive across executions. Engine create reports a missing image on a clean host. The backend pulls on that specific 404 and retries create. Digest references take precedence over tags, and a colon in the registry host and port belongs to the repository. Pulls can report errors inside an HTTP 200 progress stream. The client consumes the stream, accepts ordinary text progress, and fails on a JSON error record or a stream read failure. A failed start triggers removal of the newly created container even after caller cancellation.

On a Docker name conflict, inspect the exact name and verify the immutable sandbox ID and owner labels. Only an exited or dead container with both matching labels is eligible for replacement. Remove by its full Docker ID without force so a concurrent restart or name reuse preserves the active container. Legacy containers without the identity label remain eligible for the existing owner sweep.

Liveness lists containers with stopped entries included. Inspect previously reported a transient missing object for a container that still existed. Docker matches ID filters by prefix, so the backend compares the returned full ID before declaring a match. Owner filters accept key/value pairs and Docker combines label constraints. A sweep finds owned containers even after a restart.

## Container limits

Docker's init process reaps orphaned children while the sandbox's long-lived command sleeps. Unreaped zombies consume cgroup PID slots. The backend supplies a default task limit even when called directly. A task includes each thread, so the default leaves room for a JVM or Go build while bounding host PID consumption. The wire field is a pointer because omission selects the daemon default and zero means unlimited. MemorySwap equals Memory to prevent swap from extending the requested limit.

The capability floor drops NET_RAW and MKNOD to restrict raw packets and device-node creation. Retaining the remaining default capabilities preserves compatibility with package managers and images that depend on UID or GID changes. The no-new-privileges setting prevents privilege gains through setuid binaries and file capabilities. An omitted network setting keeps bridge compatibility.

When deployment policy requires enforcement, the backend first inspects Docker's retained settings, then reads the memory, swap, PID and CPU limits from cgroup v2 in the container. Inspect can echo settings on hosts without the required controllers. Restricted networking requires loopback to be the only interface. Bridge mode makes no claim about external egress.

## Exec lifetime

Docker creates an exec before starting it. The backend waits for the start handshake within the initiating context. Once headers arrive, the observer owns the stream so the originating HTTP request can finish without interrupting later output. Cancellation already in progress wins the handoff. An ambiguous start leaves the files for sandbox retirement because a late-starting wrapper might still need them.

For ordinary execs, the wrapper writes its PID to a random file on a container tmpfs before any directory or environment operation can fail, then replaces itself with the command. Signaling from inside the container targets the process group and its descendants. An in-container read can see the mounted tmpfs, which Docker's archive endpoint may resolve in another mount namespace. A brief retry covers the interval before the PID is published. The sequence number and random suffix distinguish executions across server restarts. Only decimal PID contents reach the BusyBox-compatible signal operand.

The stdout/stderr observer finishes before Wait polls for Docker's recorded exit. Confirmation for file cleanup requires the expected exec ID, an explicit stopped state, and a recorded exit code. Missing fields can also describe an exec that has yet to start.

## Exec wrapper

The ordinary wrapper takes the PID path in shell argument zero, followed by the working directory, environment count, a flag selecting the environment handoff, and the command payload. An empty directory and zero count preserve this layout. Supplying data as positional arguments keeps it out of shell syntax. The PID is written first so cancellation can still find the process when directory setup fails.

The wrapper changes directory with physical path resolution. With a symlink and a following `..`, the parent belongs to the symlink's target. A nonexistent component before `..` must fail. Docker's exec WorkingDir would turn an unusable directory into a startup error with status 127 and a message on stdout, also used by a missing command. The wrapper reports the directory error with exit 126 and a diagnostic on stderr.

The Engine receives requested environment entries in the exec configuration. For ordinary execs the wrapper also supplies them to the target with an absolutely addressed env utility, since the request may replace PATH. Shell startup can drop names such as `MY-VAR` and `CONFIG.KEY` or rewrite IFS, PWD and SHLVL. Keeping wrapper state in positional arguments also protects variables exported by the image, including short names such as `n` and `e`.

The `--` operand ends option parsing in env, which permits variable names starting with a dash. Assignment parsing continues afterward. When argv[0] contains an equals sign and environment entries are present, the wrapper places a native zero-adjustment nice invocation between env and the command. It keeps the original PID, priority, environment, stdin and argv through exec. BusyBox takes the operand directly after its adjustment. Coreutils accepts an explicit end-of-options marker for command names starting with a dash.

The special payload supplies the entry count, the entries in reverse order, a literal command marker and the original command. The shell prepends each entry to the selected launcher without turning user data into shell source. It restores a preexisting image variable if its name matches the loop variable. On images without a native launcher, the shell can export writable shell variable names directly.

Real-image checks exercise filesystem resolution, replacement PATH, arbitrary accepted names and assignments in command paths. The image-variable check exports names from an outer launcher before entering the wrapper and checks the returned environment contains the expected entries. Empty output or a shifted test argv would otherwise allow a false pass.

## Output streams

Docker multiplexes stdout and stderr on one non-TTY connection. Each eight-byte header contains a stream byte, three padding bytes and a big-endian unsigned length. Each payload streams through a bounded copy. A short header or payload is an error, even when some bytes have reached the destination. An EOF between frames is clean. Unrecognized stream types and stdin frames are consumed to keep framing aligned. The fuzz oracle independently decodes the frames and checks partial output and error behavior, including lengths up to the wire maximum.

A single goroutine demultiplexes both outputs. An unbuffered pipe for stdout could prevent a caller waiting on stderr from receiving already-available data. Each output has a bounded buffer larger than the daemon's event chunks, and draining it wakes writers. Readers can consume buffered bytes after Close. A closed writer returns its completed byte count and an error.

## Managed commands

Managed commands use a Linux Python 3 supervisor with a separate child session. Command, cwd and environment are staged through a private archive file so large payloads avoid Linux's per-argument limit. The supervisor removes the input before spawning and applies requested environment values only to the child. Failure to confirm a managed identity after an accepted start leaves the observer and supervisor deadline active. A read requested just before PID publication can return after process exit, so the backend retries after the stream-completion barrier.

The supervisor holds the process group leader unreaped while the group can receive signals. WNOWAIT observes an exit while keeping its number reserved, including while descendants remain. A private, authenticated control channel belongs to that invocation. For a kill, the supervisor stops live group members, confirms the stop, then sends KILL. A group consisting entirely of zombies cannot confirm delivery. This closes the race between a natural exit and signal delivery. The terminal record is published before the socket closes, allowing a late client to distinguish a natural exit from an unreachable supervisor. Only after the listener closes can the leader be reaped. A dispatch-boundary test models reuse of the same numeric identity, while installed Linux acceptance exercises actual process-group control.

## Cleanup

Stdin goes into a randomly named directory created by the image user with mode 0700. The ordinary wrapper opens the file as fd 0, then unlinks the file and removes the directory. The bytes stay out of argv and the execution record. A staging failure, including a lost mkdir response, can reclaim its unique directory before a command owns it. An uncertain ExecStart must defer cleanup.

Cleanup and signaling synchronize on the execution's file identities. Retries keep only identities and lock state so output buffers can be released. Recursive removal applies only to a validated staging directory, with no trailing slash. PID paths are validated single components and removed nonrecursively because the guest may replace them with directories. The cleanup helper receives paths as operands and runs as the image user.

A single lazy worker rotates outstanding entries FIFO with bounded calls, rate, and batch size. Retaining in-flight entries lets container removal retire them without a retry re-enqueuing them. A confirmed exit is latched, so a failed file removal can be retried even if Docker later forgets the exec. Before exit is confirmed, the worker rechecks a transient 404. If Docker has forgotten the exec, container removal reclaims its files. An error removing files after confirmation is retired if the container itself has gone.

## OOM evidence

Sample the exact container's cumulative oom_kill counter. On Linux with a local socket, two fixed cgroup v2 layouts are eligible, both keyed by the full lowercase container ID. Each sample rereads the counter. No parent counter, sibling, prefix or guest-controlled path can attribute a kill to this sandbox. This fast path also works while pause prevents an in-container exec. Otherwise the backend probes memory.events on v2 or memory.oom_control on v1 in the guest. Exact key matching separates oom_kill from oom and oom_group_kill. If the exec probe fails, Docker's OOMKilled flag supplies fallback evidence for an init process killed by the kernel.

Exit 137 alone is ambiguous. A real-Docker OOM check allocates anonymous memory above the container's limit because page cache could be reclaimed without a kill. External loss of the container without recorded intent or counter movement is eviction. With concurrent executions an ambiguous kill can yield an unknown outcome.

## Archives

The single-file upload uses a relative tar entry under the container root. Docker extracts as root. Without explicit ownership, a guest could read an upload but fail to create output alongside it. The backend probes the default user's UID and GID through /proc/self/status using shell builtins. It emits missing parents as NUL-delimited names to preserve newlines and inserts entries shallowest first. Existing directories retain their ownership and permissions. If the ownership probe fails, the upload falls back to root ownership. The backend independently verifies that a regular file exists afterward because Docker can acknowledge an ineffective extraction, including under /proc.

Single-file reads inspect the first tar header and accept only a bounded regular entry. The tar reader bounds the subsequent read to its checked size. Link targets and children of a directory cannot substitute for the requested file. Whole-archive reads cap encoded bytes before daemon validation. Uploads preflight destination components and use fixed shell helpers with names as positional arguments. Control responses bound the entire framed stream, including guest-controlled stderr.

Directory listing reads immediate child metadata as the image user. Missing traversal permission can make an existing directory appear absent. Those cases, and images without compatible stat/readlink tools, use a bounded archive scan. The CLI downloads to a temporary file in the destination directory and replaces the destination only after a complete save. Local upload opens the source nonblocking and rechecks the opened file's type, because a FIFO can appear between a path check and open.

## Daemon lifetime

The default owner derives from the listen address. Pin the owner when changing the port so existing containers can still be identified. In-memory startup removes owned orphans. Durable startup reconciles the journal before accepting requests. During shutdown, event handlers get an explicit signal because http.Server.Shutdown waits for them without canceling their request contexts. Main waits for both the HTTP drain and container handling after ListenAndServe returns. Persistent shutdown can preserve idle containers while retiring live executions. Docker errors logged on startup use status or failure categories so runtime payloads and private image names stay out of the logs. An empty listen host binds every interface.

## CLI contract

Global flags precede the command. Run accepts an argv after `--` or a bare command. Repeatable environment options preserve separate entries. Flag presence is tracked separately from its numeric value, because an explicit zero disables a timeout and a negative value is invalid, including the old sentinel value of -1. An explicitly requested zero PID limit and an unsupported network mode are usage errors. Creation options combined with an existing sandbox are rejected. The Python code shorthand supplies an image only for a new sandbox without an explicit image.

Long-running commands observe cancellation. Attach streams from the beginning and Ctrl+C requests remote cancellation through the SDK. Short information requests use the default process signal handling. Canceled starts request cleanup of newly created sandboxes. A killed sandbox can remain visible as a terminal record. Command stdout and stderr use distinct CLI streams. The SDK supplies error classifications, and the CLI drops the duplicate SDK prefix in diagnostics. Ordinary nonzero exits 1 through 125 propagate. Launch failures and other failed outcomes map to 1, while cancellation maps to 130. Inspect prints labeled fields. The CLI harness invokes the real handler over the in-memory backend and captures exit codes and both output streams.

## Doctor

Health is the CLI's direct HTTP call. Other service operations use the SDK so its start recovery applies. A lookup for a fixed probe sandbox ID treats an authorized 404 as success and a 401 as an authentication failure. Docker is a host dependency of a local server, so the CLI probes it only for local endpoints. The probe allows 30 seconds for slow shared CI hosts. It reports a missing binary, timeout, or the first error line from Docker on a single check row. Success takes precedence if the deadline expires just after the process exits.

## Example integrations

The agent example delivers a final JSON tool result with a machine-readable outcome, retained output, exit code when available, and truncation indicator. A consumer can distinguish ordinary failure, OOM, timeout, and an uncertain outcome without interpreting a diagnostic. A fresh sandbox belongs to the tool invocation, so it requests cleanup with an independent deadline even after cancellation. Reusing a caller-owned sandbox preserves files across Python executions. Both the local demo and a configured server use the same SDK path, streaming recovery and error mapping. Unsetting SANDBOX_DOMAIN selects the in-process HTTP handler and a fixed simulated Python response.

The E2B example is a local compatibility profile. Pin the built image ID since a pinned base still permits apt updates at build time. The package acceptance workflow records the installed packages. Authenticated servers require SANDBOX_API_KEY in the example environment.

## Engine test boundaries

The fake Engine listens over a real Unix socket and negotiates an API version. Short socket paths fit macOS's roughly 104-byte Unix address limit even on systems with deeply nested test directories. The fake models image pulls, name resolution, prefix filters and a retained conflicting name for create retries. Real Docker tests give each test a unique owner label and verify the owner's container list is empty after cleanup. The pause check measures a running counter and allows an in-flight write to settle before comparing its frozen value. Run the real suite with `go test -tags docker ./internal/dockerbackend -run TestReal -v`.
