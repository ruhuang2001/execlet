package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	sandbox "github.com/ruhuang2001/tiny-sandbox/sandbox-go"
)

var version = "dev"

const usage = `sandboxctl — developer CLI for tiny-sandbox

usage:
  sandboxctl <command> [flags]

commands:
  mcp --workspace DIR --image IMAGE      serve sandbox tools over MCP stdio
  doctor                                 check config, connectivity, docker
  run [flags] -- CMD [ARG...]            run a command in a sandbox
  run [flags] -- code 'PYTHON'           shorthand for python3 -c
  attach SANDBOX EXECUTION               stream an execution from the start
  logs SANDBOX EXECUTION                 print retained output
  inspect SANDBOX                        print one sandbox
  list                                   list sandboxes
  files list SANDBOX PATH [flags]         list directory entries as JSON
  files archive upload|download SANDBOX PATH LOCAL_TAR
                                         transfer a tar archive
  pause SANDBOX                          pause a sandbox
  resume SANDBOX                         resume a sandbox
  kill SANDBOX                           kill a sandbox
  version                                print the build version
  help                                   print this message

run flags:
  --sandbox ID          use an existing sandbox instead of creating one
  --image NAME          image for a new sandbox
  --memory MB           memory limit
  --max-concurrent-executions N  execution slots (default 1, maximum 8)
  --pids-limit N        task limit (default 1024, minimum 64)
  --nano-cpus N         CPU limit in units of 1e-9 CPUs (0 is unlimited)
  --network MODE        bridge (default) or none
  --timeout S           lifetime timeout in seconds
  --timeout-action A    kill or pause
  --idle-timeout S      idle timeout in seconds
  --idle-action A       kill or pause
  --exec-timeout S      timeout for this command in seconds (0 disables)
  --env NAME=VALUE      variable for this command only; repeatable
  --cwd DIR             absolute directory to run the command in
  --stdin-file FILE     one-shot UTF-8 input (use - for stdin; maximum 1 MiB)
  --rm                  kill the sandbox after the command

global flags (before the command):
  --domain URL          server base URL
  --api-key KEY         API key

Configuration:
  SANDBOX_DOMAIN   server base URL (default http://localhost:8080)
  SANDBOX_API_KEY  API key, when the server requires one
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}

	g := &globals{}
	if code, done := g.parse(args, stdout, stderr); done {
		return code
	}
	args = g.rest
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}

	command, rest := args[0], args[1:]
	in := &invocation{stdout: stdout, stderr: stderr, globals: g}

	switch command {
	case "version":
		_, _ = fmt.Fprintln(stdout, version)
		return 0
	case "help", "-h", "--help":
		_, _ = fmt.Fprint(stdout, usage)
		return 0
	case "mcp":
		return in.mcpServer(rest)
	case "doctor":
		return in.doctor(rest)
	case "run":
		return in.runCommand(rest)
	case "attach":
		return in.attach(rest)
	case "logs":
		return in.logs(rest)
	case "inspect":
		return in.inspect(rest)
	case "files":
		return in.files(rest)
	case "list":
		return in.list(rest)
	case "pause", "resume", "kill":
		return in.lifecycle(command, rest)
	default:
		_, _ = fmt.Fprintf(stderr, "sandboxctl: unknown command %q\n\n%s", command, usage)
		return 2
	}
}

type globals struct {
	domain string
	apiKey string
	rest   []string
}

func (g *globals) parse(args []string, _, stderr io.Writer) (int, bool) {
	fs := flag.NewFlagSet("sandboxctl", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&g.domain, "domain", "", "server base URL (default $SANDBOX_DOMAIN)")
	fs.StringVar(&g.apiKey, "api-key", "", "API key (default $SANDBOX_API_KEY)")
	if err := fs.Parse(args); err != nil {
		return 2, true
	}
	g.rest = fs.Args()
	return 0, false
}

type invocation struct {
	stdout  io.Writer
	stderr  io.Writer
	globals *globals
}

func (in *invocation) client() (*sandbox.Client, error) {
	cfg := sandbox.Config{Domain: in.globals.domain, APIKey: in.globals.apiKey}
	c, err := sandbox.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func contextWithSignals() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func (in *invocation) fail(err error) int {
	_, _ = fmt.Fprintf(in.stderr, "sandboxctl: %s\n", messageOf(err))
	if hint := hintFor(err); hint != "" {
		_, _ = fmt.Fprintf(in.stderr, "hint: %s\n", hint)
	}
	return exitCode(err)
}

func messageOf(err error) string {
	msg := err.Error()
	msg = strings.TrimPrefix(msg, "sandbox: ")
	return msg
}

func exitCode(err error) int {
	var sdkErr *sandbox.Error
	if !errors.As(err, &sdkErr) {
		return 1
	}
	if sdkErr.Execution != nil && sdkErr.Execution.TerminationReason != nil &&
		*sdkErr.Execution.TerminationReason == sandbox.ReasonExited &&
		sdkErr.Execution.ExitCode != nil {
		code := *sdkErr.Execution.ExitCode
		if code >= 1 && code <= 125 {
			return code
		}
		return 1
	}
	if errors.Is(err, sandbox.ErrCanceled) {
		return 130
	}
	return 1
}

func hintFor(err error) string {
	switch {
	case errors.Is(err, sandbox.ErrOOM):
		return "increase --memory or reduce the command's memory use"
	case errors.Is(err, sandbox.ErrTimeout):
		return "increase --timeout or --exec-timeout"
	case errors.Is(err, sandbox.ErrEvicted):
		return "the container was stopped outside sandboxd"
	case errors.Is(err, sandbox.ErrBusy):
		return "wait for the running execution or cancel it"
	case errors.Is(err, sandbox.ErrUnauthorized):
		return "set a valid SANDBOX_API_KEY or --api-key"
	case errors.Is(err, sandbox.ErrNotFound):
		return "check the sandbox and execution IDs"
	case isConnectionRefused(err):
		return "start sandboxd or set SANDBOX_DOMAIN"
	}
	return ""
}

func isConnectionRefused(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	var sdkErr *sandbox.Error
	if errors.As(err, &sdkErr) && sdkErr.Code == "transport" {
		return true
	}
	return strings.Contains(err.Error(), "connection refused")
}

func (in *invocation) printSandbox(sb *sandbox.Sandbox) {
	_, _ = fmt.Fprintf(in.stdout, "%s %s %s %s\n", sb.ID, sb.State, sb.Image,
		sb.CreatedAt.UTC().Format(time.RFC3339))
}

func (in *invocation) printExecutionStatus(ex *sandbox.Execution) {
	if ex == nil {
		return
	}
	reason := "unknown"
	if ex.TerminationReason != nil {
		reason = string(*ex.TerminationReason)
	}
	exit := "none"
	if ex.ExitCode != nil {
		exit = fmt.Sprintf("%d", *ex.ExitCode)
	}
	_, _ = fmt.Fprintf(in.stderr, "execution %s: %s (%s), exit %s\n", ex.ID, ex.State, reason, exit)
}

func splitCommand(args []string) (flags, command []string) {
	for i, arg := range args {
		if arg == "--" {
			return args[:i], args[i+1:]
		}
	}
	return args, nil
}

func splitArgs(args []string) (positional, flags []string) {
	i := 0
	for ; i < len(args); i++ {
		if strings.HasPrefix(args[i], "-") {
			break
		}
		positional = append(positional, args[i])
	}
	return positional, args[i:]
}

func isLocalDomain(domain string) bool {
	u, err := url.Parse(domain)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func healthProbe(ctx context.Context, domain string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(domain, "/")+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz answered %d", resp.StatusCode)
	}
	return nil
}
