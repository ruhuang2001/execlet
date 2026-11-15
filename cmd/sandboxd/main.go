// Command sandboxd is the reference server for tiny-sandbox.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
	"github.com/ruhuang2001/tiny-sandbox/internal/dockerapi"
	"github.com/ruhuang2001/tiny-sandbox/internal/dockerbackend"
)

var version = "dev"

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	dockerHost := flag.String("docker", defaultDockerSocket(), "Docker socket path")
	owner := flag.String("owner", "", "container ownership label; defaults to the listen address")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	if !isLoopback(*addr) && os.Getenv("SANDBOX_API_KEY") == "" {
		slog.Error("refusing non-loopback listener without authentication", "addr", *addr)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	backend, err := dockerbackend.Dial(ctx, *dockerHost)
	if err != nil {
		slog.Error("docker connection failed", "error_category", runtimeErrorCategory(err))
		os.Exit(1)
	}

	// State is in memory, so containers from a previous process are orphans.
	// Removing them by the owner label is what keeps a restart from leaking
	// sandboxes. The label defaults to the listen address, the only stable
	// identity a restart has, but it can be pinned with -owner so a restart on
	// a different port still finds the previous process's containers.
	if *owner == "" {
		*owner = *addr
	}
	if removed, err := backend.RemoveOwned(ctx, daemon.OwnerLabelKey, *owner); err != nil {
		slog.Warn("startup cleanup incomplete", "removed", removed, "error_category", runtimeErrorCategory(err))
	} else if removed > 0 {
		slog.Info("startup cleanup completed", "removed", removed)
	}

	store := daemon.New(backend, daemon.WithOwnerLabel(*owner))
	go store.Run(ctx)

	// shuttingDown tells event streams to end. Without it a live SSE handler
	// outlives the shutdown deadline: http.Server.Shutdown stops accepting and
	// waits for handlers, but does not cancel their request contexts.
	shuttingDown := make(chan struct{})
	handler := daemon.NewHandler(store, version).WithShutdown(shuttingDown)
	if key := os.Getenv("SANDBOX_API_KEY"); key != "" {
		handler = handler.WithAPIKey(key)
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	// The drain runs in its own goroutine because it must unblock
	// ListenAndServe. main then waits on it: returning as soon as
	// ListenAndServe reports ErrServerClosed would kill the sweep halfway and
	// leave the containers behind.
	srvDone := make(chan struct{})
	go func() {
		defer close(srvDone)
		<-ctx.Done()
		close(shuttingDown)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Warn("http shutdown incomplete", "error", err)
		}
		// Containers are this process's state, so they go with it. Startup
		// cleanup covers a crash; this covers the ordinary stop.
		store.Shutdown(context.Background())
		slog.Info("shutdown completed")
	}()

	slog.Info("server listening", "version", version, "addr", *addr, "owner", *owner)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("http server failed", "error", err)
		os.Exit(1)
	}
	// ListenAndServe returns as soon as Shutdown begins. Wait for the drain and
	// the container sweep before exiting.
	<-srvDone
}

// runtimeErrorCategory preserves actionable failure classes without Docker's
// response body, which may contain private image names or runtime payloads.
func runtimeErrorCategory(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, os.ErrNotExist):
		return "socket_missing"
	case errors.Is(err, os.ErrPermission):
		return "permission_denied"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection_refused"
	}
	if status, ok := errors.AsType[*dockerapi.StatusError](err); ok {
		return fmt.Sprintf("docker_http_%d", status.Status)
	}
	return "runtime_error"
}

// defaultDockerSocket honours DOCKER_HOST and otherwise uses the standard
// unix socket.
func defaultDockerSocket() string {
	if host := os.Getenv("DOCKER_HOST"); host != "" {
		const prefix = "unix://"
		if after, ok := strings.CutPrefix(host, prefix); ok {
			return after
		}
		slog.Warn("ignoring non-unix DOCKER_HOST")
	}
	return "/var/run/docker.sock"
}

// isLoopback reports whether addr binds a loopback interface. An empty host,
// such as ":8080", binds every interface and is not loopback.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
