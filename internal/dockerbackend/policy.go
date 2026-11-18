package dockerbackend

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
)

// VerifyLimits fails closed if Docker did not retain the requested limits.
// None means no external interface; bridge deliberately makes no egress claim.
func (b *Backend) VerifyLimits(ctx context.Context, id string, spec daemon.ContainerSpec) error {
	value, err := b.api.ContainerInspect(ctx, id)
	if err != nil {
		return err
	}
	h := value.HostConfig
	if h.Memory != int64(spec.MemoryMB)<<20 || h.MemorySwap != h.Memory || h.NanoCpus != spec.NanoCPUs || h.PidsLimit == nil || *h.PidsLimit != int64(spec.PidsLimit) || h.NetworkMode != string(spec.Network) {
		return errors.New("runtime did not confirm requested deployment limits")
	}
	// Inspect alone can echo settings even when the kernel lacks controllers.
	// Policy mode requires readable cgroup v2 enforcement, not a warning.
	data, err := b.capture(ctx, id, []string{"sh", "-c", "set -eu; cat /sys/fs/cgroup/memory.max /sys/fs/cgroup/memory.swap.max /sys/fs/cgroup/pids.max /sys/fs/cgroup/cpu.max; ls /sys/class/net"})
	if err != nil {
		return fmt.Errorf("cannot verify cgroup v2 limits: %w", err)
	}
	f := strings.Fields(string(data))
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
	if spec.Network == daemon.NetworkNone && (len(f) != 6 || f[5] != "lo") {
		return errors.New("restricted network has a non-loopback interface")
	}
	return nil
}
