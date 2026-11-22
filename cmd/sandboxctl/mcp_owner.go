package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	sandbox "github.com/ruhuang2001/tiny-sandbox/sandbox-go"
)

var errSandboxGone = toolError("sandbox_not_running", "sandbox has expired or was destroyed")

var errUnknownSandbox = toolError("sandbox_not_running", "sandbox does not belong to this workspace and MCP configuration")

func (s *sandboxTools) ownerScope(in *invocation) (string, error) {
	workspace, err := filepath.Abs(s.workspace.Name())
	if err != nil {
		return "", err
	}
	workspace, err = filepath.EvalSymlinks(workspace)
	if err != nil {
		return "", err
	}
	domain := in.effectiveDomain()
	if !strings.Contains(domain, "://") {
		domain = "http://" + domain
	}
	u, err := url.Parse(domain)
	if err != nil {
		return "", err
	}
	u.Host = strings.ToLower(u.Host)
	if (u.Scheme == "http" && u.Port() == "80") || (u.Scheme == "https" && u.Port() == "443") {
		u.Host = strings.TrimSuffix(u.Host, ":"+u.Port())
	}
	sorted := func(items []string) []string {
		values := slices.Clone(items)
		slices.Sort(values)
		return slices.Compact(values)
	}
	scope := []any{"sandboxctl-mcp-v1", workspace, strings.TrimRight(u.String(), "/"), cmp.Or(in.globals.apiKey, os.Getenv("SANDBOX_API_KEY")), sorted(s.images), sorted(s.networks)}
	data, err := json.Marshal(scope)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

func (s *sandboxTools) receiptPath(id string) string { return filepath.Join(s.cache, id+".owner") }

func (s *sandboxTools) saveReceipt(id string) error {
	if err := os.MkdirAll(s.cache, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.cache, ".owner-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	_, err = tmp.WriteString(s.scope)
	if err = errors.Join(err, tmp.Sync(), tmp.Close()); err != nil {
		return err
	}
	return os.Link(tmp.Name(), s.receiptPath(id))
}

func (s *sandboxTools) openReceipt(ctx context.Context, id string) (*os.File, error) {
	f, err := os.OpenFile(s.receiptPath(id), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errUnknownSandbox
	}
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
		}
	}()
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errUnknownSandbox
	}
	content, err := io.ReadAll(io.LimitReader(f, 65))
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(content, []byte(s.scope)) {
		return nil, errUnknownSandbox
	}
	ok = true
	return f, nil
}

func (s *sandboxTools) forgetReceipt(id string) error {
	for _, name := range []string{s.manifestPath(id), s.receiptPath(id)} {
		if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (s *sandboxTools) renew(ctx context.Context, id string) error {
	sb, err := s.client.UpdateSandbox(ctx, id, sandbox.UpdateSandboxRequest{TimeoutSeconds: &s.ttl})
	if err == nil && sb.State == sandbox.StateRunning {
		return nil
	}
	if errors.Is(err, sandbox.ErrInvalidState) {
		sb, err = s.client.GetSandbox(ctx, id)
	}
	if err == nil && sb.State == sandbox.StateKilled {
		_, err = s.client.DestroySandbox(ctx, id)
		if err == nil {
			err = sandbox.ErrNotFound
		}
	}
	if errors.Is(err, sandbox.ErrNotFound) {
		if cleanupErr := s.forgetReceipt(id); cleanupErr != nil {
			return cleanupErr
		}
		return errSandboxGone
	}
	if err != nil {
		if isConnectionRefused(err) || ctx.Err() != nil {
			return err
		}
		return toolError("sandbox_not_running", "cannot renew sandbox: "+err.Error())
	}
	return toolError("sandbox_not_running", "sandbox is "+string(sb.State))
}
