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

	"github.com/ruhuang2001/execlet/internal/daemon"
	"github.com/ruhuang2001/execlet/internal/dockerapi"
	"github.com/ruhuang2001/execlet/internal/dockerbackend"
)

var version = "dev"

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	dockerHost := flag.String("docker", defaultDockerSocket(), "Docker socket path")
	owner := flag.String("owner", "", "container ownership label; defaults to the listen address")
	showVersion := flag.Bool("version", false, "print version and exit")
	policyPath := flag.String("policy", "", "administrator deployment policy JSON file")
	stateDir := flag.String("state-dir", "", "private local directory for conservative restart recovery")
	guestMode := flag.String("guest-agent", "auto", "guest agent mode (auto or off)")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	if !isLoopback(*addr) && os.Getenv("SANDBOX_API_KEY") == "" {
		slog.Error("refusing non-loopback listener without authentication", "addr", *addr)
		os.Exit(1)
	}
	var policy *daemon.DeploymentPolicy
	if *policyPath != "" {
		value, err := daemon.LoadPolicy(*policyPath)
		if err != nil {
			slog.Error("invalid deployment policy", "error", err)
			os.Exit(1)
		}
		policy = &value
		if policy.MaxVolumes > 0 && *stateDir == "" {
			slog.Error("persistent volumes require -state-dir")
			os.Exit(1)
		}
	}

	if *owner == "" {
		*owner = *addr
	}
	var journal *daemon.Journal
	if *stateDir != "" {
		var err error
		journal, err = daemon.OpenJournal(*stateDir, *owner)
		if err != nil {
			slog.Error("cannot open durable state", "error", err)
			os.Exit(1)
		}
		defer func() { _ = journal.Close() }()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	backend, err := dockerbackend.Dial(ctx, *dockerHost)
	if err != nil {
		slog.Error("docker connection failed", "error_category", runtimeErrorCategory(err))
		os.Exit(1)
	}

	if policy != nil {
		backend.ConfigureEgress(policy.Egress)
	}
	if err := backend.SetGuestAgent(*guestMode, policy != nil && policy.MaxVolumes > 0); err != nil {
		slog.Error("invalid guest mode", "error", err)
		os.Exit(1)
	}
	if err := backend.CheckLegacyOwner(ctx, *owner); err != nil {
		slog.Error("startup ownership check failed; refusing admission", "error", err)
		os.Exit(1)
	}
	if journal == nil {
		if removed, err := backend.RemoveOwned(ctx, daemon.OwnerLabelKey, *owner); err != nil {
			slog.Error("startup cleanup incomplete; refusing admission", "removed", removed, "error_category", runtimeErrorCategory(err))
			os.Exit(1)
		} else if removed > 0 {
			slog.Info("startup cleanup completed", "removed", removed)
		}
	}

	var runtime daemon.Backend = backend
	if policy != nil {
		runtime, err = daemon.NewPolicyBackend(backend, *policy)
		if err != nil {
			slog.Error("unsupported deployment policy", "error", err)
			os.Exit(1)
		}
	}
	store := daemon.New(runtime, daemon.WithOwnerLabel(*owner))
	if journal != nil {
		if err := store.Recover(ctx, journal, backend); err != nil {
			slog.Error("recovery failed; refusing admission", "error", err)
			os.Exit(1)
		}
	}
	reaperDone := make(chan struct{})
	go func() { defer close(reaperDone); store.Run(ctx) }()

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
		<-reaperDone
		if journal == nil {
			store.Shutdown(context.Background())
		} else {
			store.DrainPersistent(context.Background())
		}
		slog.Info("shutdown completed")
	}()

	slog.Info("server listening", "version", version, "addr", *addr, "owner", *owner)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("http server failed", "error", err)
		os.Exit(1)
	}
	<-srvDone
}

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
