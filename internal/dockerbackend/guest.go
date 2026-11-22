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
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
	"github.com/ruhuang2001/tiny-sandbox/internal/dockerapi"
	"github.com/ruhuang2001/tiny-sandbox/internal/guestprotocol"
)

//go:embed guestbin
var binaries embed.FS

type guestSession struct {
	b         *Backend
	id        string
	lock      chan struct{}
	stream    io.ReadWriteCloser
	protected bool
	runtime   string
}

func (b *Backend) SetGuestAgent(mode string) error {
	switch mode {
	case "auto", "off":
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
	for _, h := range []*tar.Header{{Name: ".tiny-sandbox/", Mode: 0755, Typeflag: tar.TypeDir}, {Name: ".tiny-sandbox/guest", Mode: 0755, Typeflag: tar.TypeReg, Size: int64(len(binary))}} {
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
func (b *Backend) deliverGuest(ctx context.Context, id string) error {
	c, err := b.api.ContainerInspect(ctx, id)
	if err != nil {
		return err
	}
	if c.Image == "" {
		return errors.New("missing container image identity")
	}
	image, err := b.api.ImageInspect(ctx, c.Image)
	if err != nil {
		return err
	}
	archive, err := guestArchive(image.Architecture)
	if err != nil {
		return err
	}
	return b.api.ArchivePut(ctx, id, "/", archive)
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
	g := &guestSession{b: b, id: id, lock: make(chan struct{}, 1), runtime: spec.Runtime, protected: identity.UID != 0}
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
			b.guests.Store(id, g)
			return nil
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
func (g *guestSession) close() {
	g.lock <- struct{}{}
	defer func() { <-g.lock }()
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
	if g.stream == nil {
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
	var data []byte
	var reply byte
	err := protocol.Write(stream, kind, req)
	if err == nil {
		reply, data, err = protocol.Read(stream)
	}
	if !stop() {
		err = ctx.Err()
	}
	if err != nil {
		_ = stream.Close()
		g.stream = nil
		if errors.Is(err, context.DeadlineExceeded) {
			check, stop := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
			defer stop()
			if live, e := g.b.Liveness(check, g.id); e == nil && live == daemon.LivenessPaused {
				return &daemon.Error{Code: daemon.CodeInvalidState, Message: "sandbox is paused"}
			}
		}
		return err
	}
	if reply == 'e' {
		var failure protocol.Error
		if err = json.Unmarshal(data, &failure); err != nil {
			return err
		}
		return &daemon.Error{Code: daemon.Code(failure.Code), Message: failure.Message}
	}
	if reply != kind+32 {
		return errors.New("unexpected guest response")
	}
	return json.Unmarshal(data, out)
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
	var entries []daemon.FileEntry
	err := g.call(ctx, 'L', protocol.Request{Path: path, After: after, Limit: limit}, &entries)
	return entries, err
}

func (b *Backend) RestoreGuest(id string, protected bool, rt string) {
	b.guests.Store(id, &guestSession{b: b, id: id, runtime: rt, protected: protected, lock: make(chan struct{}, 1)})
}
