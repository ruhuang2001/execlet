package dockerbackend

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
)

func (b *Backend) Rename(ctx context.Context, id, name string) error {
	return b.api.ContainerRename(ctx, id, name)
}

func (b *Backend) VerifyLimits(ctx context.Context, id string, spec daemon.ContainerSpec) error {
	value, err := b.api.ContainerInspect(ctx, id)
	if err != nil {
		return err
	}
	h := value.HostConfig
	if spec.Runtime != "" && h.Runtime != spec.Runtime {
		return errors.New("runtime did not confirm isolation runtime")
	}
	if h.Memory != int64(spec.MemoryMB)<<20 || h.MemorySwap != h.Memory || h.NanoCpus != spec.NanoCPUs || h.PidsLimit == nil || *h.PidsLimit != int64(spec.PidsLimit) || h.NetworkMode != networkMode(spec.Network) {
		return errors.New("runtime did not confirm requested deployment limits")
	}
	var f []string
	if h.Runtime == "runsc" {
		f, err = b.runscLimits(ctx, id, value)
	} else {
		var data []byte
		data, err = b.capture(ctx, id, []string{"sh", "-c", "set -eu; cat /sys/fs/cgroup/memory.max /sys/fs/cgroup/memory.swap.max /sys/fs/cgroup/pids.max /sys/fs/cgroup/cpu.max; ls /sys/class/net"})
		f = strings.Fields(string(data))
	}
	if err != nil {
		return fmt.Errorf("cannot verify cgroup v2 limits: %w", err)
	}
	if len(f) < 6 {
		return errors.New("incomplete runtime policy evidence")
	}
	memory, e1 := strconv.ParseInt(f[0], 10, 64)
	swap, e2 := strconv.ParseInt(f[1], 10, 64)
	pids, e3 := strconv.ParseInt(f[2], 10, 64)
	quota, e4 := strconv.ParseInt(f[3], 10, 64)
	period, e5 := strconv.ParseInt(f[4], 10, 64)
	if errors.Join(e1, e2, e3, e4, e5) != nil || period <= 0 || period > 1_000_000 {
		return errors.New("missing or invalid kernel resource limits")
	}
	wantQuota := spec.NanoCPUs/1_000_000_000*period + spec.NanoCPUs%1_000_000_000*period/1_000_000_000
	if memory != int64(spec.MemoryMB)<<20 || swap != 0 || pids != int64(spec.PidsLimit) || quota != wantQuota || quota <= 0 {
		return errors.New("kernel resource limits differ from policy")
	}
	if networkMode(spec.Network) == "none" && (len(f) != 6 || f[5] != "lo") {
		return errors.New("restricted network has a non-loopback interface")
	}
	return nil
}
