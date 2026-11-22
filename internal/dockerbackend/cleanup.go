package dockerbackend

import (
	"container/list"
	"context"
	"strings"
	"sync"
	"time"

	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
	"github.com/ruhuang2001/tiny-sandbox/internal/dockerapi"
)

// The retry queue needs only these identities and the lock shared with Signal.
// Never retain a process (in particular, its output buffers) for a retry.
type execFiles struct {
	containerID, execID string
	pidPath, stdinDir   string
	managed             bool
	signalMu            sync.Mutex
	exited              bool // latched: subsequent rm failures need no new exit proof
	cleaned             bool
	missingRetryAt      time.Time
}

// Never apply recursive rm to a PID path: a guest can replace it with a
// directory. Only the exact random stdin directory gets recursive removal,
// with no trailing slash to follow a replacement symlink. The shell receives
// paths as operands and runs as the image user, not root.
const cleanupStagedScript = `status=0; /bin/rm -rf -- "$1" || status=1; shift; /bin/rm -f -- "$@" || status=1; exit "$status"`

func (f *execFiles) remove(ctx context.Context, b *Backend) error {
	cmd := []string{"/bin/rm", "-f", "--"}
	if f.stdinDir != "" && validStdinDir(f.stdinDir) {
		if f.pidPath == "" { // staging failed before the command was started
			cmd = []string{"/bin/rm", "-rf", "--", f.stdinDir}
		} else {
			cmd = []string{"/bin/sh", "-c", cleanupStagedScript, "cleanup", f.stdinDir}
		}
	}
	if f.pidPath != "" {
		name, ok := strings.CutPrefix(f.pidPath, PidDir+"/")
		if !ok || name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
			return nil // never turn a malformed path into an rm operand
		}
		cmd = append(cmd, f.pidPath)
		if f.managed {
			for _, suffix := range []string{".result", ".sock", ".tmp", ".result.tmp"} {
				cmd = append(cmd, f.pidPath+suffix)
			}
		}
	}
	_, err := b.captureControl(ctx, f.containerID, cmd)
	return err
}

func (b *Backend) cleanupAfterExit(f *execFiles) {
	f.signalMu.Lock()
	f.exited = true
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	f.cleaned = f.cleaned || f.remove(ctx, b) == nil
	cancel()
	cleaned := f.cleaned
	f.signalMu.Unlock()
	if !cleaned {
		b.deferCleanup(f)
	}
}

// One lazy worker per backend, one bounded call at a time, FIFO rotation.
// Never drop work at a queue cap: bound the rate and batch size instead.
// Entries stay in the list even in flight, so Remove can retire them without
// a retry resurrecting an entry that was removed during an API call.
type cleanupQueue struct {
	mu      sync.Mutex
	items   list.List
	running bool
}

func (b *Backend) deferCleanup(f *execFiles) {
	q := &b.cleanup
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items.PushBack(f)
	if !q.running {
		q.running = true
		go b.retryCleanup()
	}
}

func (q *cleanupQueue) forget(id string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for e := q.items.Front(); e != nil; {
		next := e.Next()
		if e.Value.(*execFiles).containerID == id {
			q.items.Remove(e)
		}
		e = next
	}
}

func (b *Backend) retryCleanup() {
	q := &b.cleanup
	for {
		time.Sleep(time.Second)
		q.mu.Lock()
		for range min(8, q.items.Len()) {
			e := q.items.Front()
			if e == nil {
				break
			}
			f := e.Value.(*execFiles)
			q.mu.Unlock()
			done := b.retryFiles(f)
			q.mu.Lock()
			if done {
				q.items.Remove(e)
			} else {
				q.items.MoveToBack(e) // no-op if concurrently forgotten
			}
		}
		if q.items.Len() == 0 {
			q.running = false
			q.mu.Unlock()
			return
		}
		q.mu.Unlock()
	}
}

func (b *Backend) retryFiles(f *execFiles) bool {
	f.signalMu.Lock()
	defer f.signalMu.Unlock()
	if !f.exited && time.Now().Before(f.missingRetryAt) {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var err error
	if !f.exited {
		var inspect dockerapi.ExecInspect
		inspect, err = b.api.ExecInspect(ctx, f.execID)
		if dockerapi.IsNotFound(err) {
			// Recheck a transient 404 once. If Docker has forgotten the exec,
			// only container removal can safely reclaim its files; no rm or
			// further per-exec polling is needed for that to happen.
			if !f.missingRetryAt.IsZero() {
				return true
			}
			f.missingRetryAt = time.Now().Add(time.Minute)
			return false
		}
		if err == nil {
			f.missingRetryAt = time.Time{}
		}
		f.exited = err == nil && inspect.Stopped(f.execID)
	}
	if f.exited && !f.cleaned {
		err = f.remove(ctx, b)
		f.cleaned = err == nil
	}
	if dockerapi.IsNotFound(err) {
		// Even after the original exited, Docker may forget the rm helper.
		// Only retire a failed removal if the container itself is gone.
		live, err := b.Liveness(ctx, f.containerID)
		return err == nil && live == daemon.LivenessMissing
	}
	return f.cleaned
}
