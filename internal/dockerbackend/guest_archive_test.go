package dockerbackend

import (
	"archive/tar"
	"bytes"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/ruhuang2001/execlet/internal/daemon"
	"github.com/ruhuang2001/execlet/internal/guestprotocol"
)

func serveTransferPut(t *testing.T, w http.ResponseWriter, ex *fakeExec) {
	t.Helper()
	size, err := strconv.ParseInt(ex.command[4], 10, 64)
	if err != nil {
		t.Error(err)
		return
	}
	w.Header().Set("Connection", "Upgrade")
	w.Header().Set("Upgrade", "tcp")
	w.WriteHeader(http.StatusSwitchingProtocols)
	conn, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		t.Error(err)
		return
	}
	defer func() { _ = conn.Close() }()
	if _, err := io.CopyN(io.Discard, rw, size); err != nil {
		t.Error(err)
		return
	}
	writeFrame(conn, 1, ex.stdout)
}

func TestArchiveWriteAttribution(t *testing.T) {
	f := newFakeDaemon(t)
	b := f.backend(t)
	s := daemon.New(b)
	sb, err := s.Create(t.Context(), daemon.CreateRequest{})
	if err != nil {
		t.Fatal(err)
	}
	id := f.resolveContainer("execlet-" + sb.ID)
	b.guests.Store(id, &guestSession{mounts: []daemon.BindMount{{Target: "/mnt/data"}, {Target: "/mnt/input", ReadOnly: true}}})
	handler := daemon.NewHandler(s, "test")
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	for _, name := range []string{"data-old/first", "./data/result", "input/file"} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	const full, ro = protocol.CodeInsufficientStorage, protocol.CodeReadOnlyFilesystem
	for _, tc := range []struct {
		name, url, code, member string
		status                  int
		want                    daemon.Code
	}{
		{"mixed full", "archive?path=/mnt", full, "data/result", 507, daemon.CodeVolumeQuotaExceeded},
		{"mixed read only", "archive?path=/mnt", ro, "input/file", 409, daemon.CodeVolumeReadOnly},
		{"ordinary full", "archive?path=/mnt", full, "data-old/first", 502, daemon.CodeBackendError},
		{"ordinary read only", "archive?path=/mnt", ro, "data-old/first", 502, daemon.CodeBackendError},
		{"wrong mount mode", "archive?path=/mnt", ro, "data/result", 502, daemon.CodeBackendError},
		{"missing member", "archive?path=/mnt", full, "", 502, daemon.CodeBackendError},
		{"unknown member", "archive?path=/mnt", full, "data/not-in-archive", 502, daemon.CodeBackendError},
		{"direct volume", "archive?path=/mnt/input", ro, "data/result", 409, daemon.CodeVolumeReadOnly},
		{"missing file", "archive?path=/mnt", "not_found", "", 404, daemon.CodeNotFound},
		{"invalid request", "archive?path=/mnt", "invalid_request", "", 400, daemon.CodeInvalidRequest},
		{"single file full", "?path=/mnt/data/result", full, "mnt/data/result", 507, daemon.CodeVolumeQuotaExceeded},
		{"single file read only", "?path=/mnt/input/file", ro, "mnt/input/file", 409, daemon.CodeVolumeReadOnly},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reply bytes.Buffer
			if err := protocol.Write(&reply, 'e', protocol.Error{Code: tc.code, Message: "filesystem operation failed", ArchiveMember: tc.member}); err != nil {
				t.Fatal(err)
			}
			f.setProgram(func(_ string, cmd []string) (string, string, int) {
				if cmd[0] == protocol.Binary && cmd[1] == "transfer" {
					return reply.String(), "", 0
				}
				return "", "", 0
			})
			target := fmt.Sprintf("/v1/sandboxes/%s/files", sb.ID)
			body := archive.Bytes()
			if tc.url[0] == '?' {
				body = []byte("file contents")
			} else {
				target += "/"
			}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, target+tc.url, bytes.NewReader(body))
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			var got struct {
				Code daemon.Code `json:"code"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != tc.status || got.Code != tc.want {
				t.Errorf("got %d %s, want %d %s", w.Code, w.Body.String(), tc.status, tc.want)
			}
		})
	}
}
