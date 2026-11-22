package dockerapi

import (
	"fmt"
	"strconv"
	"strings"
)

const OOMScript = `if [ -r /sys/fs/cgroup/memory.events ]; then cat /sys/fs/cgroup/memory.events;
elif [ -r /sys/fs/cgroup/memory/memory.oom_control ]; then cat /sys/fs/cgroup/memory/memory.oom_control;
else exit 3; fi`

func ParseOOMKill(data []byte) (int64, error) {
	for line := range strings.Lines(string(data)) {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "oom_kill" {
			continue
		}
		n, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("dockerapi: parse oom_kill %q: %w", fields[1], err)
		}
		return n, nil
	}
	return 0, fmt.Errorf("dockerapi: no oom_kill counter in %d bytes", len(data))
}
