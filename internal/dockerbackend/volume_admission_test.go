package dockerbackend

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ruhuang2001/execlet/internal/daemon"
	"github.com/ruhuang2001/execlet/internal/dockerapi"
)

func TestVolumeMountInspection(t *testing.T) {
	m := daemon.BindMount{Source: "/storage", Target: "/mnt/work", ReadOnly: true}
	bind := dockerapi.MountPoint{Type: "bind", Source: m.Source, Destination: m.Target, RW: new(false)}
	for _, tc := range []struct {
		name   string
		mounts []dockerapi.MountPoint
		valid  bool
	}{
		{"matching", []dockerapi.MountPoint{bind}, true},
		{"unrelated", []dockerapi.MountPoint{bind, {Type: "volume", Destination: "/mnt/work-2"}}, true},
		{"descendant", []dockerapi.MountPoint{bind, {Type: "volume", Destination: "/mnt/work/sub"}}, false},
		{"other type at target", []dockerapi.MountPoint{bind, {Type: "tmpfs", Destination: m.Target}}, false},
		{"ambiguous", []dockerapi.MountPoint{bind, bind}, false},
		{"missing", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := verifyDockerMounts(tc.mounts, []daemon.BindMount{m}); (err == nil) != tc.valid {
				t.Fatalf("verify: %v", err)
			}
		})
	}
}

func TestVolumeCreateDispatchCertainty(t *testing.T) {
	for _, scenario := range []string{"cancel during inspect", "cannot dial", "response lost"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var posts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/images/") {
					if scenario == "cancel during inspect" {
						cancel()
					}
					_, _ = io.WriteString(w, `{}`)
					return
				}
				if r.URL.Path == "/containers/create" {
					posts.Add(1)
					panic(http.ErrAbortHandler)
				}
				t.Errorf("unexpected Docker call %s", r.URL.Path)
			}))
			defer server.Close()
			client := server.Client()
			if scenario == "cannot dial" {
				client = &http.Client{Transport: &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("socket offline") }}}
			}
			b := New(dockerapi.New(server.URL, client))
			b.guestMode = "auto"
			id, err := b.Create(ctx, daemon.ContainerSpec{Image: "test", Name: "execlet-sbx_1", Labels: map[string]string{daemon.OwnerLabelKey: "owner", daemon.SandboxIDLabelKey: "sbx_1"}, Mounts: []daemon.BindMount{{Target: "/mnt/test"}}})
			if err == nil {
				t.Fatal("expected create failure")
			}
			if scenario == "response lost" {
				if id != daemon.PendingVolumeCreatePrefix+"sbx_1" || posts.Load() != 1 {
					t.Fatalf("lost response: id=%q posts=%d", id, posts.Load())
				}
			} else if id != "" || posts.Load() != 0 || !errors.Is(err, dockerapi.ErrCreateNotSent) {
				t.Fatalf("not dispatched: id=%q posts=%d err=%v", id, posts.Load(), err)
			}
		})
	}
}
