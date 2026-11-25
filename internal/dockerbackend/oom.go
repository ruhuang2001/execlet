package dockerbackend

import (
	"os"
	"path/filepath"

	"github.com/ruhuang2001/execlet/internal/dockerapi"
)

func fullContainerID(id string) bool {
	if len(id) != 64 {
		return false
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func localOOMKillCount(root, id string) (int64, bool) {
	if root == "" || !fullContainerID(id) {
		return 0, false
	}
	for _, dir := range []string{"system.slice/docker-" + id + ".scope", "docker/" + id} {
		if n, ok := readHostOOM(root, dir); ok {
			return n, true
		}
	}
	return 0, false
}

func readHostOOM(root, dir string) (int64, bool) {
	scope, err := os.OpenRoot(root)
	if err != nil {
		return 0, false
	}
	defer func() { _ = scope.Close() }()
	data, err := scope.ReadFile(filepath.Join(dir, "memory.events"))
	if err != nil {
		return 0, false
	}
	n, err := dockerapi.ParseOOMKill(data)
	return n, err == nil && n >= 0
}
