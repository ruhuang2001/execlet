package sandbox

import (
	"encoding/json/v2"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestVolumeOperations(t *testing.T) {
	const id = "vol_a/b?c"
	const created = "2026-11-24T12:00:00Z"
	const size int64 = 1 << 34
	volume := map[string]any{"id": id, "size_bytes": size, "created_at": created}
	f := newFakeServer(t)
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.EscapedPath() {
		case "GET /v1/capabilities":
			writeJSON(w, http.StatusOK, map[string][]string{"capabilities": {"volumes.v1"}})
		case "POST /v1/volumes":
			writeJSON(w, http.StatusCreated, volume)
		case "GET /v1/volumes":
			writeJSON(w, http.StatusOK, map[string]any{"volumes": []any{volume}})
		case "GET /v1/volumes/vol_a%2Fb%3Fc":
			writeJSON(w, http.StatusOK, volume)
		case "DELETE /v1/volumes/vol_a%2Fb%3Fc":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	})
	c := f.client(t, -1)
	got, err := c.CreateVolume(t.Context(), CreateVolumeRequest{SizeBytes: size})
	if err != nil || got.ID != id || got.SizeBytes != size || got.CreatedAt.Format(time.RFC3339) != created {
		t.Fatalf("CreateVolume = %+v, %v", got, err)
	}
	listed, err := c.ListVolumes(t.Context())
	if err != nil || len(listed) != 1 || listed[0] != *got {
		t.Fatalf("ListVolumes = %+v, %v", listed, err)
	}
	fetched, err := c.GetVolume(t.Context(), id)
	if err != nil || *fetched != *got {
		t.Fatalf("GetVolume = %+v, %v", fetched, err)
	}
	if err := c.DeleteVolume(t.Context(), id); err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 5 || f.requests[0].path != "/v1/capabilities" {
		t.Fatalf("expected discovery before four operations, got %+v", f.requests)
	}
	assertVolumeJSON(t, f.requests[1].body, `{"size_bytes":17179869184}`)
}

func TestCreateSandboxWithMounts(t *testing.T) {
	mounts := []VolumeMount{{VolumeID: "vol_a", Path: "/mnt/data"}, {VolumeID: "vol_b", Path: "/mnt/input", ReadOnly: true}}
	f := newFakeServer(t)
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/capabilities":
			writeJSON(w, http.StatusOK, map[string][]string{"capabilities": {"volumes.v1", "sandbox.runtime.v1", "execution.env-cwd.v1", "execution.concurrent.v1"}})
		case "/v1/sandboxes", "/v1/sandboxes/sbx_a":
			_, _ = w.Write([]byte(`{"id":"sbx_a","state":"running","mounts":[{"volume_id":"vol_a","path":"/mnt/data"},{"volume_id":"vol_b","path":"/mnt/input","read_only":true}]}`))
		default:
			http.NotFound(w, r)
		}
	})
	c := f.client(t, -1)
	sb, err := c.CreateSandbox(t.Context(), CreateSandboxRequest{
		Runtime: "runc", Env: map[string]string{"A": "B"}, MaxConcurrentExecutions: new(2), Mounts: mounts,
	})
	if err != nil || !slices.Equal(sb.Mounts, mounts) {
		t.Fatalf("CreateSandbox = %+v, %v", sb, err)
	}
	sb, err = c.GetSandbox(t.Context(), sb.ID)
	if err != nil || !slices.Equal(sb.Mounts, mounts) {
		t.Fatalf("GetSandbox = %+v, %v", sb, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 3 || f.requests[0].path != "/v1/capabilities" || f.requests[1].method != http.MethodPost {
		t.Fatalf("expected one discovery before create and get, got %+v", f.requests)
	}
	assertVolumeJSON(t, f.requests[1].body, `{"runtime":"runc","env":{"A":"B"},"max_concurrent_executions":2,"mounts":[{"volume_id":"vol_a","path":"/mnt/data"},{"volume_id":"vol_b","path":"/mnt/input","read_only":true}]}`)
}

func TestUnsupportedVolumeOperationsFailClosed(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		f := newFakeServer(t)
		f.setHandler(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/capabilities" {
				if legacy {
					http.NotFound(w, r)
					return
				}
				writeJSON(w, http.StatusOK, map[string][]string{"capabilities": {"execution.env-cwd.v1"}})
				return
			}
			writeJSON(w, http.StatusCreated, map[string]string{"id": "sbx_legacy", "state": "running"})
		})
		c := f.client(t, -1)
		_, create := c.CreateVolume(t.Context(), CreateVolumeRequest{SizeBytes: 1 << 24})
		_, list := c.ListVolumes(t.Context())
		_, get := c.GetVolume(t.Context(), "vol_a")
		_, mount := c.CreateSandbox(t.Context(), CreateSandboxRequest{
			Mounts: []VolumeMount{{VolumeID: "vol_a", Path: "/mnt/data"}}, Env: map[string]string{"A": "B"},
		})
		for _, err := range []error{create, list, get, mount, c.DeleteVolume(t.Context(), "vol_a")} {
			if value, ok := errors.AsType[*Error](err); !ok || value.Code != "incompatible_server" {
				t.Fatalf("unsupported operation returned %v", err)
			}
		}
		before := f.count(http.MethodGet, "/v1/capabilities")
		if _, err := c.CreateSandbox(t.Context(), CreateSandboxRequest{Mounts: []VolumeMount{}}); err != nil {
			t.Fatalf("create without mounts: %v", err)
		}
		f.mu.Lock()
		requests := f.requests
		f.mu.Unlock()
		if before == 0 || len(requests) != before+1 || requests[before].path != "/v1/sandboxes" {
			t.Fatalf("expected only discovery before unmounted create, got %+v", requests)
		}
		assertVolumeJSON(t, requests[before].body, `{}`)
	}
}

func TestVolumeValidatesBeforeDispatch(t *testing.T) {
	f := newFakeServer(t)
	c := f.client(t, -1)
	_, create := c.CreateVolume(t.Context(), CreateVolumeRequest{SizeBytes: (1 << 24) + 1})
	_, invalidMount := c.CreateSandbox(t.Context(), CreateSandboxRequest{
		Mounts: []VolumeMount{{VolumeID: "vol_a", Path: "/mnt/../data"}},
	})
	_, overlapping := c.CreateSandbox(t.Context(), CreateSandboxRequest{
		Mounts: []VolumeMount{{VolumeID: "vol_a", Path: "/mnt/data"}, {VolumeID: "vol_b", Path: "/mnt/data/child"}},
	})
	for _, err := range []error{create, invalidMount, overlapping, c.DeleteVolume(t.Context(), "..")} {
		if value, ok := errors.AsType[*Error](err); !ok || value.Code != "invalid_request" {
			t.Fatalf("invalid operation returned %v", err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 0 {
		t.Fatalf("invalid request reached the server: %+v", f.requests)
	}
}

func TestMountPathBoundary(t *testing.T) {
	f := newFakeServer(t)
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/capabilities" {
			writeJSON(w, 200, map[string][]string{"capabilities": {"volumes.v1"}})
			return
		}
		writeJSON(w, 201, Sandbox{ID: "sbx_a", State: StateRunning})
	})
	c := f.client(t, 1)
	p := "/mnt/" + strings.Repeat("x/", 2045) + "x"
	create := func(path string) error {
		_, err := c.CreateSandbox(t.Context(), CreateSandboxRequest{Mounts: []VolumeMount{{VolumeID: "vol_a", Path: path}}})
		return err
	}
	if e, ok := errors.AsType[*Error](create(p + "x")); !ok || e.Code != "invalid_request" || len(f.requests) != 0 {
		t.Fatalf("4097 bytes: err=%v requests=%d", e, len(f.requests))
	}
	if err := create(p); err != nil || f.count(http.MethodPost, "/v1/sandboxes") != 1 {
		t.Fatalf("4096 bytes: %v", err)
	}
}

func TestDeleteVolumeLostResponse(t *testing.T) {
	f := newFakeServer(t)
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/capabilities" {
			writeJSON(w, 200, map[string][]string{"capabilities": {"volumes.v1"}})
			return
		}
		if r.URL.Path == "/v1/volumes/lost" && f.count(r.Method, r.URL.Path) == 1 {
			panic(http.ErrAbortHandler)
		}
		writeAPIError(w, 404, "not_found", "volume is absent")
	})
	c := f.client(t, 2)
	for id, code := range map[string]string{"missing": "not_found", "lost": "transport"} {
		err := c.DeleteVolume(t.Context(), id)
		if e, ok := errors.AsType[*Error](err); !ok || e.Code != code || f.count(http.MethodDelete, "/v1/volumes/"+id) != 1 {
			t.Errorf("DeleteVolume(%s): %v, want %s after one DELETE", id, err, code)
		}
	}
}

func TestVolumeRetryAndErrorCodes(t *testing.T) {
	f := newFakeServer(t)
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/capabilities":
			writeJSON(w, http.StatusOK, map[string][]string{"capabilities": {"volumes.v1"}})
		case "POST /v1/volumes":
			writeAPIError(w, http.StatusServiceUnavailable, "unavailable", "unavailable")
		case "GET /v1/volumes":
			if f.count(r.Method, r.URL.Path) == 1 {
				writeAPIError(w, http.StatusServiceUnavailable, "unavailable", "unavailable")
				return
			}
			writeJSON(w, http.StatusOK, map[string][]Volume{"volumes": {}})
		case "DELETE /v1/volumes/vol_a":
			writeAPIError(w, http.StatusConflict, "volume_in_use", "attached")
		case "DELETE /v1/volumes/vol_b":
			writeAPIError(w, http.StatusConflict, "future_volume_error", "future error")
		case "DELETE /v1/volumes/vol_c":
			if f.count(r.Method, r.URL.Path) == 1 {
				writeAPIError(w, http.StatusServiceUnavailable, "unavailable", "unavailable")
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
	c := f.client(t, 1)
	if _, err := c.CreateVolume(t.Context(), CreateVolumeRequest{SizeBytes: 1 << 24}); err == nil || f.count(http.MethodPost, "/v1/volumes") != 1 {
		t.Fatalf("volume creation did not fail after exactly one attempt: %v", err)
	}
	if list, err := c.ListVolumes(t.Context()); err != nil || list == nil || len(list) != 0 || f.count(http.MethodGet, "/v1/volumes") != 2 {
		t.Fatalf("volume list did not retry: %+v, %v", list, err)
	}
	for id, code := range map[string]string{"vol_a": "volume_in_use", "vol_b": "future_volume_error"} {
		err := c.DeleteVolume(t.Context(), id)
		if value, ok := errors.AsType[*Error](err); !ok || value.Code != code || value.StatusCode != http.StatusConflict {
			t.Fatalf("DeleteVolume(%s) = %v, want %s", id, err, code)
		}
	}
	if err := c.DeleteVolume(t.Context(), "vol_c"); err != nil || f.count(http.MethodDelete, "/v1/volumes/vol_c") != 2 {
		t.Fatalf("volume delete did not retry: %v", err)
	}
}

func assertVolumeJSON(t *testing.T, body []byte, expected string) {
	t.Helper()
	var got, want any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode request %q: %v", body, err)
	}
	if err := json.Unmarshal([]byte(expected), &want); err != nil {
		t.Fatalf("decode expectation %q: %v", expected, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("payload %s, want %s", body, expected)
	}
}
