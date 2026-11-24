package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	sandbox "github.com/ruhuang2001/tiny-sandbox/sandbox-go"
)

func (in *invocation) doctor(args []string) int {
	if len(args) > 0 && args[0] == "mcp" {
		return in.doctorMCP(args[1:])
	}
	if len(args) > 0 {
		_, _ = fmt.Fprintln(in.stderr, "usage: sandboxctl doctor")
		return 2
	}

	client, clientErr := in.client()
	domain := in.effectiveDomain()

	var failed []error
	report := func(name string, err error, detail string) {
		if err == nil {
			if detail != "" {
				_, _ = fmt.Fprintf(in.stdout, "%s: ok (%s)\n", name, detail)
			} else {
				_, _ = fmt.Fprintf(in.stdout, "%s: ok\n", name)
			}
			return
		}
		_, _ = fmt.Fprintf(in.stdout, "%s: fail (%s)\n", name, messageOf(err))
		failed = append(failed, err)
	}

	if clientErr != nil {
		report("config", clientErr, "")
		checksFailed(in.stdout, in.stderr, failed)
		return 1
	}
	keyState := "no api key"
	if in.globals.apiKey != "" || os.Getenv("SANDBOX_API_KEY") != "" {
		keyState = "api key set"
	}
	report("config", nil, domain+", "+keyState)

	healthCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	err := healthProbe(healthCtx, domain)
	cancel()
	report("health", err, "")

	authCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, err = client.GetSandbox(authCtx, "sbx_doctor_probe")
	cancel()
	switch {
	case err == nil:
		report("auth", fmt.Errorf("a nonexistent sandbox was returned"), "")
	case isNotFound(err):
		report("auth", nil, "")
	case isUnauthorized(err):
		report("auth", err, "")
	default:
		report("auth", err, "")
	}

	if isLocalDomain(domain) {
		report("docker", checkDocker(), "")
	} else {
		_, _ = fmt.Fprintf(in.stdout, "docker: skipped (remote domain)\n")
	}

	report("sandbox", in.checkSandbox(client), "")

	return checksFailed(in.stdout, in.stderr, failed)
}

func (in *invocation) doctorMCP(args []string) int {
	fs := flag.NewFlagSet("doctor mcp", flag.ContinueOnError)
	fs.SetOutput(in.stderr)
	dir := fs.String("workspace", ".", "host workspace directory")
	image := fs.String("image", "tiny-sandbox-mcp:local", "offline pytest image")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || *dir == "" || strings.TrimSpace(*image) == "" {
		fs.Usage()
		return 2
	}
	workspace, err := filepath.Abs(*dir)
	if err != nil {
		return in.fail(err)
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return in.fail(fmt.Errorf("workspace: %w", err))
	}
	defer func() { _ = root.Close() }()
	binary, err := os.Executable()
	if err != nil {
		return in.fail(err)
	}
	client, err := in.client()
	if err != nil {
		return in.fail(err)
	}
	domain := in.effectiveDomain()
	healthCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	err = healthProbe(healthCtx, domain)
	cancel()
	if err != nil {
		return in.fail(fmt.Errorf("daemon: %w", err))
	}
	if err := checkPytestImage(client, *image); err != nil {
		return in.fail(err)
	}
	config := map[string]any{"mcpServers": map[string]any{"tiny-sandbox": map[string]any{
		"command": binary, "args": []string{"mcp", "--workspace", workspace, "--image", *image},
		"env": map[string]string{"SANDBOX_DOMAIN": domain},
	}}}
	if err := json.MarshalWrite(in.stdout, config); err != nil {
		return in.fail(err)
	}
	return 0
}

func checkPytestImage(client *sandbox.Client, image string) (result error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	sb, err := client.CreateSandbox(ctx, sandbox.CreateSandboxRequest{Image: &image, Network: new(sandbox.NetworkNone), TimeoutSeconds: new(120)})
	if err != nil {
		return fmt.Errorf("create offline sandbox with image %q (check image availability and daemon policy): %w", image, err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := client.DestroySandbox(ctx, sb.ID); err != nil {
			result = errors.Join(result, fmt.Errorf("clean up %s: %w", sb.ID, err))
		}
	}()
	if _, err := client.Exec(ctx, sb.ID, []string{"python3", "-m", "pytest", "--version"}); err != nil {
		return fmt.Errorf("pytest probe in %q failed (install Python and pytest in the image): %w", image, err)
	}
	return nil
}

func checksFailed(stdout, stderr io.Writer, failed []error) int {
	if len(failed) == 0 {
		_, _ = fmt.Fprintln(stdout, "Ready")
		return 0
	}
	_, _ = fmt.Fprintln(stdout, "not ready")
	seen := make(map[string]bool)
	for _, err := range failed {
		hint := hintFor(err)
		if hint == "" || seen[hint] {
			continue
		}
		seen[hint] = true
		_, _ = fmt.Fprintf(stderr, "hint: %s\n", hint)
	}
	return 1
}

func (in *invocation) effectiveDomain() string {
	if in.globals.domain != "" {
		return in.globals.domain
	}
	if env := os.Getenv("SANDBOX_DOMAIN"); env != "" {
		return env
	}
	return "http://localhost:8080"
}

const dockerInfoTimeout = 30 * time.Second

func checkDocker() error {
	ctx, cancel := context.WithTimeout(context.Background(), dockerInfoTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "info")
	cmd.Stdout = io.Discard
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	switch err := cmd.Run(); {
	case err != nil && ctx.Err() != nil:
		return fmt.Errorf("docker info timed out after %s", dockerInfoTimeout)
	case errors.Is(err, exec.ErrNotFound):
		return fmt.Errorf("docker CLI is not installed")
	case err != nil:
		if reason := strings.TrimSpace(stderr.String()); reason != "" {
			return fmt.Errorf("docker info failed: %s", firstLine(reason))
		}
		return fmt.Errorf("docker info failed: %w", err)
	}
	return nil
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

func (in *invocation) checkSandbox(client *sandbox.Client) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sb, err := client.CreateSandbox(ctx, sandbox.CreateSandboxRequest{})
	if err != nil {
		return err
	}
	defer func() {
		killCtx, killCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer killCancel()
		_, _ = client.KillSandbox(killCtx, sb.ID)
	}()

	ex, err := client.Exec(ctx, sb.ID, []string{"echo", "doctor-ok"})
	if err != nil {
		return err
	}
	if strings.TrimSpace(ex.Stdout) != "doctor-ok" {
		return fmt.Errorf("echo returned %q", strings.TrimSpace(ex.Stdout))
	}
	return nil
}

func isNotFound(err error) bool     { return errors.Is(err, sandbox.ErrNotFound) }
func isUnauthorized(err error) bool { return errors.Is(err, sandbox.ErrUnauthorized) }

func contextWithoutSignals() context.Context { return context.Background() }
