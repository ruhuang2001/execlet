package dockerbackend

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"

	"github.com/ruhuang2001/execlet/internal/dockerapi"
)

func (b *Backend) stageStdin(ctx context.Context, containerID, input string) (string, error) {
	if input == "" {
		return "", nil
	}
	dir := "/tmp/execlet-stdin." + rand.Text()
	_, err := b.captureControl(ctx, containerID, []string{"/bin/sh", "-c",
		`test -x /bin/mkdir && test -x /bin/rm && test -x /bin/rmdir || exit 125; umask 077; /bin/mkdir -m 700 -- "$1"`, "stdin-stage", dir})
	if err != nil {
		b.cleanupStdin(containerID, dir)
		return "", fmt.Errorf("dockerbackend: stage stdin (requires writable /tmp and /bin/mkdir, /bin/rm, /bin/rmdir): %w", err)
	}
	body, err := dockerapi.BuildFileTar("input", []byte(input))
	if err == nil {
		err = b.archivePut(ctx, containerID, dir, body)
	}
	if err != nil {
		b.cleanupStdin(containerID, dir)
		return "", fmt.Errorf("dockerbackend: stage stdin: %w", err)
	}
	return dir, nil
}

func validStdinDir(dir string) bool {
	suffix, ok := strings.CutPrefix(dir, "/tmp/execlet-stdin.")
	if !ok || len(suffix) < 26 || len(suffix) > 128 {
		return false
	}
	for _, c := range suffix {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

func withStdin(argv []string, dir string) []string {
	argv[2] = `printf %s $$ > "$0" || exit 125
exec 0<"$1/input" || exit 125
/bin/rm -f -- "$1/input" || exit 125
/bin/rmdir -- "$1" || exit 125
shift
` + execWrapper
	out := make([]string, 0, len(argv)+1)
	out = append(out, argv[:4]...)
	out = append(out, dir)
	return append(out, argv[4:]...)
}

func (b *Backend) cleanupStdin(containerID, dir string) {
	if dir == "" || !validStdinDir(dir) {
		return
	}
	b.cleanupAfterExit(&execFiles{containerID: containerID, stdinDir: dir})
}
