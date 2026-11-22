package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ruhuang2001/tiny-sandbox/internal/filearchive"
)

type workspaceDelta struct {
	hashes            map[string]string
	archive           bytes.Buffer
	dirs              map[string]bool
	uploaded, skipped int
	total             int64
}

func (s *sandboxTools) manifestPath(id string) string { return filepath.Join(s.cache, id+".json") }

func (s *sandboxTools) loadManifest(id string) (map[string]string, error) {
	state := map[string]string{}
	data, err := os.ReadFile(s.manifestPath(id))
	if errors.Is(err, fs.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	for name := range state {
		if name == "." || !fs.ValidPath(name) || strings.Contains(name, "\\") {
			return nil, fmt.Errorf("invalid workspace manifest path %q", name)
		}
	}
	return state, nil
}

func (s *sandboxTools) saveManifest(id string, state map[string]string) error {
	if err := os.MkdirAll(s.cache, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(s.cache, ".sync-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	err = json.MarshalWrite(file, state)
	if err := errors.Join(err, file.Close()); err != nil {
		return err
	}
	return os.Rename(file.Name(), s.manifestPath(id))
}

type ignoreRule struct {
	pattern                  string
	rooted, dirOnly, include bool
}

func (s *sandboxTools) ignoreRules() ([]ignoreRule, error) {
	info, err := s.workspace.Lstat(".gitignore")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil
	}
	f, err := s.workspace.Open(".gitignore")
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 1<<20 {
		return nil, errors.New("payload_too_large: root .gitignore exceeds 1 MiB")
	}
	var rules []ignoreRule
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		pattern, include := strings.CutPrefix(line, "!")
		pattern, dirOnly := strings.CutSuffix(pattern, "/")
		rooted := strings.Contains(pattern, "/")
		pattern = strings.TrimPrefix(pattern, "/")
		if pattern != "" {
			rules = append(rules, ignoreRule{pattern, rooted, dirOnly, include})
		}
	}
	return rules, nil
}

func ignored(name string, isDir bool, rules []ignoreRule) bool {
	base := path.Base(name)
	switch base {
	case ".git", ".venv", "node_modules", ".env":
		return true
	}
	exclude := false
	for _, rule := range rules {
		if rule.dirOnly && !isDir {
			continue
		}
		candidate := base
		if rule.rooted {
			candidate = name
		}
		if ok, _ := path.Match(rule.pattern, candidate); ok {
			exclude = !rule.include
		}
	}
	return exclude
}

func (s *sandboxTools) scan(ctx context.Context, previous map[string]string, reset bool) (*workspaceDelta, error) {
	rules, err := s.ignoreRules()
	if err != nil {
		return nil, err
	}
	d := &workspaceDelta{hashes: map[string]string{}, dirs: map[string]bool{}}
	writer := tar.NewWriter(&d.archive)
	err = fs.WalkDir(s.workspace.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		if ignored(name, entry.IsDir(), rules) {
			d.skipped++
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			d.skipped++
			return nil
		}
		return s.addWorkspaceFile(d, writer, name, previous[name], reset)
	})
	if err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	if d.uploaded != 0 {
		if _, err := filearchive.Parse(d.archive.Bytes()); err != nil {
			if e, ok := errors.AsType[*filearchive.Error](err); ok && e.TooLarge {
				return nil, fmt.Errorf("payload_too_large: %w", err)
			}
			return nil, err
		}
	}
	return d, nil
}

func (s *sandboxTools) addWorkspaceFile(d *workspaceDelta, w *tar.Writer, name, previous string, reset bool) error {
	f, err := s.workspace.Open(name)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("workspace file changed type during sync: %s", name)
	}
	if info.Size() > filearchive.MaxExtractedBytes {
		return errors.New("payload_too_large: workspace file exceeds 16 MiB extracted limit")
	}
	digest := sha256.New()
	data, err := io.ReadAll(io.LimitReader(io.TeeReader(f, digest), filearchive.MaxExtractedBytes+1))
	if err != nil {
		return err
	}
	hash := hex.EncodeToString(digest.Sum(nil))
	if info.Mode()&0111 != 0 {
		hash += ":x"
	}
	d.hashes[name] = hash
	if !reset && previous == hash {
		d.skipped++
		return nil
	}
	d.total += int64(len(data))
	d.uploaded++
	if d.total > filearchive.MaxExtractedBytes {
		return errors.New("payload_too_large: changed files exceed 16 MiB extracted limit")
	}
	if d.uploaded > filearchive.MaxEntries {
		return errors.New("payload_too_large: changed files exceed 4096 archive members")
	}
	mode := int64(0644)
	if info.Mode()&0111 != 0 {
		mode = 0755
	}
	if err := w.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(data)), ModTime: info.ModTime(), Format: tar.FormatPAX}); err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	if dir := path.Dir(name); dir != "." {
		d.dirs[dir] = true
	}
	if d.archive.Len() > filearchive.MaxBytes {
		return errors.New("payload_too_large: archive exceeds 32 MiB encoded limit")
	}
	return nil
}

func (s *sandboxTools) syncWorkspace(ctx context.Context, in syncInput) (toolOutput, error) {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	previous, err := s.loadManifest(in.SandboxID)
	if err != nil {
		return toolOutput{}, err
	}
	d, err := s.scan(ctx, previous, in.Reset)
	if err != nil {
		return toolOutput{}, err
	}
	var removed []string
	for name := range previous {
		if _, ok := d.hashes[name]; !ok {
			removed = append(removed, name)
		}
	}
	slices.Sort(removed)
	if d.uploaded != 0 || len(removed) != 0 {
		pending := maps.Clone(previous)
		for name, hash := range d.hashes {
			if hash != previous[name] || in.Reset {
				pending[name] = ""
			}
		}
		for _, name := range removed {
			pending[name] = ""
		}
		if err := s.saveManifest(in.SandboxID, pending); err != nil {
			return toolOutput{}, err
		}
	}
	if err := s.guestExec(ctx, in.SandboxID, syncCommand("mkdir", guestWorkspace)); err != nil {
		return toolOutput{}, err
	}
	if err := s.applyWorkspaceDelta(ctx, in.SandboxID, d, removed); err != nil {
		return toolOutput{}, err
	}
	if err := s.saveManifest(in.SandboxID, d.hashes); err != nil {
		return toolOutput{}, err
	}
	value := map[string]any{"uploaded": d.uploaded, "deleted": len(removed), "skipped": d.skipped, "total_bytes": d.total}
	return toolOutput{fmt.Sprintf("Uploaded %d files (%d bytes), deleted %d, skipped %d.", d.uploaded, d.total, len(removed), d.skipped), value}, nil
}

func (s *sandboxTools) guestExec(ctx context.Context, id string, command []string) error {
	ex, err := s.client.Exec(ctx, id, command)
	if err != nil && ex != nil && ex.Stderr != "" {
		return fmt.Errorf("%w: %s", err, ex.Stderr)
	}
	return err
}

const workspaceMutation = `set -eu
action=$1
shift
walk() {
 cd /
 rest=${1#/}
 expected=
 while [ -n "$rest" ]; do
  component=${rest%%/*}
  if [ "$component" = "$rest" ]; then rest=; else rest=${rest#*/}; fi
  expected=$expected/$component
  if [ -L "./$component" ]; then echo "symlink in sync path: $expected" >&2; exit 1; fi
  if [ ! -e "./$component" ]; then
   if [ "$action" != mkdir ]; then exit 0; fi
   mkdir -- "./$component"
  fi
  cd -P "./$component"
  if [ "$PWD" != "$expected" ]; then echo "sync directory moved: $expected" >&2; exit 1; fi
 done
}
case $action in
 mkdir) for dir do (walk "$dir"); done ;;
 remove) parent=$1; shift; walk "$parent"; rm -f -- "$@" ;;
 prune) for dir do (walk "${dir%/*}"; rmdir -- "${dir##*/}" 2>/dev/null || :); done ;;
esac`

func syncCommand(action string, items ...string) []string {
	return append([]string{"sh", "-c", workspaceMutation, "sync-workspace", action}, items...)
}

func (s *sandboxTools) applyWorkspaceDelta(ctx context.Context, id string, d *workspaceDelta, removed []string) error {
	groups := map[string][]string{}
	parents := map[string]bool{}
	for _, name := range removed {
		parent := path.Dir(name)
		groups[parent] = append(groups[parent], path.Base(name))
		for dir := parent; dir != "."; dir = path.Dir(dir) {
			parents[dir] = true
		}
	}
	for _, parent := range slices.Sorted(maps.Keys(groups)) {
		for batch := range slices.Chunk(groups[parent], 100) {
			if err := s.guestExec(ctx, id, syncCommand("remove", append([]string{path.Join(guestWorkspace, parent)}, batch...)...)); err != nil {
				return err
			}
		}
	}
	dirs := slices.Sorted(maps.Keys(parents))
	slices.Reverse(dirs)
	if err := s.syncDirectories(ctx, id, "prune", dirs); err != nil {
		return err
	}
	if err := s.syncDirectories(ctx, id, "mkdir", slices.Sorted(maps.Keys(d.dirs))); err != nil {
		return err
	}
	if d.uploaded == 0 {
		return nil
	}
	return s.client.WriteArchive(ctx, id, guestWorkspace, d.archive.Bytes())
}

func (s *sandboxTools) syncDirectories(ctx context.Context, id, action string, dirs []string) error {
	for batch := range slices.Chunk(dirs, 100) {
		paths := make([]string, 0, len(batch))
		for _, dir := range batch {
			paths = append(paths, path.Join(guestWorkspace, dir))
		}
		if err := s.guestExec(ctx, id, syncCommand(action, paths...)); err != nil {
			return err
		}
	}
	return nil
}
