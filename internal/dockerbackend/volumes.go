package dockerbackend

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/ruhuang2001/execlet/internal/daemon"
	"github.com/ruhuang2001/execlet/internal/dockerapi"
	"github.com/ruhuang2001/execlet/internal/guestprotocol"
)

func (b *Backend) verifyMounts(ctx context.Context, id string, observed []dockerapi.MountPoint, expected []daemon.BindMount) error {
	if err := verifyDockerMounts(observed, expected); err != nil {
		return err
	}
	if len(expected) == 0 {
		return nil
	}
	args := []string{protocol.Binary, "verify-volume-mounts"}
	for _, m := range expected {
		args = append(args, m.Target, strconv.FormatInt(m.SizeBytes, 10), strconv.FormatBool(m.ReadOnly))
	}
	if _, err := b.captureExec(ctx, id, "0:0", args, 64<<10); err != nil {
		return fmt.Errorf("guest did not confirm volume bounds and mount permissions: %w", err)
	}
	return nil
}

func verifyDockerMounts(observed []dockerapi.MountPoint, expected []daemon.BindMount) error {
	matched := make(map[string]bool, len(expected))
	for _, actual := range observed {
		requested := false
		for _, m := range expected {
			if strings.HasPrefix(actual.Destination, m.Target+"/") {
				return fmt.Errorf("runtime exposed an additional mount at %s", actual.Destination)
			}
			if actual.Destination == m.Target {
				if matched[m.Target] || actual.Type != "bind" || actual.Source != m.Source || actual.RW == nil || *actual.RW == m.ReadOnly {
					return fmt.Errorf("runtime did not confirm the requested bind mount at %s", m.Target)
				}
				matched[m.Target] = true
				requested = true
			}
		}
		if actual.Type == "bind" && !requested {
			return errors.New("runtime exposed an unrequested bind mount")
		}
	}
	if len(matched) != len(expected) {
		return errors.New("runtime did not report all requested volume mounts")
	}
	return nil
}

func (b *Backend) volumeWriteError(id, p string, err error) error {
	e, ok := errors.AsType[*protocol.Error](err)
	if !ok || (e.Code != protocol.CodeInsufficientStorage && e.Code != protocol.CodeReadOnlyFilesystem) {
		return notFound(err, "%s", p)
	}
	if g := b.guest(id); g != nil {
		for _, m := range g.mounts {
			if p == m.Target || strings.HasPrefix(p, m.Target+"/") {
				if e.Code == protocol.CodeInsufficientStorage && !m.ReadOnly {
					return &daemon.Error{Code: daemon.CodeVolumeQuotaExceeded, Message: "persistent volume capacity exhausted"}
				}
				if e.Code == protocol.CodeReadOnlyFilesystem && m.ReadOnly {
					return &daemon.Error{Code: daemon.CodeVolumeReadOnly, Message: "persistent volume is read-only"}
				}
			}
		}
	}
	return &daemon.Error{Code: daemon.CodeBackendError, Message: "filesystem write failed", Err: err}
}
