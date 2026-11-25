package daemon

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type volumeDiskStub struct {
	provision func(Volume) error
	verify    func(Volume) error
	reclaim   func(Volume) error
}

func (d volumeDiskStub) create(v Volume) error { return d.provision(v) }
func (d volumeDiskStub) ensure(v Volume) error { return d.verify(v) }
func (d volumeDiskStub) remove(v Volume) error { return d.reclaim(v) }
func (volumeDiskStub) path(id string) string   { return "/test/" + id }

func volumeStore(t *testing.T, disk volumeDiskStub, limit int) *Sandboxes {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	j, err := OpenJournal(dir, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	s := New(nil, WithCleanupInterval(time.Millisecond*10))
	s.journal = j
	s.volumes = &volumes{journal: j, disk: disk, maxSize: minVolumeBytes, limit: limit, records: make(map[string]volumeRecord), holder: make(map[string]string)}
	return s
}

func TestVolumePartialCreateRecoversWithoutRestart(t *testing.T) {
	var offline atomic.Bool
	offline.Store(true)
	var partial Volume
	checkDisk := func(Volume) error {
		if offline.Load() {
			return errors.New("disk unavailable")
		}
		return nil
	}
	s := volumeStore(t, volumeDiskStub{provision: func(v Volume) error { partial = v; return checkDisk(v) }, reclaim: checkDisk}, 1)
	req := CreateVolumeRequest{SizeBytes: minVolumeBytes}
	if _, err := s.CreateVolume(req); CodeOf(err) != CodeUnavailable {
		t.Fatalf("failed provisioning: %v", err)
	}
	var record volumeRecord
	if err := s.journal.read(partial.ID+".json", &record); err != nil || record.State != "creating" {
		t.Fatalf("rollback intent: %+v, %v", record, err)
	}
	if list, err := s.ListVolumes(); err != nil || len(list) != 0 {
		t.Fatalf("unfinished volume visible: %v, %v", list, err)
	}
	if _, err := s.CreateVolume(req); CodeOf(err) != CodeRateLimited {
		t.Fatalf("unconfirmed cleanup released quota: %v", err)
	}
	offline.Store(false)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, err := s.CreateVolume(req)
		if err == nil {
			break
		}
		if CodeOf(err) != CodeRateLimited || time.Now().After(deadline) {
			t.Fatalf("quota did not recover: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	if err := s.journal.read(record.ID+".json", &record); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial record survived confirmed cleanup: %v", err)
	}
}

func TestSlowVolumeIOAllowsIndependentWork(t *testing.T) {
	for _, op := range []string{"create", "bind", "delete"} {
		t.Run(op, func(t *testing.T) {
			entered, proceed := make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(proceed) })
			defer unblock()
			var enabled atomic.Bool
			var calls atomic.Int32
			gate := func(Volume) error {
				if enabled.Load() && calls.Add(1) == 1 {
					close(entered)
					<-proceed
				}
				return nil
			}
			ok := func(Volume) error { return nil }
			disk := volumeDiskStub{ok, ok, ok}
			switch op {
			case "create":
				disk.provision = gate
			case "bind":
				disk.verify = gate
			case "delete":
				disk.reclaim = gate
			}
			s := volumeStore(t, disk, 3)
			req := CreateVolumeRequest{SizeBytes: minVolumeBytes}
			a, err := s.CreateVolume(req)
			if err != nil {
				t.Fatal(err)
			}
			b, err := s.CreateVolume(req)
			if err != nil {
				t.Fatal(err)
			}
			enabled.Store(true)
			done := make(chan error, 1)
			go func() {
				var err error
				switch op {
				case "create":
					_, err = s.CreateVolume(req)
				case "bind":
					_, err = s.volumes.bind("sandbox", []VolumeMount{{VolumeID: a.ID, Path: "/mnt/a"}})
				case "delete":
					err = s.DeleteVolume(a.ID)
				}
				done <- err
			}()
			<-entered
			independent := make(chan error, 1)
			go func() {
				if _, err := s.GetVolume(b.ID); err != nil {
					independent <- err
					return
				}
				s.confirmRemoval("unrelated", "")
				independent <- s.DeleteVolume(b.ID)
			}()
			select {
			case err := <-independent:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("unrelated volume or lifecycle operation blocked")
			}
			if op == "bind" && CodeOf(s.DeleteVolume(a.ID)) != CodeVolumeInUse {
				t.Fatal("pending bind was not exclusive")
			}
			if op == "delete" && CodeOf(s.DeleteVolume(a.ID)) != CodeUnavailable {
				t.Fatal("allowed concurrent removal")
			}
			unblock()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRawVolumeContract(t *testing.T) {
	s := volumeStore(t, volumeDiskStub{}, 1)
	h := NewHandler(s, "test")
	call := func(method, path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body)))
		return w
	}
	for _, option := range []string{``, `,"read_only":true`, `,"read_only":false`, `,"read_only":null`, `,"read_only":"false"`} {
		w := call("POST", "/v1/sandboxes", `{"mounts":[{"volume_id":"vol_1234567890abcdef","path":"/mnt/work"`+option+`}]}`)
		want := 404
		if strings.HasSuffix(option, "null") || strings.HasSuffix(option, `"false"`) {
			want = 400
		}
		if w.Code != want {
			t.Errorf("option %q: got %d %s, want %d", option, w.Code, w.Body, want)
		}
	}
	s.journal.failure = errors.New("disk offline")
	if w := call("GET", "/v1/capabilities", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"volumes.v1"`) {
		t.Errorf("configured capability: %d %s", w.Code, w.Body)
	}
	if w := call("GET", "/v1/volumes", ""); w.Code != 503 || !strings.Contains(w.Body.String(), `"unavailable"`) {
		t.Errorf("unhealthy volume storage: %d %s", w.Code, w.Body)
	}
}

func TestRetentionSkipsProtectedHistory(t *testing.T) {
	s := New(nil, WithMaxRetainedSandboxes(1))
	a := &sandbox{id: "protected", state: StateKilled, opts: sandboxOptions{mounts: []VolumeMount{{Path: "/mnt/a"}}}}
	b := &sandbox{id: "expired", state: StateKilled}
	s.order = []*sandbox{a, b}
	s.byID[a.id], s.byID[b.id] = a, b
	s.reservationLive[a.id] = false
	s.pruneKilled()
	if len(s.order) != 1 || s.order[0] != a || s.byID[b.id] != nil {
		t.Fatal("retention must keep only the protected record")
	}
}
