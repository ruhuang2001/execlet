package dockerbackend

import (
	"os"
	"path/filepath"

	"github.com/ruhuang2001/execlet/internal/dockerapi"
)

func localOOMKillCount(root, id string) (int64, bool) {
	if root == "" || len(id) != 64 {
		return 0, false
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return 0, false
		}
	}
	for _, dir := range []string{"system.slice/docker-" + id + ".scope", "docker/" + id} {
		data, err := os.ReadFile(filepath.Join(root, dir, "memory.events"))
		if err == nil {
			if n, err := dockerapi.ParseOOMKill(data); err == nil && n >= 0 {
				return n, true
			}
		}
	}
	return 0, false
}
