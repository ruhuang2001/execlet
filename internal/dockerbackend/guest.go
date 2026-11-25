package dockerbackend

import (
	"archive/tar"
	"bytes"
	"context"
	"embed"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ruhuang2001/execlet/internal/daemon"
	"github.com/ruhuang2001/execlet/internal/dockerapi"
	"github.com/ruhuang2001/execlet/internal/guestprotocol"
)

//go:embed guestbin
var binaries embed.FS

type guestSession struct {
	closed    bool
	readers   sync.Map
	b         *Backend
	id        string
	lock      chan struct{}
	stream    io.ReadWriteCloser
	protected bool
	runtime   string
	mounts    []daemon.BindMount
}

func (b *Backend) SetGuestAgent(mode string, required bool) error {
	switch mode {
	case "auto", "off":
		if mode == "auto" {
			if entries, _ := fs.Glob(binaries, "guestbin/guest-linux-*"); len(entries) == 0 {
				mode = "off"
			}
		}
		if required && mode == "off" {
			return errors.New("persistent volumes require an enabled embedded guest agent; build with make build")
		}
		b.guestMode = mode
		return nil
	}
	return errors.New("guest-agent must be auto or off")
}

var archiveCache sync.Map

func guestArchive(arch string) ([]byte, error) {
	if v, ok := archiveCache.Load(arch); ok {
		return v.([]byte), nil
	}
	if arch != "amd64" && arch != "arm64" {
		return nil, fmt.Errorf("unsupported image architecture %q", arch)
	}
	binary, err := binaries.ReadFile("guestbin/guest-linux-" + arch)
	if err != nil {
		return nil, fmt.Errorf("guest binary unavailable: %w", err)
	}
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	for _, h := range []*tar.Header{{Name: ".execlet/", Mode: 0755, Typeflag: tar.TypeDir}, {Name: ".execlet/guest", Mode: 0755, Typeflag: tar.TypeReg, Size: int64(len(binary))}} {
		if err := w.WriteHeader(h); err != nil {
			return nil, err
		}
	}
	if _, err = w.Write(binary); err != nil {
		return nil, err
	}
	if err = w.Close(); err != nil {
		return nil, err
	}
	data, _ := archiveCache.LoadOrStore(arch, buf.Bytes())
	return data.([]byte), nil
}
func (b *Backend) guestImage(ctx context.Context, cfg *dockerapi.ContainerConfig) ([]byte, error) {
	if b.guestMode != "auto" {
		return nil, nil
	}
	info, err := b.api.ImageInspect(ctx, cfg.Image)
	if info.ID != "" {
		cfg.Image = info.ID
	}
	if err == nil && info.Architecture != "" {
		return guestArchive(info.Architecture)
	}
	archive, archiveErr := guestArchive(runtime.GOARCH)
	return archive, errors.Join(fmt.Errorf("image architecture unavailable; staged host architecture %s", runtime.GOARCH), err, archiveErr)
}
func (b *Backend) startGuest(ctx context.Context, id string, spec daemon.ContainerSpec) error {
	raw, err := b.captureAs(ctx, id, "", []string{protocol.Binary, "identity"})
	if err != nil {
		return err
	}
	var identity protocol.Identity
	if err := json.Unmarshal(raw, &identity); err != nil {
		return err
	}
	cmd := []string{protocol.Binary, "daemon", "--socket", protocol.Socket, "--identity", string(raw)}
	if spec.Network == daemon.NetworkFiltered {
		cmd = append(cmd, "--filtered")
	}
	eid, err := b.api.ExecCreate(ctx, id, dockerapi.ExecConfig{User: "0:0", Cmd: cmd})
	if err != nil {
		return err
	}
	if err = b.api.ExecStartDetached(ctx, eid); err != nil {
		return err
	}
	g := &guestSession{b: b, id: id, lock: make(chan struct{}, 1), runtime: spec.Runtime, protected: identity.UID != 0, mounts: slices.Clone(spec.Mounts)}
	ctx, cancel := context.WithTimeout(ctx, daemon.BackendCallTimeout)
	defer cancel()
	for {
		var out protocol.Result
		err = g.call(ctx, 'H', protocol.Request{}, &out)
		if err == nil {
			if out.Version != protocol.Version {
				g.close()
				return errors.New("incompatible guest protocol")
			}
			for _, check := range []struct {
				path string
				mode int64
				typ  daemon.FileType
			}{
				{PidDir, 0o700, daemon.FileTypeDir}, {protocol.Socket, 0o600, daemon.FileTypeOther},
			} {
				var entry daemon.FileEntry
				if err := g.call(ctx, 'S', protocol.Request{Path: check.path}, &entry); err != nil {
					g.close()
					return err
				}
				if entry.Mode != check.mode || entry.UID != 0 || entry.GID != 0 || entry.Type != check.typ {
					g.close()
					return fmt.Errorf("unexpected guest control permissions on %s", check.path)
				}
			}
			b.guests.Store(id, g)
			return nil
		}
		if daemon.CodeOf(err) == daemon.CodeInvalidState {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("guest handshake: %w", err)
		case <-time.After(25 * time.Millisecond):
		}
	}
}
func (b *Backend) guest(id string) *guestSession {
	v, ok := b.guests.Load(id)
	if !ok {
		return nil
	}
	return v.(*guestSession)
}
func (b *Backend) AgentInfo(id string) (string, bool) {
	if g := b.guest(id); g != nil {
		return "guest", g.protected
	}
	if b.guestMode == "auto" {
		return "unavailable", false
	}
	return "legacy", false
}
func (g *guestSession) reader(id string) *guestSession {
	if child, ok := g.readers.Load(id); ok {
		return child.(*guestSession)
	}
	child, _ := g.readers.LoadOrStore(id, &guestSession{b: g.b, id: g.id, lock: make(chan struct{}, 1)})
	return child.(*guestSession)
}
func (g *guestSession) closeReader(id string) {
	if r, ok := g.readers.LoadAndDelete(id); ok {
		r.(*guestSession).close()
	}
}
func (g *guestSession) close() {
	g.readers.Range(func(k, _ any) bool { g.closeReader(k.(string)); return true })
	g.lock <- struct{}{}
	defer func() { <-g.lock }()
	g.closed = true
	if g.stream != nil {
		_ = g.stream.Close()
		g.stream = nil
	}
}
func (g *guestSession) call(ctx context.Context, kind byte, req protocol.Request, out any) error {
	ctx, cancel := context.WithTimeout(ctx, daemon.BackendCallTimeout)
	defer cancel()
	select {
	case g.lock <- struct{}{}:
		defer func() { <-g.lock }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if g.closed {
		return errors.New("guest relay closed")
	}
	if g.stream != nil {
		state, err := g.b.api.ContainerInspect(ctx, g.id)
		if err != nil {
			return err
		}
		if state.State.Paused {
			return &daemon.Error{Code: daemon.CodeInvalidState, Message: "sandbox is paused"}
		}
	}
	opening := g.stream == nil
	if opening {
		attachCtx, stop := context.WithCancel(context.WithoutCancel(ctx))
		handshake := context.AfterFunc(ctx, stop)
		stream, err := g.b.api.ExecAttach(attachCtx, g.id, dockerapi.ExecConfig{User: "0:0", Cmd: []string{protocol.Binary, "connect"}}, false)
		if !handshake() {
			if stream != nil {
				_ = stream.Close()
			}
			stop()
			return ctx.Err()
		}
		if err != nil {
			stop()
			if strings.Contains(err.Error(), "is paused") {
				return &daemon.Error{Code: daemon.CodeInvalidState, Message: "sandbox is paused"}
			}
			return err
		}
		g.stream = &guestStream{ReadWriteCloser: stream, stop: stop}
	}
	stream := g.stream
	stop := context.AfterFunc(ctx, func() { _ = stream.Close() })
	var err error
	if opening && kind != 'H' {
		var hello protocol.Result
		if err = protocol.Write(stream, 'H', protocol.Request{}); err == nil {
			err = readGuestReply(stream, 'H', protocol.Request{}, &hello)
		}
		if err == nil && hello.Version != protocol.Version {
			err = errors.New("incompatible guest protocol")
		}
	}
	if err == nil {
		err = protocol.Write(stream, kind, req)
		if errors.Is(err, protocol.ErrFrameTooLarge) {
			err = &daemon.Error{Code: daemon.CodePayloadTooLarge, Message: err.Error(), Err: err}
		}
	}
	if err == nil {
		err = readGuestReply(stream, kind, req, out)
	}
	if !stop() {
		err = ctx.Err()
	}
	if _, applicationError := errors.AsType[*daemon.Error](err); err != nil && !applicationError {
		_ = stream.Close()
		g.stream = nil
	}
	return err
}

func readGuestReply(stream io.Reader, kind byte, req protocol.Request, out any) error {
	for {
		reply, data, err := protocol.Read(stream)
		if err != nil {
			return err
		}
		if reply == 'e' {
			var failure protocol.Error
			if err := json.Unmarshal(data, &failure); err != nil {
				return err
			}
			return &daemon.Error{Code: daemon.Code(failure.Code), Message: failure.Message}
		}
		if reply != protocol.ResponseType(kind) && (kind != 'K' || reply != 'k') {
			return errors.New("unexpected guest response")
		}
		if kind != 'L' || req.Op != "frames" || len(data) == 0 || data[0] != '{' {
			return json.Unmarshal(data, out)
		}
		var page protocol.ListPage[daemon.FileEntry]
		if err := json.Unmarshal(data, &page); err != nil {
			return err
		}
		entries := out.(*[]daemon.FileEntry)
		if len(*entries)+len(page.Entries) > req.Limit+1 || page.More && len(page.Entries) == 0 {
			return errors.New("invalid guest list page")
		}
		*entries = append(*entries, page.Entries...)
		if !page.More {
			return nil
		}
	}
}

type guestStream struct {
	io.ReadWriteCloser
	stop context.CancelFunc
}

func (s *guestStream) Close() error { s.stop(); return s.ReadWriteCloser.Close() }
func (b *Backend) guestUnavailable(id string, err error) {
	slog.Warn("guest agent unavailable", "container", id, "reason", err)
}
func (b *Backend) StatFile(ctx context.Context, id, path string) (daemon.FileEntry, error) {
	var entry daemon.FileEntry
	if g := b.guest(id); g != nil {
		err := g.call(ctx, 'S', protocol.Request{Path: path}, &entry)
		return entry, err
	}
	return b.statWrittenFile(ctx, id, path)
}
func (b *Backend) guestList(ctx context.Context, g *guestSession, path, after string, limit int) ([]daemon.FileEntry, error) {
	entries := []daemon.FileEntry{}
	err := g.call(ctx, 'L', protocol.Request{Op: "frames", Path: path, After: after, Limit: limit}, &entries)
	return entries, err
}

func (b *Backend) effectiveRuntime(ctx context.Context, id, requested string) (string, error) {
	if requested != "" {
		return requested, nil
	}
	container, err := b.api.ContainerInspect(ctx, id)
	if err != nil {
		return "", fmt.Errorf("inspect Docker-selected runtime: %w", err)
	}
	return container.HostConfig.Runtime, nil
}

func (b *Backend) RestoreGuest(ctx context.Context, id string, protected bool, requested string, mounts []daemon.BindMount) error {
	rt, err := b.effectiveRuntime(ctx, id, requested)
	if err != nil {
		return err
	}
	b.guests.Store(id, &guestSession{b: b, id: id, runtime: rt, protected: protected, lock: make(chan struct{}, 1), mounts: slices.Clone(mounts)})
	return nil
}
