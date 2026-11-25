package dockerbackend

import (
	"archive/tar"
	"bytes"
	"cmp"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ruhuang2001/execlet/internal/dockerapi"
	"github.com/ruhuang2001/execlet/internal/guestprotocol"
)

type fakeDaemon struct {
	t    *testing.T
	sock string

	mu         sync.Mutex
	nextCtr    int
	nextExec   int
	containers map[string]*fakeContainer
	byName     map[string]string
	execs      map[string]*fakeExec

	creates      []dockerapi.ContainerConfig
	createNames  []string
	removed      []string
	removalURIs  []string
	kills        []string
	execCommands [][]string
	execEnvs     [][]string

	failStart     bool
	failInspect   bool
	noPidFile     bool
	missingImages map[string]bool
	pulled        []string
	pullError     string
	programFn     func(containerID string, cmd []string) (stdout, stderr string, exitCode int)
}

type fakeContainer struct {
	id                 string
	spec               dockerapi.ContainerConfig
	state              string
	labels             map[string]string
	files              map[string][]byte
	oomKilled          bool
	inspectUnavailable bool
}

type fakeExec struct {
	id          string
	containerID string
	command     []string
	stdout      string
	stderr      string
	exitCode    int
	running     bool
	started     bool
}

func newFakeDaemon(t *testing.T) *fakeDaemon {
	t.Helper()
	dir, err := os.MkdirTemp("", "ts-docker")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := path.Join(dir, "d.sock")
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeDaemon{
		t:             t,
		sock:          sock,
		containers:    make(map[string]*fakeContainer),
		byName:        make(map[string]string),
		execs:         make(map[string]*fakeExec),
		missingImages: make(map[string]bool),
	}
	srv := &http.Server{Handler: f, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return f
}

func (f *fakeDaemon) client(t *testing.T) *dockerapi.Client {
	t.Helper()
	c, err := dockerapi.Dial(t.Context(), f.sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return c
}

func (f *fakeDaemon) backend(t *testing.T) *Backend {
	t.Helper()
	return New(f.client(t))
}

func (f *fakeDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/version":
		writeJSON(w, 200, map[string]string{"ApiVersion": "1.52", "Version": "29.0.0"})
	case strings.HasPrefix(r.URL.Path, "/v1.52/"):
		f.serveV152(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeDaemon) serveV152(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/v1.52")

	switch {
	case p == "/images/create" && r.Method == http.MethodPost:
		f.pullImage(w, r)
	case p == "/containers/create" && r.Method == http.MethodPost:
		f.create(w, r)
	case p == "/containers/json" && r.Method == http.MethodGet:
		f.list(w, r)
	case strings.HasSuffix(p, "/exec") && r.Method == http.MethodPost:
		f.execCreate(w, r, strings.TrimSuffix(strings.TrimPrefix(p, "/containers/"), "/exec"))
	case strings.HasPrefix(p, "/exec/") && strings.HasSuffix(p, "/start"):
		f.execStart(w, r, strings.TrimSuffix(strings.TrimPrefix(p, "/exec/"), "/start"))
	case strings.HasPrefix(p, "/exec/") && strings.HasSuffix(p, "/json"):
		f.execInspect(w, r, strings.TrimSuffix(strings.TrimPrefix(p, "/exec/"), "/json"))
	case strings.HasPrefix(p, "/containers/") && strings.HasSuffix(p, "/archive"):
		f.archive(w, r, strings.TrimSuffix(strings.TrimPrefix(p, "/containers/"), "/archive"))
	case strings.HasPrefix(p, "/containers/"):
		f.containerOp(w, r, strings.TrimPrefix(p, "/containers/"))
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeDaemon) pullImage(w http.ResponseWriter, r *http.Request) {
	repository := r.URL.Query().Get("fromImage")
	tag := r.URL.Query().Get("tag")
	image := repository
	if strings.Contains(tag, ":") {
		image = repository + "@" + tag
	} else if tag != "" && tag != "latest" {
		image = repository + ":" + tag
	}
	if image == repository && tag == "latest" {
		image = repository + ":latest"
	}

	f.mu.Lock()
	f.pulled = append(f.pulled, image)
	errMessage := f.pullError
	if errMessage == "" {
		delete(f.missingImages, image)
	}
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	if errMessage != "" {
		writeJSONLine(w, map[string]string{"error": errMessage})
		return
	}
	writeJSONLine(w, map[string]string{"status": "Pulling from library/" + repository})
	writeJSONLine(w, map[string]string{"status": "Status: Downloaded newer image for " + image})
}

func writeJSONLine(w http.ResponseWriter, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	_, _ = w.Write(append(data, '\n'))
}

func (f *fakeDaemon) containerOp(w http.ResponseWriter, r *http.Request, tail string) {
	action := ""
	id := tail
	if i := strings.LastIndex(tail, "/"); i >= 0 {
		id, action = tail[:i], tail[i+1:]
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.container(w, id)
	if !ok {
		return
	}
	lookup := id
	id = c.id
	switch {
	case action == "json" && r.Method == http.MethodGet:
		if c.inspectUnavailable {
			writeError(w, 503, "inspect unavailable")
			return
		}
		value := dockerapi.ContainerInspect{ID: id, Name: "/" + lookup, HostConfig: dockerapi.HostConfig{Runtime: cmp.Or(c.spec.HostConfig.Runtime, "runc")}, State: dockerapi.ContainerState{Status: c.state, Running: c.state == "running", Paused: c.state == "paused", OOMKilled: c.oomKilled}}
		value.Config.Labels = c.labels
		writeJSON(w, http.StatusOK, value)
	case action == "start" && r.Method == http.MethodPost:
		if f.failStart {
			writeError(w, 500, "cannot start: simulated failure")
			return
		}
		w.WriteHeader(204)
	case action == "pause" && r.Method == http.MethodPost:
		if c.state == "running" {
			c.state = "paused"
		}
		w.WriteHeader(204)
	case action == "unpause" && r.Method == http.MethodPost:
		if c.state == "paused" {
			c.state = "running"
		}
		w.WriteHeader(204)
	case action == "kill" && r.Method == http.MethodPost:
		f.kills = append(f.kills, id)
		c.state = "exited"
		w.WriteHeader(204)
	case action == "" && r.Method == http.MethodDelete:
		f.removed = append(f.removed, id)
		f.removalURIs = append(f.removalURIs, r.URL.RequestURI())
		for name, owner := range f.byName {
			if owner == id {
				delete(f.byName, name)
			}
		}
		delete(f.containers, id)
		w.WriteHeader(204)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeDaemon) create(w http.ResponseWriter, r *http.Request) {
	var cfg dockerapi.ContainerConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	name := r.URL.Query().Get("name")
	if _, held := f.byName[name]; held {
		writeError(w, 409, "Conflict. The container name "+name+" is already in use")
		return
	}
	if f.missingImages[cfg.Image] {
		writeError(w, 404, "No such image: "+cfg.Image)
		return
	}
	f.creates = append(f.creates, cfg)
	f.createNames = append(f.createNames, name)
	f.nextCtr++
	id := fmt.Sprintf("ctr_%05d", f.nextCtr)
	f.containers[id] = &fakeContainer{
		id:     id,
		spec:   cfg,
		state:  "running",
		labels: cfg.Labels,
		files:  make(map[string][]byte),
	}
	if name != "" {
		f.byName[name] = id
	}
	writeJSON(w, 201, map[string]string{"Id": id})
}

func (f *fakeDaemon) resolveContainer(id string) string {
	if mapped, ok := f.byName[id]; ok {
		return mapped
	}
	return id
}

func (f *fakeDaemon) container(w http.ResponseWriter, id string) (*fakeContainer, bool) {
	id = f.resolveContainer(id)
	c := f.containers[id]
	if c == nil {
		writeError(w, 404, "No such container: "+id)
		return nil, false
	}
	return c, true
}

func (f *fakeDaemon) list(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var filters map[string][]string
	if raw := r.URL.Query().Get("filters"); raw != "" {
		_ = json.Unmarshal([]byte(raw), &filters)
	}
	out := make([]map[string]any, 0, len(f.containers))
	for _, c := range f.containers {
		if !matchesID(c.id, filters["id"]) || !matchesLabel(c.labels, filters["label"]) {
			continue
		}
		out = append(out, map[string]any{
			"Id": c.id, "State": c.state, "Image": c.spec.Image, "Labels": c.labels,
		})
	}
	writeJSON(w, 200, out)
}

func matchesID(id string, want []string) bool {
	if len(want) == 0 {
		return true
	}
	for _, w := range want {
		if strings.HasPrefix(id, w) {
			return true
		}
	}
	return false
}

func matchesLabel(labels map[string]string, want []string) bool {
	for _, pair := range want {
		key, value, hasValue := strings.Cut(pair, "=")
		got, ok := labels[key]
		if !ok || (hasValue && got != value) {
			return false
		}
	}
	return true
}

func (f *fakeDaemon) execCreate(w http.ResponseWriter, r *http.Request, containerID string) {
	var cfg struct {
		Cmd []string `json:"Cmd"`
		Env []string `json:"Env"`
	}
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.container(w, containerID)
	if !ok {
		return
	}
	f.nextExec++
	ex := &fakeExec{
		id:          fmt.Sprintf("exe_%05d", f.nextExec),
		containerID: containerID,
		command:     cfg.Cmd,
	}
	actual := cfg.Cmd
	if _, _, _, realCmd, wrapped := unwrap(cfg.Cmd); wrapped {
		c.files[cfg.Cmd[3]] = []byte("4242\n")
		ex.command = realCmd
		actual = realCmd
	}
	ex.stdout, ex.stderr, ex.exitCode = f.programLocked(containerID, c, actual)
	f.execs[ex.id] = ex
	f.execCommands = append(f.execCommands, cfg.Cmd)
	f.execEnvs = append(f.execEnvs, cfg.Env)
	writeJSON(w, 201, map[string]string{"Id": ex.id})
}

func (f *fakeDaemon) programLocked(containerID string, c *fakeContainer, cmd []string) (string, string, int) {
	if f.programFn != nil {
		return f.programFn(containerID, cmd)
	}
	if len(cmd) == 3 && cmd[0] == "/bin/sh" && cmd[2] == dockerapi.OOMScript {
		return "low 0\nhigh 0\nmax 0\noom 0\noom_kill 5\n", "", 0
	}
	if len(cmd) == 5 && cmd[0] == "/bin/sh" && cmd[2] == writeVerifyScript {
		for _, name := range []string{cmd[4], "/" + cmd[4]} {
			if data, ok := c.files[name]; ok {
				return fmt.Sprintf("okfile\x00root:0\n---\nroot:0\x00B\x00%s\x00\x0081a4:%d:0:0:0\n\x00\x00", path.Base(name), len(data)), "", 0
			}
		}
		return "missing", "", 0
	}
	if len(cmd) == 5 && cmd[0] == "/bin/sh" && cmd[2] == writeOwnershipScript {
		return "uid=0\x00gid=0\x00", "", 0
	}
	if len(cmd) == 3 && cmd[0] == "/bin/sh" && strings.HasPrefix(cmd[2], "cat ") {
		path := strings.TrimSuffix(strings.TrimPrefix(cmd[2], "cat "), " 2>/dev/null")
		if f.noPidFile {
			return "", "", 1
		}
		if data, ok := c.files[path]; ok {
			return string(data), "", 0
		}
		return "", "", 1
	}
	return "", "", 0
}

func (f *fakeDaemon) setProgram(fn func(containerID string, cmd []string) (stdout, stderr string, exitCode int)) {
	f.mu.Lock()
	f.programFn = fn
	f.mu.Unlock()
}

func (f *fakeDaemon) execStart(w http.ResponseWriter, _ *http.Request, execID string) {
	f.mu.Lock()
	ex := f.execs[execID]
	f.mu.Unlock()
	if ex == nil {
		writeError(w, 404, "No such exec: "+execID)
		return
	}
	if len(ex.command) == 5 && ex.command[0] == protocol.Binary && ex.command[1] == "transfer" && ex.command[2] == "put" {
		serveTransferPut(f.t, w, ex)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
	w.WriteHeader(200)
	if ex.stdout != "" {
		writeFrame(w, 1, ex.stdout)
	}
	if ex.stderr != "" {
		writeFrame(w, 2, ex.stderr)
	}
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
	f.mu.Lock()
	ex.running = false
	ex.started = true
	f.mu.Unlock()
}

func (f *fakeDaemon) execInspect(w http.ResponseWriter, _ *http.Request, execID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failInspect {
		writeError(w, 500, "simulated inspect failure")
		return
	}
	ex := f.execs[execID]
	if ex == nil {
		writeError(w, 404, "No such exec: "+execID)
		return
	}
	writeJSON(w, 200, map[string]any{
		"ID": ex.id, "Running": ex.running, "ExitCode": ex.exitCode, "Pid": 4242,
	})
}

func (f *fakeDaemon) archive(w http.ResponseWriter, r *http.Request, containerID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.container(w, containerID)
	if !ok {
		return
	}
	p := r.URL.Query().Get("path")
	switch r.Method {
	case http.MethodGet:
		data, exists := c.files[p]
		if !exists {
			prefix := strings.TrimSuffix(p, "/") + "/"
			var children []string
			for name := range c.files {
				if strings.HasPrefix(name, prefix) {
					children = append(children, name)
				}
			}
			if len(children) > 0 {
				slices.Sort(children)
				var buf bytes.Buffer
				tw := tar.NewWriter(&buf)
				base := path.Base(p)
				_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: base + "/", Mode: 0o755})
				for _, child := range children {
					rel := path.Join(base, strings.TrimPrefix(child, prefix))
					body := c.files[child]
					_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: rel, Mode: 0o644, Size: int64(len(body))})
					_, _ = tw.Write(body)
				}
				_ = tw.Close()
				w.Header().Set("Content-Type", "application/x-tar")
				_, _ = w.Write(buf.Bytes())
				return
			}
			writeError(w, 404, "Could not find the file "+p+" in container "+containerID)
			return
		}
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		_ = tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg, Name: path.Base(p), Mode: 0o644, Size: int64(len(data)),
		})
		_, _ = tw.Write(data)
		_ = tw.Close()
		w.Header().Set("Content-Type", "application/x-tar")
		_, _ = w.Write(buf.Bytes())
	case http.MethodPut:
		tr := tar.NewReader(r.Body)
		for {
			hdr, err := tr.Next()
			if err != nil {
				break
			}
			data, err := io.ReadAll(tr)
			if err != nil {
				writeError(w, 400, err.Error())
				return
			}
			c.files[path.Join(p, hdr.Name)] = data
		}
		w.WriteHeader(200)
	}
}

func writeFrame(w io.Writer, stream byte, payload string) {
	var hdr [8]byte
	hdr[0] = stream
	binary.BigEndian.PutUint32(hdr[4:], uint32(len(payload)))
	_, _ = w.Write(hdr[:])
	_, _ = io.WriteString(w, payload)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"message": message})
}

func unwrap(cmd []string) (pidPath, dir string, env, actual []string, ok bool) {
	if len(cmd) < 7 || cmd[0] != "/bin/sh" || cmd[1] != "-c" || cmd[2] != execWrapper {
		return "", "", nil, nil, false
	}
	n, err := strconv.Atoi(cmd[5])
	if err != nil || n < 0 || len(cmd) < 7+n {
		return "", "", nil, nil, false
	}
	if cmd[6] == "1" {
		if len(cmd) < 9+n || cmd[7] != cmd[5] || cmd[8+n] != "command" {
			return "", "", nil, nil, false
		}
		env = slices.Clone(cmd[8 : 8+n])
		slices.Reverse(env)
		return cmd[3], cmd[4], env, cmd[9+n:], true
	}
	return cmd[3], cmd[4], cmd[7 : 7+n], cmd[7+n:], cmd[6] == "0"
}

func (f *fakeDaemon) file(containerID, path string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.containers[containerID]; c != nil {
		return c.files[path]
	}
	return nil
}

func (f *fakeDaemon) lastExecEnv() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.execEnvs) == 0 {
		return nil
	}
	return f.execEnvs[len(f.execEnvs)-1]
}

func (f *fakeDaemon) lastExecCommand() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.execCommands) == 0 {
		return nil
	}
	return f.execCommands[len(f.execCommands)-1]
}

func (f *fakeDaemon) removedContainers() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.removed...)
}

var _ = strconv.Itoa
