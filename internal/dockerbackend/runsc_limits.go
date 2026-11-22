package dockerbackend

import (
	"context"
	"errors"
	"fmt"
	"github.com/ruhuang2001/tiny-sandbox/internal/dockerapi"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func (b *Backend) runscLimits(ctx context.Context, id string, c dockerapi.ContainerInspect) ([]string, error) {
	if b.cgroupRoot == "" || len(id) != 64 || c.ID != id || c.State.Pid <= 0 {
		return nil, errors.New("runsc policy needs local host cgroup evidence")
	}
	membership, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(c.State.Pid), "cgroup"))
	if err != nil {
		return nil, err
	}
	var dir string
	for line := range strings.Lines(string(membership)) {
		path, ok := strings.CutPrefix(strings.TrimSpace(line), "0::/")
		if ok && (filepath.Base(path) == "docker-"+id+".scope" || filepath.Base(path) == id) {
			dir = path
			break
		}
	}
	if dir == "" {
		return nil, errors.New("runsc process has no matching cgroup v2 scope")
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
