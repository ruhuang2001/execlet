// Package dockerbackend implements daemon.Backend on top of the Docker Engine
// API. It knows only internal/dockerapi; nothing in it knows about HTTP or the
// sandbox state machine.
//
// One container runs per sandbox. The container's init process is
// `sleep infinity`; every execution is a Docker exec inside it. Two details
// are load-bearing:
//
//   - An exec starts in its own process group, so signalling the group ends
//     grandchildren too. The exec's PID is only visible inside the container,
//     so the command is wrapped: a tiny shell writes $$ to a tmpfs file and
//     execs the real command, keeping the PID. Signal reads that file and
//     kills the group from inside the container.
//   - Exec streams must outlive the request that started them. The engine
//     bounds the start handshake separately; after it succeeds, output
//     observation is detached until the command's stream closes.
package dockerbackend

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
	"github.com/ruhuang2001/tiny-sandbox/internal/dockerapi"
)

// PidDir is a tmpfs mounted in every sandbox container. PID files live there
// so signalling never touches the container's writable layer and cannot
// collide with files the user writes.
const PidDir = "/run/tiny-sandbox"

// initCommand keeps the container alive between executions. Every image the
// backend supports has sh and sleep; `sleep infinity` needs no shell.
//
// It is not PID 1: HostConfig.Init puts Docker's init process first, because
// PID 1 is what reaps orphaned grandchildren and `sleep infinity` never calls
// wait. Under a pids limit an unreaped zombie is a permanently lost task slot.
var initCommand = []string{"sleep", "infinity"}

// DefaultPidsLimit bounds processes and threads per sandbox when the spec does
// not set one. The value is counted by the pids cgroup, which counts tasks, not
// processes: a JVM or a Go build needs hundreds of threads. Memory limits do
// not constrain process count, so without this a fork bomb runs until the
// host's PID table is empty and every other sandbox (and the daemon) stalls.
const DefaultPidsLimit = 1024

// sandboxCapDrop removes the capabilities no sandbox needs: NET_RAW stops raw
// packet crafting and ICMP-level spoofing, MKNOD stops creating device nodes
// that could reach host devices. The rest of Docker's default set is kept
// because package managers need it: apk, pip and apt were verified to install
// with these defaults plus no-new-privileges. Dropping ALL also works for
// those three, but it removes SETUID/SETGID and breaks images that expect a
// setuid helper such as ping or sudo, so it is a separate decision.
var sandboxCapDrop = []string{"NET_RAW", "MKNOD"}

// sandboxSecurityOpt applies no-new-privileges: a setuid binary in the image
// stays useless, so `sudo` and file capabilities cannot raise privileges even
// inside the container.
var sandboxSecurityOpt = []string{"no-new-privileges"}

// Backend is a daemon.Backend over a Docker daemon.
type Backend struct {
	api        *dockerapi.Client
	cgroupRoot string // host fast path only for a Linux Unix-socket backend

	mu      sync.Mutex
	seq     uint64
	cleanup cleanupQueue
}

var _ daemon.Backend = (*Backend)(nil)

// New returns a backend that sends requests through api. The client must have
// negotiated its API version, which Dial does.
func New(api *dockerapi.Client) *Backend { return &Backend{api: api} }

// Dial connects to the Docker socket at socketPath and returns a backend.
func Dial(ctx context.Context, socketPath string) (*Backend, error) {
	api, err := dockerapi.Dial(ctx, socketPath)
	if err != nil {
		return nil, err
	}
	b := New(api)
	if runtime.GOOS == "linux" {
		b.cgroupRoot = "/sys/fs/cgroup"
	}
	return b, nil
}

// Ping reports whether Docker answers its health probe.
func (b *Backend) Ping(ctx context.Context) error { return b.api.Ping(ctx) }

// ---------------------------------------------------------------- containers

// Create creates and starts the sandbox container. It returns only once the
// container is running, as the Backend contract requires.
func (b *Backend) Create(ctx context.Context, spec daemon.ContainerSpec) (string, error) {
	limit := int64(spec.MemoryMB) << 20
	pidsLimit := spec.PidsLimit
	if pidsLimit <= 0 {
		pidsLimit = DefaultPidsLimit
	}
	pids := int64(pidsLimit)
	initProcess := true
	cfg := dockerapi.ContainerConfig{
		Image:  spec.Image,
		Cmd:    initCommand,
		Labels: spec.Labels,
		HostConfig: dockerapi.HostConfig{
			Memory:      limit,
			MemorySwap:  limit, // no swap: the limit the user asked for is the limit
			NanoCpus:    spec.NanoCPUs,
			PidsLimit:   &pids,
			CapDrop:     sandboxCapDrop,
			SecurityOpt: sandboxSecurityOpt,
			NetworkMode: networkMode(spec.Network),
			Tmpfs:       map[string]string{PidDir: "rw,size=1m,mode=1777"},
			Init:        &initProcess,
		},
	}

	id, err := b.api.ContainerCreate(ctx, spec.Name, cfg)
	if err != nil && dockerapi.IsNoSuchImage(err) {
		// The Engine API's create does not pull, unlike `docker run`. A clean
		// host therefore fails every create until the image is fetched, which
		// is exactly the difference between a developer machine and CI.
		if pullErr := b.api.PullImage(ctx, spec.Image); pullErr != nil {
			return "", fmt.Errorf("dockerbackend: pull %s: %w", spec.Image, pullErr)
		}
		id, err = b.api.ContainerCreate(ctx, spec.Name, cfg)
	}
	if err != nil && dockerapi.IsConflict(err) && spec.Name != "" {
		// A leftover container still owns the name. The sandbox ID is unique,
		// so this is a restart artefact; remove it and retry once.
		if rmErr := b.api.ContainerRemove(ctx, spec.Name); rmErr == nil {
			id, err = b.api.ContainerCreate(ctx, spec.Name, cfg)
		}
	}
	if err != nil {
		return "", fmt.Errorf("dockerbackend: create %s: %w", spec.Image, err)
	}

	if err := b.api.ContainerStart(ctx, id); err != nil {
		// A container that exists but never started would leak until the next
		// startup cleanup; remove it now.
		_ = b.api.ContainerRemove(context.WithoutCancel(ctx), id)
		return "", fmt.Errorf("dockerbackend: start container: %w", err)
	}
	return id, nil
}

// Remove force-removes the container and everything running in it.
func (b *Backend) Remove(ctx context.Context, containerID string) error {
	err := b.api.ContainerRemove(ctx, containerID)
	if err == nil || dockerapi.IsNotFound(err) {
		b.cleanup.forget(containerID)
	}
	return notFound(err, "container %s", containerID)
}

// Pause freezes the container.
func (b *Backend) Pause(ctx context.Context, containerID string) error {
	err := b.api.ContainerPause(ctx, containerID)
	return notFound(err, "container %s", containerID)
}

// Unpause resumes the container. Docker answers 204 when it is already
// running, which matches the contract's idempotent resume.
func (b *Backend) Unpause(ctx context.Context, containerID string) error {
	err := b.api.ContainerUnpause(ctx, containerID)
	return notFound(err, "container %s", containerID)
}

// Liveness reports the exact container's state.
//
// It uses the container list, not inspect. An earlier build lost this race:
// inspect intermittently answered "no such object" for a container that was
// still there. The list with all=1 distinguishes "exists and stopped" from
// "does not exist" without that window, and the ID filter is compared exactly
// because Docker's id filter matches a prefix.
func (b *Backend) Liveness(ctx context.Context, containerID string) (daemon.Liveness, error) {
	list, err := b.api.ContainerList(ctx, true, map[string][]string{"id": {containerID}})
	if err != nil {
		return daemon.LivenessMissing, fmt.Errorf("dockerbackend: list containers: %w", err)
	}
	for _, c := range list {
		if c.ID != containerID {
			continue
		}
		switch c.State {
		case "running":
			return daemon.LivenessRunning, nil
		case "paused":
			return daemon.LivenessPaused, nil
		default:
			return daemon.LivenessStopped, nil
		}
	}
	return daemon.LivenessMissing, nil
}

// RemoveOwned removes every container carrying labelKey=owner and returns how
// many it removed. The server runs it at startup so a restart does not leak
// containers from the previous process.
func (b *Backend) RemoveOwned(ctx context.Context, labelKey, owner string) (int, error) {
	filters := map[string][]string{"label": {labelKey + "=" + owner}}
	list, err := b.api.ContainerList(ctx, true, filters)
	if err != nil {
		return 0, fmt.Errorf("dockerbackend: list owned containers: %w", err)
	}
	removed := 0
	var errs []error
	for _, c := range list {
		if err := b.Remove(ctx, c.ID); err != nil {
			errs = append(errs, err)
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}

// Owned returns exact container identities for startup reconciliation.
func (b *Backend) Owned(ctx context.Context, owner string) ([]string, error) {
	list, err := b.api.ContainerList(ctx, true, map[string][]string{"label": {daemon.OwnerLabelKey + "=" + owner}})
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, container := range list {
		if container.Labels[daemon.OwnerLabelKey] == owner {
			ids = append(ids, container.ID)
		}
	}
	return ids, nil
}

// OwnedLiveness returns container states for every container owned by owner in
// a single Engine API list call.
func (b *Backend) OwnedLiveness(ctx context.Context, owner string) (map[string]daemon.Liveness, error) {
	list, err := b.api.ContainerList(ctx, true, map[string][]string{"label": {daemon.OwnerLabelKey + "=" + owner}})
	if err != nil {
		return nil, fmt.Errorf("dockerbackend: list owned liveness: %w", err)
	}
	states := make(map[string]daemon.Liveness, len(list))
	for _, c := range list {
		if c.Labels[daemon.OwnerLabelKey] != owner {
			continue
		}
		switch c.State {
		case "running":
			states[c.ID] = daemon.LivenessRunning
		case "paused":
			states[c.ID] = daemon.LivenessPaused
		default:
			states[c.ID] = daemon.LivenessStopped
		}
	}
	return states, nil
}

// ---------------------------------------------------------------- executions

// Exec starts spec.Command inside the container. The command is wrapped in a
// shell that records its PID, changes directory, and then execs the real argv,
// so the PID file names the process group leader and the command itself still
// runs without a shell.
func (b *Backend) Exec(ctx context.Context, containerID string, spec daemon.ExecSpec) (daemon.Process, error) {
	if len(spec.Command) == 0 {
		return nil, errors.New("dockerbackend: empty command")
	}
	if spec.TimeoutMS != nil && spec.Stdin != "" {
		return nil, errors.New("managed commands do not support stdin")
	}
	pidPath := b.nextPidPath()
	stdinDir, err := b.stageStdin(ctx, containerID, spec.Stdin)
	if err != nil {
		return nil, err
	}
	wrapped := wrapCommand(pidPath, spec.Dir, spec.Env, spec.Command)
	execEnv := spec.Env
	controlToken := ""
	if spec.TimeoutMS != nil {
		controlToken = strings.TrimPrefix(b.nextPidPath(), PidDir+"/")
		wrapped, stdinDir, err = b.managedCommand(ctx, containerID, pidPath, controlToken, spec)
		if err != nil {
			return nil, err
		}
		execEnv = nil // User environment belongs to the child, not its supervisor.
	}
	if stdinDir != "" && controlToken == "" {
		wrapped = withStdin(wrapped, stdinDir)
	}

	execID, err := b.api.ExecCreate(ctx, containerID, dockerapi.ExecConfig{
		Cmd: wrapped,
		Env: execEnv,
	})
	if err != nil {
		b.cleanupStdin(containerID, stdinDir)
		return nil, fmt.Errorf("dockerbackend: exec create: %w", err)
	}
	// Only the handshake inherits the start budget. Once headers arrive, the
	// observer owns streamCtx; canceling the initiating request cannot cut off
	// later output. A cancellation already in progress wins this handoff.
	streamCtx, stopStream := context.WithCancel(context.WithoutCancel(ctx))
	stopHandshake := context.AfterFunc(ctx, stopStream)
	stream, err := b.api.ExecStart(streamCtx, execID)
	if !stopHandshake() {
		stopStream()
		if stream != nil {
			_ = stream.Close()
		}
		err = ctx.Err()
	}
	if err != nil {
		stopStream()
		// The wrapper may still start. Leave its files to container removal
		// (ErrStartUnconfirmed retires the sandbox), not a racing rm.
		return nil, fmt.Errorf("dockerbackend: exec start: %w", errors.Join(daemon.ErrStartUnconfirmed, err))
	}

	outW := newBufferedStream()
	errW := newBufferedStream()
	p := &process{
		b:            b,
		execFiles:    &execFiles{containerID: containerID, execID: execID, pidPath: pidPath, stdinDir: stdinDir, managed: controlToken != ""},
		controlToken: controlToken,
		stdout:       outW,
		stderr:       errW,
		done:         make(chan struct{}),
	}
	go func() {
		defer close(p.done)
		defer stopStream()
		err := dockerapi.Demux(stream, outW, errW)
		_ = stream.Close()
		_ = outW.Close()
		_ = errW.Close()
		p.mu.Lock()
		p.demuxErr = err
		p.mu.Unlock()
	}()
	if controlToken != "" {
		if err := p.readManagedPID(ctx); err != nil {
			// ExecStart was accepted. Keep its observer and original supervisor
			// deadline alive; the caller retains the invocation but cannot
			// advertise a handle whose guest identity was never confirmed.
			return p, err
		}
	}
	return p, nil
}

// OOMKillCount prefers the exact container's local v2 counter. If unavailable,
// read from inside the container (v2 or v1). If even exec fails, Docker's
// OOMKilled flag is the last remaining evidence.
func (b *Backend) OOMKillCount(ctx context.Context, containerID string) (int64, error) {
	if n, ok := localOOMKillCount(b.cgroupRoot, containerID); ok {
		return n, nil
	}
	out, err := b.captureControl(ctx, containerID, []string{"/bin/sh", "-c", dockerapi.OOMScript})
	if err == nil {
		return dockerapi.ParseOOMKill(out)
	}

	inspect, inspectErr := b.api.ContainerInspect(ctx, containerID)
	if inspectErr != nil || !inspect.State.OOMKilled {
		return 0, fmt.Errorf("dockerbackend: read oom_kill: %w", err)
	}
	return 1, nil
}

// ---------------------------------------------------------------- files

// ReadFile reads a file through the archive API. Docker tar-wraps the bytes,
// so the single entry is unwrapped here.
func (b *Backend) ReadFile(ctx context.Context, containerID, path string) ([]byte, error) {
	rc, err := b.api.ArchiveGet(ctx, containerID, path)
	if err != nil {
		return nil, notFound(err, "%s", path)
	}
	defer func() { _ = rc.Close() }()
	data, err := dockerapi.ExtractFile(rc)
	if err != nil {
		return nil, fmt.Errorf("dockerbackend: read %s: %w", path, err)
	}
	return data, nil
}

// WriteFile writes a file through the archive API. The archive is extracted at
// "/" with the full relative path as its entry name, which makes Docker create
// every missing parent directory itself.
//
// The archive carries the guest's default uid and gid on the file and on every
// parent directory it creates. Docker extracts as root, so without this a file
// written into a fresh directory is root-owned and a non-root guest cannot then
// write beside it — the common shape of an upload followed by a task that
// produces output in the same directory.
func (b *Backend) WriteFile(ctx context.Context, containerID, path string, data []byte) error {
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("dockerbackend: path %q must be absolute", path)
	}
	name := strings.TrimPrefix(path, "/")
	uid, gid, dirs := b.writeOwnership(ctx, containerID, name)
	tarBody, err := dockerapi.BuildOwnedFileTar(name, data, uid, gid, dirs)
	if err != nil {
		return err
	}
	if err := b.api.ArchivePut(ctx, containerID, "/", tarBody); err != nil {
		return notFound(err, "%s", path)
	}
	// Docker answers 200 for an extraction it could not perform: a write under
	// /proc is accepted and no file appears. The caller asked for the file, so
	// the file is what confirms the write; a request that changed nothing must
	// not read as success.
	out, err := b.captureAs(ctx, containerID, "0", []string{"/bin/sh", "-c", writeVerifyScript, "write-verify", path})
	if err != nil {
		return &daemon.Error{Code: daemon.CodeBackendError, Message: "cannot confirm the write to " + path, Err: err}
	}
	if string(out) != "ok" {
		return &daemon.Error{Code: daemon.CodeInvalidRequest, Message: "destination cannot hold a file: " + path}
	}
	return nil
}

// Fixed shell program; the target path is a positional argument, never code.
const writeVerifyScript = `if [ -f "$1" ]; then printf ok; elif [ -e "$1" ]; then printf notfile; else printf missing; fi`

// writeOwnership reports the ownership a write into name must carry, and the
// ancestors that do not exist yet, shallowest first.
//
// One exec runs as the container's default user, so the ids are that user's
// without naming it. It does not require `id`: the shell reads the ids from
// /proc/self/status using builtins. When the probe cannot run, including when
// /bin/sh is unavailable, the write proceeds as root — the
// behaviour before this existed — rather than failing an upload that worked.
//
// Only missing ancestors are listed. An existing directory keeps its own owner
// and mode: /home/user must stay the guest's, and /tmp must stay writable by
// everyone.
func (b *Backend) writeOwnership(ctx context.Context, containerID, name string) (uid, gid int, dirs []string) {
	out, err := b.capture(ctx, containerID, []string{"/bin/sh", "-c", writeOwnershipScript, "write-owner", name})
	if err != nil {
		return 0, 0, nil
	}
	for line := range strings.SplitSeq(string(out), "\x00") {
		switch {
		case line == "":
		case strings.HasPrefix(line, "uid="):
			uid, _ = strconv.Atoi(strings.TrimPrefix(line, "uid="))
		case strings.HasPrefix(line, "gid="):
			gid, _ = strconv.Atoi(strings.TrimPrefix(line, "gid="))
		case strings.HasPrefix(line, "dir="):
			dirs = append(dirs, strings.TrimPrefix(line, "dir="))
		}
	}
	// A shallowest-first order is what extracting the archive needs: a child
	// entry under a directory Docker has not created yet would fail.
	slices.Reverse(dirs)
	return uid, gid, dirs
}

// Fixed shell program; the target name is a positional argument, never code.
// It prints the default user's ids, then one NUL-delimited dir= entry per
// missing ancestor. Paths stay intact even when they contain newlines.
const writeOwnershipScript = `set -f
u=0; g=0
while read -r key value rest; do
 case "$key" in
  Uid:) u=$value;;
  Gid:) g=$value;;
 esac
done < /proc/self/status
printf 'uid=%s\0gid=%s\0' "$u" "$g"
parent="/$1"
parent=${parent%/*}
while [ -n "$parent" ]; do
 [ -e "$parent" ] && break
 printf 'dir=%s\0' "${parent#/}"
 parent=${parent%/*}
done
`

// ---------------------------------------------------------------- helpers

// capture runs an internal command and returns its stdout.
func (b *Backend) capture(ctx context.Context, containerID string, command []string) ([]byte, error) {
	return b.captureAs(ctx, containerID, "", command)
}

func (b *Backend) captureAs(ctx context.Context, containerID, user string, command []string) ([]byte, error) {
	return b.captureExec(ctx, containerID, user, command, 0)
}

// Control helpers have tiny responses but their binaries and output are
// guest-controlled. Bound the entire framed stream, including stderr.
func (b *Backend) captureControl(ctx context.Context, containerID string, command []string) ([]byte, error) {
	return b.captureExec(ctx, containerID, "", command, 64<<10)
}

var errCaptureLimit = errors.New("dockerbackend: helper output limit exceeded")

func (b *Backend) captureExec(ctx context.Context, containerID, user string, command []string, limit int64) ([]byte, error) {
	execID, err := b.api.ExecCreate(ctx, containerID, dockerapi.ExecConfig{Cmd: command, User: user})
	if err != nil {
		return nil, err
	}
	stream, err := b.api.ExecStart(ctx, execID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = stream.Close() }()

	var reader io.Reader = stream
	bounded := &io.LimitedReader{R: stream, N: limit + 1}
	if limit > 0 {
		reader = bounded
	}
	var stdout, stderr bytes.Buffer
	err = dockerapi.Demux(reader, &stdout, &stderr)
	if limit > 0 && bounded.N == 0 {
		return nil, errCaptureLimit
	}
	if err != nil {
		return nil, err
	}
	inspect, err := b.waitInspect(ctx, execID)
	if err != nil {
		return nil, err
	}
	if limit > 0 && inspect.ExitCode == nil {
		return nil, errors.New("dockerbackend: control helper exit status unconfirmed")
	}
	if inspect.ExitCode != nil && *inspect.ExitCode != 0 {
		return nil, &execExitError{code: *inspect.ExitCode, stderr: strings.TrimSpace(stderr.String())}
	}
	return stdout.Bytes(), nil
}

type execExitError struct {
	code   int
	stderr string
}

func (e *execExitError) Error() string {
	return fmt.Sprintf("internal exec exited %d: %s", e.code, e.stderr)
}

// waitInspect polls until the exec is no longer running and returns its final
// state.
func (b *Backend) waitInspect(ctx context.Context, execID string) (dockerapi.ExecInspect, error) {
	deadline := time.Now().Add(10 * time.Second)
	for step := 0; ; step++ {
		inspect, err := b.api.ExecInspect(ctx, execID)
		if err == nil && inspect.Stopped(execID) {
			return inspect, nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return dockerapi.ExecInspect{}, fmt.Errorf("dockerbackend: inspect exec %s: %w", execID, err)
			}
			return dockerapi.ExecInspect{}, fmt.Errorf("dockerbackend: exec %s still running after 10s", execID)
		}
		select {
		case <-ctx.Done():
			return dockerapi.ExecInspect{}, ctx.Err()
		case <-time.After(pollDelay(step)):
		}
	}
}

func pollDelay(step int) time.Duration {
	delay := time.Millisecond << min(step, 5)
	return min(delay, 20*time.Millisecond)
}

// execWrapper records the process group leader's PID, changes directory, and
// then becomes the command, without a shell around the command itself: $0 is
// the PID file, $1 the directory, $2 the number of environment entries, $3
// whether a command separator follows them, and "$@" after the shift the
// entries, that separator when present, and the argv. No argument is ever
// parsed as shell syntax.
//
// The PID is written first. Signal kills the process group through this file,
// so everything that can fail after it fails where a cancel still reaches the
// process.
//
// The directory is handled here rather than through Docker's exec WorkingDir
// so a bad directory stays an ordinary outcome: exit 126 with our own message
// on stderr. WorkingDir reports 127 on stdout instead, which reads like the
// program's own output and collides with a missing command. Exit 126 is this
// wrapper's own code, so it is distinguishable from a missing command (127).
//
// cd -P resolves the path physically. Plain cd simplifies ".." lexically
// before looking at the filesystem, so for /work/link/.. with link -> /data it
// lands in /work, and for /nope/.. it succeeds instead of failing.
//
// The environment is re-applied at the last step rather than left to the shell
// that starts here. A shell imports only the names it considers valid
// identifiers, so dash drops MY-VAR and CONFIG.KEY and rewrites IFS, PWD and
// SHLVL; BusyBox keeps more. Exec env is what the target must see, and env(1)
// passes every entry through unchanged.
//
// Nothing here assigns a shell variable. An assignment to a name the image
// already exports replaces the value the image set, so a wrapper-local "n" or
// "e" would silently rewrite the image's own n and e. The count is read from
// $2 before the shift and env is probed by path, so no variable is needed.
//
// $3 is layout metadata that this script only shifts away: it records whether
// a separator was inserted, so a reader of the argv does not have to infer it
// from the command.
const execWrapper = `printf %s $$ > "$0" || exit 125
if [ -n "$1" ]; then cd -P -- "$1" 2>/dev/null || { printf 'sandbox: cwd is not a usable directory: %s\n' "$1" >&2; exit 126; }; fi
if [ "$2" -eq 0 ]; then shift 3; exec "$@"; fi
shift 3
if [ -x /usr/bin/env ]; then exec /usr/bin/env -- "$@"; fi
if [ -x /bin/env ]; then exec /bin/env -- "$@"; fi
printf 'sandbox: this image has no env utility, so the requested environment cannot be applied\n' >&2
exit 126`

// commandSeparator keeps a command out of env's assignment parsing, and is used
// only when it is needed.
//
// env(1) reads every leading NAME=VALUE as an assignment and only then starts
// the command; "--" ends option parsing, not assignment parsing. A command
// whose own path contains "=" would therefore be swallowed: env would apply it
// as a variable, run nothing, exit 0, and print the environment, secrets
// included, as if that were the program's output. A command whose path has no
// "=" ends the scan by itself, so the separator is only added in the case that
// needs it.
//
// It is a shell, and a shell resets SHLVL and IFS on startup, so an execution
// that both sets those names and names a command containing "=" sees the
// shell's values instead of the requested ones. That combination is not worth a
// mechanism of its own: every other execution keeps a single shell, which is
// what lets the requested SHLVL and IFS reach the target.
var commandSeparator = []string{"/bin/sh", "-c", `exec "$@"`, "sandbox"}

// needsCommandSeparator reports whether the command would be read as another
// assignment by env(1).
func needsCommandSeparator(command []string) bool {
	return len(command) > 0 && strings.Contains(command[0], "=")
}

// wrapCommand builds the argv an exec runs. Writing the PID before the exec
// matters: the PID of the shell equals the PID of the process group that
// Docker created for the exec, and the exec keeps that PID and group.
func wrapCommand(pidPath, dir string, env, command []string) []string {
	// $3 records whether the separator follows, so the layout is readable from
	// the argv instead of being inferred from the command.
	separated := len(env) > 0 && needsCommandSeparator(command)
	flag := "0"
	if separated {
		flag = "1"
	}

	argv := make([]string, 0, 7+len(commandSeparator)+len(env)+len(command))
	argv = append(argv, "/bin/sh", "-c", execWrapper, pidPath, dir, strconv.Itoa(len(env)), flag)
	argv = append(argv, env...)
	if separated {
		argv = append(argv, commandSeparator...)
	}
	return append(argv, command...)
}

// nextPidPath returns a unique PID-file path. The counter keeps it unique per
// process; the random suffix keeps it unique per execution even if the process
// restarts while a container is reused from cleanup.
func (b *Backend) nextPidPath() string {
	b.mu.Lock()
	b.seq++
	n := b.seq
	b.mu.Unlock()
	var r [6]byte
	if _, err := rand.Read(r[:]); err != nil {
		return fmt.Sprintf("%s/%d-%d", PidDir, n, time.Now().UnixNano())
	}
	return fmt.Sprintf("%s/%d-%s", PidDir, n, hex.EncodeToString(r[:]))
}

// notFound maps a Docker 404 to the daemon's sentinel so the contract answers
// not_found rather than backend_error.
func notFound(err error, format string, args ...any) error {
	if err == nil {
		return nil
	}
	if dockerapi.IsNotFound(err) {
		return fmt.Errorf("dockerbackend: %s: %w", fmt.Sprintf(format, args...), daemon.ErrNotFound)
	}
	return fmt.Errorf("dockerbackend: %s: %w", fmt.Sprintf(format, args...), err)
}

// networkMode maps the daemon's network mode to Docker's. An empty or unknown
// value falls back to bridge, which is the compatibility default: a request
// that does not mention networking must keep working the way it always has.
func networkMode(mode daemon.NetworkMode) string {
	switch mode {
	case daemon.NetworkNone:
		return string(daemon.NetworkNone)
	default:
		return string(daemon.NetworkBridge)
	}
}

// parsePid validates the PID file contents: decimal digits only, so a changed
// file cannot turn into shell syntax.
func parsePid(data []byte) (string, error) {
	s := strings.TrimSpace(string(data))
	if s == "" {
		return "", errors.New("dockerbackend: pid file is empty")
	}
	if _, err := strconv.ParseUint(s, 10, 32); err != nil {
		return "", fmt.Errorf("dockerbackend: pid file %q is not a pid", s)
	}
	return s, nil
}
