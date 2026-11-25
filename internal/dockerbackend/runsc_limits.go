package dockerbackend

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ruhuang2001/execlet/internal/dockerapi"
)

func containerCgroupDir(id string, c dockerapi.ContainerInspect) (string, error) {
	if !fullContainerID(id) || c.ID != id || c.State.Pid <= 0 {
		return "", errors.New("cannot identify the container's host cgroup")
	}
	membership, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(c.State.Pid), "cgroup"))
	if err != nil {
		return "", err
	}
	return cgroupV2Dir(id, string(membership))
}

func cgroupV2Dir(id, membership string) (string, error) {
	for line := range strings.Lines(membership) {
		dir, ok := strings.CutPrefix(strings.TrimSpace(line), "0::/")
		if ok && filepath.IsLocal(dir) && filepath.Clean(dir) == dir && (filepath.Base(dir) == "docker-"+id+".scope" || filepath.Base(dir) == id) {
			return dir, nil
		}
	}
	return "", errors.New("process has no matching cgroup v2 scope")
}

func (b *Backend) runscLimits(ctx context.Context, id string, c dockerapi.ContainerInspect) ([]string, error) {
	if b.cgroupRoot == "" {
		return nil, errors.New("runsc policy needs local host cgroup evidence")
	}
	dir, err := containerCgroupDir(id, c)
	if err != nil {
		return nil, err
	}
	scope, err := os.OpenRoot(b.cgroupRoot)
	if err != nil {
		return nil, err
	}
	defer func() { _ = scope.Close() }()
	var fields []string
	for _, name := range []string{"memory.max", "memory.swap.max", "pids.max", "cpu.max"} {
		data, err := scope.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		fields = append(fields, strings.Fields(string(data))...)
	}
	if len(fields) != 5 {
		return nil, errors.New("incomplete host cgroup limits")
	}
	data, err := b.capture(ctx, id, []string{"cat", "/proc/net/dev"})
	if err != nil {
		return nil, fmt.Errorf("cannot read runsc network interfaces: %w", err)
	}
	for line := range strings.Lines(string(data)) {
		name, _, ok := strings.Cut(line, ":")
		if ok {
			fields = append(fields, strings.TrimSpace(name))
		}
	}
	return fields, nil
}
