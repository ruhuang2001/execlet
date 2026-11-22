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

const PidDir = "/run/tiny-sandbox"

var initCommand = []string{"sleep", "infinity"}

const DefaultPidsLimit = 1024

var sandboxCapDrop = []string{"NET_RAW", "MKNOD"}

var sandboxSecurityOpt = []string{"no-new-privileges"}

type Backend struct {
	api        *dockerapi.Client
	cgroupRoot string

	mu      sync.Mutex
	seq     uint64
	cleanup cleanupQueue
}

var _ daemon.Backend = (*Backend)(nil)

func New(api *dockerapi.Client) *Backend { return &Backend{api: api} }

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

func (b *Backend) Ping(ctx context.Context) error { return b.api.Ping(ctx) }

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
			MemorySwap:  limit,
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
		if pullErr := b.api.PullImage(ctx, spec.Image); pullErr != nil {
			return "", fmt.Errorf("dockerbackend: pull %s: %w", spec.Image, pullErr)
		}
		id, err = b.api.ContainerCreate(ctx, spec.Name, cfg)
	}
	if err != nil && dockerapi.IsConflict(err) && spec.Name != "" {
		if rmErr := b.api.ContainerRemove(ctx, spec.Name); rmErr == nil {
			id, err = b.api.ContainerCreate(ctx, spec.Name, cfg)
		}
	}
	if err != nil {
		return "", fmt.Errorf("dockerbackend: create %s: %w", spec.Image, err)
	}

	if err := b.api.ContainerStart(ctx, id); err != nil {
		_ = b.api.ContainerRemove(context.WithoutCancel(ctx), id)
		return "", fmt.Errorf("dockerbackend: start container: %w", err)
	}
	return id, nil
}

func (b *Backend) Remove(ctx context.Context, containerID string) error {
	err := b.api.ContainerRemove(ctx, containerID)
	if err == nil || dockerapi.IsNotFound(err) {
		b.cleanup.forget(containerID)
	}
	return notFound(err, "container %s", containerID)
}

func (b *Backend) Pause(ctx context.Context, containerID string) error {
	err := b.api.ContainerPause(ctx, containerID)
	return notFound(err, "container %s", containerID)
}

func (b *Backend) Unpause(ctx context.Context, containerID string) error {
	err := b.api.ContainerUnpause(ctx, containerID)
	return notFound(err, "container %s", containerID)
}

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
		execEnv = nil
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
			return p, err
		}
	}
	return p, nil
}

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

func (b *Backend) ReadFile(ctx context.Context, containerID, path string) ([]byte, error) {
	rc, err := b.api.ArchiveGet(ctx, containerID, path)
	if err != nil {
		return nil, notFound(err, "%s", path)
	}
	defer func() { _ = rc.Close() }()
	data, err := dockerapi.ExtractFile(io.LimitReader(rc, daemon.MaxFileScanBytes+1))
	switch {
	case errors.Is(err, dockerapi.ErrNotRegular):
		return nil, &daemon.Error{Code: daemon.CodeInvalidRequest, Message: "path must identify a regular file: " + path}
	case errors.Is(err, dockerapi.ErrFileTooLarge):
		return nil, &daemon.Error{Code: daemon.CodePayloadTooLarge, Message: fmt.Sprintf("file exceeds %d bytes", daemon.MaxFileBytes)}
	case err != nil:
		return nil, fmt.Errorf("dockerbackend: read %s: %w", path, err)
	}
	return data, nil
}

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
	out, err := b.captureAs(ctx, containerID, "0", []string{"/bin/sh", "-c", writeVerifyScript, "write-verify", path})
	if err != nil {
		return &daemon.Error{Code: daemon.CodeBackendError, Message: "cannot confirm the write to " + path, Err: err}
	}
	if string(out) != "ok" {
		return &daemon.Error{Code: daemon.CodeInvalidRequest, Message: "destination cannot hold a file: " + path}
	}
	return nil
}

const writeVerifyScript = `if [ -f "$1" ]; then printf ok; elif [ -e "$1" ]; then printf notfile; else printf missing; fi`

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
	slices.Reverse(dirs)
	return uid, gid, dirs
}

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

func (b *Backend) capture(ctx context.Context, containerID string, command []string) ([]byte, error) {
	return b.captureAs(ctx, containerID, "", command)
}

func (b *Backend) captureAs(ctx context.Context, containerID, user string, command []string) ([]byte, error) {
	return b.captureExec(ctx, containerID, user, command, 0)
}

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

const execWrapper = `printf %s $$ > "$0" || exit 125
if [ -n "$1" ]; then cd -P -- "$1" 2>/dev/null || { printf 'sandbox: cwd is not a usable directory: %s\n' "$1" >&2; exit 126; }; fi
if [ "$2" -eq 0 ]; then shift 3; exec "$@"; fi
shift 3
if [ -x /usr/bin/env ]; then exec /usr/bin/env -- "$@"; fi
if [ -x /bin/env ]; then exec /bin/env -- "$@"; fi
printf 'sandbox: this image has no env utility, so the requested environment cannot be applied\n' >&2
exit 126`

var commandSeparator = []string{"/bin/sh", "-c", `exec "$@"`, "sandbox"}

func needsCommandSeparator(command []string) bool {
	return len(command) > 0 && strings.Contains(command[0], "=")
}

func wrapCommand(pidPath, dir string, env, command []string) []string {
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

func notFound(err error, format string, args ...any) error {
	if err == nil {
		return nil
	}
	if dockerapi.IsNotFound(err) {
		return fmt.Errorf("dockerbackend: %s: %w", fmt.Sprintf(format, args...), daemon.ErrNotFound)
	}
	return fmt.Errorf("dockerbackend: %s: %w", fmt.Sprintf(format, args...), err)
}

func networkMode(mode daemon.NetworkMode) string {
	switch mode {
	case daemon.NetworkNone:
		return string(daemon.NetworkNone)
	default:
		return string(daemon.NetworkBridge)
	}
}

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
