package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/ruhuang2001/tiny-sandbox/internal/guestprotocol"
	"golang.org/x/sys/unix"
)

type server struct {
	identity protocol.Identity
	mu       sync.Mutex
	jobs     map[string]*job
}
type job struct {
	changed        chan struct{}
	mu             sync.Mutex
	pid            int
	terminal       *os.File
	done           bool
	exit           int
	reason         string
	stdout, stderr output
}
type output struct {
	changed chan struct{}
	head    int
	mu      sync.Mutex
	data    []byte
	base    int64
	rolling bool
}

func (o *output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	const capBytes = (1 << 20) + 1
	n := len(p)
	if o.rolling && n >= capBytes {
		o.base += int64(len(o.data) + n - capBytes)
		o.data = append(o.data[:0], p[n-capBytes:]...)
		o.head = 0
	} else {
		room := capBytes - len(o.data)
		take := min(room, n)
		o.data = append(o.data, p[:take]...)
		if o.rolling && n > take {
			rest := p[take:]
			first := copy(o.data[o.head:], rest)
			copy(o.data, rest[first:])
			o.head = (o.head + len(rest)) % capBytes
			o.base += int64(len(rest))
		}
	}
	select {
	case o.changed <- struct{}{}:
	default:
	}
	return len(p), nil
}
func (o *output) read(offset int64) (data []byte, next int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	start := max(0, min(offset-o.base, int64(len(o.data))))
	size := min(int64(len(o.data))-start, int64(64<<10))
	if size > 0 {
		index := (o.head + int(start)) % len(o.data)
		data = make([]byte, size)
		n := copy(data, o.data[index:])
		copy(data[n:], o.data[:size-int64(n)])
	}
	return data, o.base + start + int64(len(data))
}
func (s *server) dispatch(kind byte, r protocol.Request) (protocol.Result, error) {
	if r.ID == "" {
		return protocol.Result{}, errors.New("missing job identity")
	}
	s.mu.Lock()
	j := s.jobs[r.ID]
	if r.Op == "start" && j == nil {
		var err error
		j, err = s.start(r, kind == 'P')
		if err != nil {
			s.mu.Unlock()
			return protocol.Result{}, err
		}
		s.jobs[r.ID] = j
	}
	if j == nil {
		s.mu.Unlock()
		return protocol.Result{}, &protocol.Error{Code: "not_found", Message: "unknown guest job"}
	}
	if r.Op == "forget" {
		j.mu.Lock()
		if j.done {
			delete(s.jobs, r.ID)
		}
		j.mu.Unlock()
	}
	s.mu.Unlock()
	if r.Op == "read" {
		result := j.snapshot(r)
		if !result.Done && len(result.Stdout)+len(result.Stderr) == 0 && r.WaitMS > 0 {
			select {
			case <-j.changed:
			case <-time.After(time.Duration(min(r.WaitMS, 1000)) * time.Millisecond):
			}
			result = j.snapshot(r)
		}
		return result, nil
	}
	if r.Op == "input" {
		return protocol.Result{}, j.input(r.Data)
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	result := protocol.Result{PID: j.pid, Done: j.done, ExitCode: j.exit, Reason: j.reason}
	switch r.Op {
	case "start", "forget", "status":
	case "close", "kill", "term":
		if !j.done {
			var err error
			result.Signaled, err = j.signal(r.Op)
			if err != nil {
				return result, err
			}
			if result.Signaled && j.reason == "" {
				j.reason = "canceled"
			}
		}
	case "resize":
		if j.done || j.terminal == nil {
			return result, &protocol.Error{Code: "invalid_state", Message: "terminal is closed"}
		}
		return result, pty.Setsize(j.terminal, &pty.Winsize{Rows: r.Rows, Cols: r.Cols})
	default:
		return result, errors.New("unknown guest job operation")
	}
	return result, nil
}
func (s *server) start(r protocol.Request, terminal bool) (*job, error) {
	if len(r.Command) == 0 {
		return nil, errors.New("empty guest command")
	}
	id := s.identity
	args := append([]string{"run", cmp.Or(r.Dir, id.Dir, "/")}, r.Command...)
	cmd := exec.CommandContext(context.Background(), protocol.Binary, args...)
	cmd.WaitDelay = time.Second
	cmd.Env = append(os.Environ(), "HOME="+id.Home)
	if terminal && os.Getenv("TERM") == "" {
		cmd.Env = append(cmd.Env, "TERM=xterm-256color")
	}
	cmd.Env = append(cmd.Env, r.Env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Credential: &syscall.Credential{Uid: id.UID, Gid: id.GID, Groups: id.Groups}}
	j := &job{changed: make(chan struct{}, 1)}
	j.stdout.changed, j.stderr.changed = j.changed, j.changed

	j.stdout.rolling, j.stderr.rolling = r.OutputTail, r.OutputTail
	var readDone chan struct{}
	if terminal {
		j.stdout.rolling = true
		f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: r.Rows, Cols: r.Cols})
		if err != nil {
			return nil, err
		}
		fd, err := unix.FcntlInt(f.Fd(), unix.F_DUPFD_CLOEXEC, 0)
		_ = f.Close()
		if err == nil {
			err = unix.SetNonblock(fd, true)
		}
		if err != nil {
			if fd >= 0 {
				_ = unix.Close(fd)
			}
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return nil, err
		}
		f = os.NewFile(uintptr(fd), "guest-terminal")
		j.terminal = f
		readDone = make(chan struct{})
		go func() { defer close(readDone); _, _ = io.Copy(&j.stdout, f) }()
	} else {
		cmd.Stdout, cmd.Stderr = &j.stdout, &j.stderr
		cmd.Stdin = strings.NewReader(r.Stdin)
		if err := cmd.Start(); err != nil {
			return nil, err
		}
	}
	j.pid = cmd.Process.Pid
	go j.wait(cmd, readDone, r.TimeoutMS)
	return j, nil
}

func members(session int) (live, stopped []int, err error) {
	files, err := os.ReadDir("/proc")
	if err != nil {
		return nil, nil, err
	}
	for _, f := range files {
		pid, err := strconv.Atoi(f.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile("/proc/" + f.Name() + "/stat")
		if err != nil {
			continue
		}
		_, tail, ok := strings.CutLast(string(raw), ") ")
		if !ok {
			continue
		}
		fields := strings.Fields(tail)
		if len(fields) < 4 || fields[3] != strconv.Itoa(session) || fields[0] == "Z" || fields[0] == "X" {
			continue
		}
		live = append(live, pid)
		if fields[0] == "T" || fields[0] == "t" {
			stopped = append(stopped, pid)
		}
	}
	return live, stopped, nil
}
func (j *job) signal(action string) (bool, error) {
	live, _, err := members(j.pid)
	if err != nil || len(live) == 0 {
		return false, err
	}
	if action == "term" {
		return true, syscall.Kill(-j.pid, syscall.SIGTERM)
	}
	until := time.Now().Add(time.Second)
	for {
		for _, pid := range live {
			_ = syscall.Kill(pid, syscall.SIGSTOP)
		}
		var stopped []int
		live, stopped, err = members(j.pid)
		if err != nil {
			return false, err
		}
		if len(live) == len(stopped) {
			for _, pid := range stopped {
				if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
					return false, err
				}
			}
			return len(live) > 0, nil
		}
		if time.Now().After(until) {
			return false, errors.New("signal delivery unconfirmed")
		}
		time.Sleep(time.Millisecond)
	}
}
func (j *job) wait(cmd *exec.Cmd, readDone chan struct{}, timeoutMS int64) {
	var deadline *time.Timer
	if timeoutMS > 0 {
		deadline = time.AfterFunc(time.Duration(timeoutMS)*time.Millisecond, func() {
			j.mu.Lock()
			defer j.mu.Unlock()
			if !j.done {
				if sent, _ := j.signal("kill"); sent && j.reason == "" {
					j.reason = "timeout"
				}
			}
		})
		defer deadline.Stop()
	}
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, j.pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	for {
		live, _, err := members(j.pid)
		if err != nil || len(live) == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	j.mu.Lock()
	err := cmd.Wait()
	if cmd.ProcessState == nil {
		fmt.Fprintln(os.Stderr, err)
		j.exit = -1
	} else {
		j.exit = cmd.ProcessState.ExitCode()
		if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			j.exit = 128 + int(status.Signal())
		}
	}
	if j.terminal != nil {
		_ = j.terminal.SetReadDeadline(time.Now().Add(time.Second))
		<-readDone
		_ = j.terminal.Close()
	}
	j.done = true
	select {
	case j.changed <- struct{}{}:
	default:
	}
	j.mu.Unlock()
}

func (j *job) input(data []byte) error {
	j.mu.Lock()
	f, done := j.terminal, j.done
	j.mu.Unlock()
	if f == nil || done {
		return &protocol.Error{Code: "invalid_state", Message: "terminal is closed"}
	}
	if err := f.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		return err
	}
	n, err := f.Write(data)
	if err != nil {
		return fmt.Errorf("terminal accepted %d of %d input bytes: %w", n, len(data), err)
	}
	return nil
}

func (j *job) snapshot(r protocol.Request) protocol.Result {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := protocol.Result{PID: j.pid, Done: j.done, ExitCode: j.exit, Reason: j.reason}
	out.Stdout, out.Next = j.stdout.read(r.Offset)
	out.Stderr, out.ErrNext = j.stderr.read(r.ErrOffset)
	return out
}
