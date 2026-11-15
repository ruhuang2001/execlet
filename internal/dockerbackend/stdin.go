package dockerbackend

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"
	"time"

	"github.com/ruhuang2001/tiny-sandbox/internal/dockerapi"
)

// stageStdin keeps input out of exec argv and the retained execution. The
// private directory is created by the image's user; archive bytes are readable
// only through that directory. The wrapper unlinks them as soon as it opens fd0.
func (b *Backend) stageStdin(ctx context.Context, containerID, input string) (string, error) {
	if input == "" {
		return "", nil
	}
	dir := "/tmp/tiny-sandbox-stdin." + rand.Text()
	// Keep a cleanup target even when the directory was created but the
	// creation response is lost. mkdir never reuses an existing directory.
	_, err := b.capture(ctx, containerID, []string{"/bin/sh", "-c",
		`test -x /bin/mkdir && test -x /bin/rm && test -x /bin/rmdir || exit 125; umask 077; /bin/mkdir -m 700 -- "$1"`, "stdin-stage", dir})
	if err != nil {
		b.cleanupStdin(containerID, dir)
		return "", fmt.Errorf("dockerbackend: stage stdin (requires writable /tmp and /bin/mkdir, /bin/rm, /bin/rmdir): %w", err)
	}
	body, err := dockerapi.BuildFileTar("input", []byte(input))
	if err == nil {
		err = b.api.ArchivePut(ctx, containerID, dir, body)
	}
	if err != nil {
		b.cleanupStdin(containerID, dir)
		return "", fmt.Errorf("dockerbackend: stage stdin: %w", err)
	}
	return dir, nil
}

func validStdinDir(dir string) bool {
	suffix, ok := strings.CutPrefix(dir, "/tmp/tiny-sandbox-stdin.")
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

// withStdin changes only the wrapper's positional argument layout. All input
// stays in the archive; none is evaluated by the shell or placed in argv.
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

// cleanupStdin is also used if exec creation or startup fails before the
// wrapper runs. It never recursively removes a directory. Cleanup is bounded
// and best effort; container removal is the final fallback if the runtime fails.
func (b *Backend) cleanupStdin(containerID, dir string) {
	if dir == "" || !validStdinDir(dir) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = b.capture(ctx, containerID, []string{"/bin/sh", "-c", `/bin/rm -f -- "$1/input"; /bin/rmdir -- "$1" 2>/dev/null`, "stdin-cleanup", dir})
}
