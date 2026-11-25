package membackend

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/ruhuang2001/execlet/internal/daemon"
)

type Program func(ctx context.Context, args []string) (stdout, stderr string, exitCode int)

type Streamer func(ctx context.Context, write func(stdout, stderr []byte) error) (exitCode int)

type Backend struct {
	mu         sync.Mutex
	containers map[string]*container
	nextID     int

	Programs  map[string]Program
	Streamers map[string]Streamer
	Default   Program
	CreateErr error
	ExecErr   error

	lastExec daemon.ExecSpec

	createBlock chan struct{}
}

type container struct {
	id             string
	spec           daemon.ContainerSpec
	state          daemon.Liveness
	oom            int64
	files          map[string][]byte
	archiveHeaders map[string]tar.Header
	procs          []*process
}

type process struct {
	container string
	command   []string
	outW      *io.PipeWriter
	errW      *io.PipeWriter
	outR      *io.PipeReader
	errR      *io.PipeReader

	mu         sync.Mutex
	signals    []daemon.Signal
	ignoreTerm bool
	ignoreKill bool
	cancel     context.CancelFunc
	dead       bool
	exitCode   int
	waitErr    error

	closeOnce sync.Once
	done      chan struct{}
}

var _ daemon.Backend = (*Backend)(nil)

func New() *Backend {
	return &Backend{
		containers: make(map[string]*container),
		Programs:   make(map[string]Program),
		Streamers:  make(map[string]Streamer),
		Default: func(context.Context, []string) (string, string, int) {
			return "", "", 0
		},
	}
}

func (b *Backend) Create(_ context.Context, spec daemon.ContainerSpec) (string, error) {
	b.mu.Lock()
	block := b.createBlock
	b.mu.Unlock()
	if block != nil {
		<-block
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.CreateErr != nil {
		return "", b.CreateErr
	}
	b.nextID++
	c := &container{
		id:    fmt.Sprintf("ctr_%d", b.nextID),
		spec:  spec,
		state: daemon.LivenessRunning,
		files: make(map[string][]byte),
	}
	b.containers[c.id] = c
	return c.id, nil
}

func (b *Backend) Remove(_ context.Context, id string) error {
	b.mu.Lock()
	c := b.containers[id]
	if c != nil {
		c.state = daemon.LivenessMissing
	}
	if c != nil {
		c.stopProcess()
	}
	b.mu.Unlock()
	if c == nil {
		return fmt.Errorf("membackend: container %s: %w", id, daemon.ErrNotFound)
	}
	return nil
}

func (b *Backend) Pause(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, err := b.get(id)
	if err != nil {
		return err
	}
	if c.state != daemon.LivenessRunning {
		return fmt.Errorf("membackend: container %s is %s", id, c.state)
	}
	c.state = daemon.LivenessPaused
	return nil
}

func (b *Backend) Unpause(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, err := b.get(id)
	if err != nil {
		return err
	}
	if c.state != daemon.LivenessPaused {
		return fmt.Errorf("membackend: container %s is %s", id, c.state)
	}
	c.state = daemon.LivenessRunning
	return nil
}

func (b *Backend) Exec(ctx context.Context, id string, spec daemon.ExecSpec) (daemon.Process, error) {
	command := spec.Command
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lastExec = daemon.ExecSpec{
		Command: append([]string(nil), spec.Command...),
		Env:     append([]string(nil), spec.Env...),
		Dir:     spec.Dir,
		Stdin:   spec.Stdin,
	}
	if b.ExecErr != nil {
		err := b.ExecErr
		b.ExecErr = nil
		return nil, err
	}
	c, err := b.get(id)
	if err != nil {
		return nil, err
	}
	if c.state != daemon.LivenessRunning {
		return nil, fmt.Errorf("membackend: container %s is %s", id, c.state)
	}

	if len(command) == 0 {
		return nil, errors.New("membackend: empty command")
	}

	outR, outW := io.Pipe()
	errR, errW := io.Pipe()
	p := &process{
		container: id,
		command:   append([]string(nil), command...),
		outR:      outR,
		errR:      errR,
		outW:      outW,
		errW:      errW,
		done:      make(chan struct{}),
	}
	c.procs = slices.DeleteFunc(c.procs, func(existing *process) bool { return existing.finished() })
	c.procs = append(c.procs, p)

	if streamer, ok := b.Streamers[command[0]]; ok {
		go p.runStreamer(context.WithoutCancel(ctx), streamer)
		return p, nil
	}
	program := b.Default
	if fn, ok := b.Programs[command[0]]; ok {
		program = fn
	}
	go p.run(context.WithoutCancel(ctx), program, command[1:])
	return p, nil
}

func (b *Backend) Liveness(_ context.Context, id string) (daemon.Liveness, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.containers[id]
	if c == nil {
		return daemon.LivenessMissing, nil
	}
	return c.state, nil
}

func (b *Backend) OOMKillCount(_ context.Context, id string) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.containers[id]
	if c == nil {
		return 0, fmt.Errorf("membackend: container %s: %w", id, daemon.ErrNotFound)
	}
	return c.oom, nil
}

func (b *Backend) ReadFile(_ context.Context, id, p string) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, err := b.get(id)
	if err != nil {
		return nil, err
	}
	p = path.Clean(p)
	if h, ok := c.archiveHeaders[p]; ok && h.Typeflag != tar.TypeReg && h.Typeflag != 0 && h.Typeflag != tar.TypeLink {
		return nil, &daemon.Error{Code: daemon.CodeInvalidRequest, Message: "path is not a regular file: " + p}
	}
	data, ok := c.files[p]
	if !ok {
		prefix := strings.TrimSuffix(p, "/") + "/"
		for name := range c.files {
			if strings.HasPrefix(name, prefix) {
				return nil, &daemon.Error{Code: daemon.CodeInvalidRequest, Message: "path must identify a regular file: " + p}
			}
		}
		for name := range c.archiveHeaders {
			if strings.HasPrefix(name, prefix) {
				return nil, &daemon.Error{Code: daemon.CodeInvalidRequest, Message: "path must identify a regular file: " + p}
			}
		}
		return nil, fmt.Errorf("membackend: %s: %w", p, daemon.ErrNotFound)
	}
	if len(data) > daemon.MaxFileBytes {
		return nil, &daemon.Error{Code: daemon.CodePayloadTooLarge, Message: fmt.Sprintf("file exceeds %d bytes", daemon.MaxFileBytes)}
	}
	return append([]byte(nil), data...), nil
}

func (b *Backend) WriteFile(_ context.Context, id, p string, data []byte) (daemon.FileEntry, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, err := b.get(id)
	if err != nil {
		return daemon.FileEntry{}, err
	}
	p = path.Clean(p)
	if h, ok := c.archiveHeaders[p]; ok && h.Typeflag == tar.TypeDir {
		return daemon.FileEntry{}, &daemon.Error{Code: daemon.CodeInvalidRequest, Message: "destination cannot hold a file: " + p}
	}
	prefix := strings.TrimSuffix(p, "/") + "/"
	for name := range c.files {
		if strings.HasPrefix(name, prefix) {
			return daemon.FileEntry{}, &daemon.Error{Code: daemon.CodeInvalidRequest, Message: "destination cannot hold a file: " + p}
		}
	}
	c.detachHardlinks(p)
	c.files[p] = append([]byte(nil), data...)
	delete(c.archiveHeaders, p)
	return memRegularFile(path.Base(p), int64(len(data))), nil
}

func (b *Backend) get(id string) (*container, error) {
	c := b.containers[id]
	if c == nil || c.state == daemon.LivenessMissing {
		return nil, fmt.Errorf("membackend: container %s: %w", id, daemon.ErrNotFound)
	}
	return c, nil
}

func (b *Backend) Kill(id string) {
	b.mu.Lock()
	c := b.containers[id]
	if c != nil {
		c.state = daemon.LivenessStopped
	}
	if c != nil {
		c.stopProcess()
	}
	b.mu.Unlock()
}

func (b *Backend) Disappear(id string) {
	b.mu.Lock()
	c := b.containers[id]
	if c != nil {
		c.state = daemon.LivenessMissing
	}
	if c != nil {
		c.stopProcess()
	}
	b.mu.Unlock()
}

func (b *Backend) IgnoreTerm(id string) {
	b.setIgnore(id, true, false)
}

func (b *Backend) IgnoreSignals(id string) {
	b.setIgnore(id, true, true)
}

func (b *Backend) setIgnore(id string, term, kill bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if c := b.containers[id]; c != nil {
		for _, p := range c.procs {
			p.mu.Lock()
			p.ignoreTerm = term
			p.ignoreKill = kill
			p.mu.Unlock()
		}
	}
}

func (b *Backend) IDs() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	ids := make([]string, 0, len(b.containers))
	for id := range b.containers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (b *Backend) SetOOMKills(id string, n int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if c := b.containers[id]; c != nil {
		c.oom = n
	}
}

func (b *Backend) Process(id string) *Process {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.containers[id]
	if c == nil || len(c.procs) == 0 {
		return nil
	}
	return &Process{proc: c.procs[len(c.procs)-1]}
}

func (b *Backend) Processes(id string) []*Process {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.containers[id]
	if c == nil {
		return nil
	}
	out := make([]*Process, 0, len(c.procs))
	for _, p := range c.procs {
		out = append(out, &Process{proc: p})
	}
	return out
}

type Process struct{ proc *process }

func (p *Process) Signals() []daemon.Signal { return p.proc.Signals() }

func (b *Backend) LastExecSpec() daemon.ExecSpec {
	b.mu.Lock()
	defer b.mu.Unlock()
	return daemon.ExecSpec{
		Command: append([]string(nil), b.lastExec.Command...),
		Env:     append([]string(nil), b.lastExec.Env...),
		Dir:     b.lastExec.Dir,
		Stdin:   b.lastExec.Stdin,
	}
}

func (b *Backend) State(id string) daemon.Liveness {
	b.mu.Lock()
	defer b.mu.Unlock()
	if c := b.containers[id]; c != nil {
		return c.state
	}
	return daemon.LivenessMissing
}

func (b *Backend) HoldCreate() (release func()) {
	ch := make(chan struct{})
	b.mu.Lock()
	b.createBlock = ch
	b.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			b.createBlock = nil
			b.mu.Unlock()
			close(ch)
		})
	}
}

func (p *process) Signals() []daemon.Signal {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]daemon.Signal(nil), p.signals...)
}

func (p *process) Stdout() io.Reader { return p.outR }

func (p *process) Stderr() io.Reader { return p.errR }

func (p *process) Wait() (int, error) {
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exitCode, p.waitErr
}

func (p *process) Signal(sig daemon.Signal) error {
	p.mu.Lock()
	p.signals = append(p.signals, sig)
	if p.dead {
		p.mu.Unlock()
		return nil
	}
	cancel := p.cancel
	ignored := (sig == daemon.SignalTerm && p.ignoreTerm) || (sig == daemon.SignalKill && p.ignoreKill)
	p.mu.Unlock()
	if ignored {
		return nil
	}

	switch sig {
	case daemon.SignalTerm:
		if cancel != nil {
			cancel()
		}
	case daemon.SignalKill:
		p.finish(137, nil)
	}
	return nil
}

func (p *process) finished() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func (p *process) runStreamer(ctx context.Context, streamer Streamer) {
	ctx, cancel := context.WithCancel(ctx)
	p.mu.Lock()
	p.cancel = cancel
	if p.dead {
		cancel()
	}
	p.mu.Unlock()
	defer cancel()

	write := func(stdout, stderr []byte) error {
		if len(stdout) > 0 {
			if _, err := p.outW.Write(stdout); err != nil {
				return err
			}
		}
		if len(stderr) > 0 {
			if _, err := p.errW.Write(stderr); err != nil {
				return err
			}
		}
		return nil
	}
	code := streamer(ctx, write)
	if ctx.Err() != nil {
		code = 137
	}
	p.finish(code, nil)
}

func (p *process) run(ctx context.Context, program Program, args []string) {
	ctx, cancel := context.WithCancel(ctx)
	p.mu.Lock()
	p.cancel = cancel
	if p.dead {
		cancel()
	}
	p.mu.Unlock()
	defer cancel()

	stdout, stderr, code := program(ctx, args)
	if ctx.Err() != nil {
		code = 137
	}
	_, _ = io.WriteString(p.outW, stdout)
	_, _ = io.WriteString(p.errW, stderr)
	p.finish(code, nil)
}

func (p *process) finish(code int, waitErr error) {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.dead = true
		p.exitCode = code
		p.waitErr = waitErr
		p.mu.Unlock()
		_ = p.outW.Close()
		_ = p.errW.Close()
		close(p.done)
	})
}

func (c *container) stopProcess() {
	for _, p := range c.procs {
		p.mu.Lock()
		cancel := p.cancel
		p.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		p.finish(137, nil)
	}
}
