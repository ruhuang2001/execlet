package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	sandbox "github.com/ruhuang2001/execlet/sandbox-go"
)

type runFlags struct {
	sandboxID               string
	image                   string
	memoryMB                int
	pidsLimit               int
	nanoCPUs                int64
	network                 string
	timeout                 int
	timeoutAction           string
	idleTimeout             int
	idleAction              string
	execTimeout             int
	cwd                     string
	stdinFile               string
	env                     envFlags
	remove                  bool
	maxConcurrentExecutions int

	given map[string]bool
}

func (rf *runFlags) set(name string) bool { return rf.given[name] }

type envFlags []string

func (e *envFlags) String() string { return strings.Join(*e, ",") }

func (e *envFlags) Set(value string) error {
	name, _, ok := strings.Cut(value, "=")
	if !ok || name == "" {
		return fmt.Errorf("expected NAME=VALUE, got %q", value)
	}
	*e = append(*e, value)
	return nil
}

func (in *invocation) runCommand(args []string) int {
	flagsPart, command := splitCommand(args)
	rf := &runFlags{}
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(in.stderr)
	fs.StringVar(&rf.sandboxID, "sandbox", "", "use an existing sandbox")
	fs.StringVar(&rf.image, "image", "", "image for a new sandbox")
	fs.IntVar(&rf.memoryMB, "memory", 0, "memory limit in MB")
	fs.IntVar(&rf.maxConcurrentExecutions, "max-concurrent-executions", 1, "maximum simultaneous executions (1-8)")
	fs.IntVar(&rf.pidsLimit, "pids-limit", 0, "task limit in the sandbox")
	fs.Int64Var(&rf.nanoCPUs, "nano-cpus", 0, "CPU limit in units of 1e-9 CPUs (0 is unlimited)")
	fs.StringVar(&rf.network, "network", "", "bridge, filtered or none")
	fs.IntVar(&rf.timeout, "timeout", 0, "lifetime timeout in seconds (0 disables)")
	fs.StringVar(&rf.timeoutAction, "timeout-action", "", "kill or pause")
	fs.IntVar(&rf.idleTimeout, "idle-timeout", 0, "idle timeout in seconds (0 disables)")
	fs.StringVar(&rf.idleAction, "idle-action", "", "kill or pause")
	fs.IntVar(&rf.execTimeout, "exec-timeout", 0, "timeout for this command in seconds (0 disables)")
	fs.StringVar(&rf.cwd, "cwd", "", "absolute directory to run the command in")
	fs.StringVar(&rf.stdinFile, "stdin-file", "", "one-shot UTF-8 input file (use - for stdin; maximum 1 MiB)")
	fs.Var(&rf.env, "env", "NAME=VALUE for this command; repeatable")
	fs.BoolVar(&rf.remove, "rm", false, "kill the sandbox after the command")
	if err := fs.Parse(flagsPart); err != nil {
		return 2
	}
	command = append(command, fs.Args()...)

	rf.given = make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { rf.given[f.Name] = true })

	if rf.set("memory") && rf.memoryMB < 0 {
		_, _ = fmt.Fprintln(in.stderr, "execletctl: --memory must not be negative")
		return 2
	}
	if rf.set("timeout") && rf.timeout < 0 {
		_, _ = fmt.Fprintln(in.stderr, "execletctl: --timeout must not be negative; use 0 to disable")
		return 2
	}
	if rf.set("idle-timeout") && rf.idleTimeout < 0 {
		_, _ = fmt.Fprintln(in.stderr, "execletctl: --idle-timeout must not be negative; use 0 to disable")
		return 2
	}
	if rf.set("exec-timeout") && rf.execTimeout < 0 {
		_, _ = fmt.Fprintln(in.stderr, "execletctl: --exec-timeout must not be negative; use 0 to disable")
		return 2
	}
	if rf.set("max-concurrent-executions") && (rf.maxConcurrentExecutions < 1 || rf.maxConcurrentExecutions > 8) {
		_, _ = fmt.Fprintln(in.stderr, "execletctl: --max-concurrent-executions must be between 1 and 8")
		return 2
	}
	if rf.set("pids-limit") && rf.pidsLimit <= 0 {
		_, _ = fmt.Fprintln(in.stderr, "execletctl: --pids-limit must be positive")
		return 2
	}
	if rf.set("nano-cpus") && rf.nanoCPUs < 0 {
		_, _ = fmt.Fprintln(in.stderr, "execletctl: --nano-cpus must not be negative; use 0 for no limit")
		return 2
	}
	if rf.network != "" && rf.network != sandbox.NetworkBridge && rf.network != sandbox.NetworkNone && rf.network != sandbox.NetworkFiltered {
		_, _ = fmt.Fprintf(in.stderr, "execletctl: --network must be %q, %q or filtered\n",
			sandbox.NetworkBridge, sandbox.NetworkNone)
		return 2
	}

	if len(command) == 0 {
		_, _ = fmt.Fprintln(in.stderr, "execletctl: run needs a command, e.g. execletctl run -- ls -l")
		return 2
	}

	if command[0] == "code" {
		if len(command) != 2 {
			_, _ = fmt.Fprintln(in.stderr, "execletctl: code takes exactly one argument: the Python program")
			return 2
		}
		command = []string{"python3", "-c", command[1]}
		if rf.image == "" && rf.sandboxID == "" {
			rf.image = "python:3.12-alpine"
		}
	}

	if err := checkNoCreationFlags(rf); err != nil {
		return in.fail(err)
	}
	var input string
	if rf.set("stdin-file") {
		source := os.Stdin
		if rf.stdinFile != "-" {
			var err error
			source, err = os.Open(rf.stdinFile)
			if err != nil {
				return in.fail(err)
			}
			defer func() { _ = source.Close() }()
		}
		data, err := io.ReadAll(io.LimitReader(source, (1<<20)+1))
		if err != nil {
			return in.fail(err)
		}
		if len(data) > 1<<20 || !utf8.Valid(data) {
			_, _ = fmt.Fprintln(in.stderr, "execletctl: stdin must be valid UTF-8 and at most 1 MiB")
			return 2
		}
		input = string(data)
	}
	client, err := in.client()
	if err != nil {
		return in.fail(err)
	}

	ctx, stop := contextWithSignals()
	defer stop()

	created := false
	sandboxID := rf.sandboxID
	if sandboxID == "" {
		sb, err := client.CreateSandbox(ctx, rf.createRequest())
		if err != nil {
			return in.fail(err)
		}
		created = true
		sandboxID = sb.ID
		_, _ = fmt.Fprintf(in.stderr, "sandbox: %s (created; remove with: execletctl kill %s)\n", sb.ID, sb.ID)
	} else {
		_, _ = fmt.Fprintf(in.stderr, "sandbox: %s\n", sandboxID)
	}

	startReq := sandbox.ExecuteRequest{Command: command, Cwd: rf.cwd, Stdin: input}
	if rf.set("exec-timeout") {
		startReq.TimeoutSeconds = &rf.execTimeout
	}
	if len(rf.env) > 0 {
		startReq.Env = make(map[string]string, len(rf.env))
		for _, kv := range rf.env {
			name, value, _ := strings.Cut(kv, "=")
			startReq.Env[name] = value
		}
	}
	started, err := client.StartExecution(ctx, sandboxID, startReq)
	if err != nil {
		return in.startFailed(client, sandboxID, created, rf.remove, err)
	}
	_, _ = fmt.Fprintf(in.stderr, "execution: %s\n", started.ID)

	final, streamErr := client.StreamExecution(ctx, sandboxID, started.ID, func(ev sandbox.Event) error {
		return in.writeEvent(ev)
	})
	if final == nil {
		final = started
	}
	in.printExecutionStatus(final)

	if rf.remove && created {
		in.cleanupSandbox(client, sandboxID)
	}

	if streamErr != nil {
		return in.fail(streamErr)
	}
	if err := outcomeExit(final); err != nil {
		return in.fail(err)
	}
	return 0
}

func (in *invocation) startFailed(client *sandbox.Client, sandboxID string, created, remove bool, err error) int {
	if created && (remove || errors.Is(err, sandbox.ErrCanceled) || errors.Is(err, sandbox.ErrCancelUnconfirmed)) {
		in.cleanupSandbox(client, sandboxID)
	}
	return in.fail(err)
}

func (in *invocation) cleanupSandbox(client *sandbox.Client, sandboxID string) {
	kctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sb, err := client.KillSandbox(kctx, sandboxID)
	if err == nil && (sb.ID != sandboxID || sb.State != sandbox.StateKilled) {
		err = fmt.Errorf("server returned sandbox %q in state %q", sb.ID, sb.State)
	}
	if err != nil {
		_, _ = fmt.Fprintf(in.stderr, "execletctl: cleanup unconfirmed for %s: %s\n", sandboxID, messageOf(err))
	}
}

func outcomeExit(ex *sandbox.Execution) error {
	if ex == nil {
		return nil
	}
	if ex.TerminationReason != nil && *ex.TerminationReason == sandbox.ReasonExited {
		if ex.ExitCode == nil || *ex.ExitCode == 0 {
			return nil
		}
	}
	code := 0
	if ex.ExitCode != nil {
		code = *ex.ExitCode
	}
	reason := sandbox.Reason("")
	if ex.TerminationReason != nil {
		reason = *ex.TerminationReason
	}
	return &sandbox.Error{
		Code:      string(reason),
		Message:   fmt.Sprintf("command ended (%s) with exit code %d", reason, code),
		Execution: ex,
	}
}

func (rf *runFlags) createRequest() sandbox.CreateSandboxRequest {
	req := sandbox.CreateSandboxRequest{}
	if rf.image != "" {
		req.Image = &rf.image
	}
	if rf.set("memory") {
		req.MemoryMB = &rf.memoryMB
	}
	if rf.set("max-concurrent-executions") {
		req.MaxConcurrentExecutions = &rf.maxConcurrentExecutions
	}
	if rf.set("pids-limit") {
		req.PidsLimit = &rf.pidsLimit
	}
	if rf.set("nano-cpus") {
		req.NanoCPUs = &rf.nanoCPUs
	}
	if rf.network != "" {
		req.Network = &rf.network
	}
	if rf.set("timeout") {
		req.TimeoutSeconds = &rf.timeout
	}
	if rf.timeoutAction != "" {
		action := sandbox.Action(rf.timeoutAction)
		req.TimeoutAction = &action
	}
	if rf.set("idle-timeout") {
		req.IdleTimeoutSeconds = &rf.idleTimeout
	}
	if rf.idleAction != "" {
		action := sandbox.Action(rf.idleAction)
		req.IdleTimeoutAction = &action
	}
	return req
}

func checkNoCreationFlags(rf *runFlags) error {
	var used []string
	if rf.image != "" {
		used = append(used, "--image")
	}
	if rf.set("memory") {
		used = append(used, "--memory")
	}
	if rf.set("max-concurrent-executions") {
		used = append(used, "--max-concurrent-executions")
	}
	if rf.set("pids-limit") {
		used = append(used, "--pids-limit")
	}
	if rf.set("nano-cpus") {
		used = append(used, "--nano-cpus")
	}
	if rf.network != "" {
		used = append(used, "--network")
	}
	if rf.set("timeout") {
		used = append(used, "--timeout")
	}
	if rf.timeoutAction != "" {
		used = append(used, "--timeout-action")
	}
	if rf.set("idle-timeout") {
		used = append(used, "--idle-timeout")
	}
	if rf.idleAction != "" {
		used = append(used, "--idle-action")
	}
	if len(used) == 0 {
		return nil
	}
	if rf.sandboxID == "" {
		return nil
	}
	return fmt.Errorf("%s cannot be combined with --sandbox", strings.Join(used, ", "))
}
