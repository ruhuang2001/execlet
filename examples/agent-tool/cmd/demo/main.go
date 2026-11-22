package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"flag"
	"fmt"
	"net/http/httptest"
	"os"
	"os/signal"
	"syscall"

	agenttool "github.com/ruhuang2001/tiny-sandbox/examples/agent-tool"
	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
	"github.com/ruhuang2001/tiny-sandbox/internal/daemon/membackend"
	sandbox "github.com/ruhuang2001/tiny-sandbox/sandbox-go"
)

func main() {
	code := flag.String("code", "print(2 ** 10)", "Python source to run")
	timeout := flag.Int("timeout", 60, "sandbox lifetime in seconds")
	showDefinition := flag.Bool("definition", false, "print the tool definition as JSON and exit")
	flag.Parse()

	if *showDefinition {
		printJSON(agenttool.ToolDefinition())
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client, cleanup, err := clientFor(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-tool: %v\n", err)
		os.Exit(1)
	}
	defer cleanup()

	tool := agenttool.New(client)
	result := tool.Run(ctx, agenttool.Input{Code: *code, TimeoutSeconds: *timeout})

	printJSON(result)
	if result.IsError {
		os.Exit(1)
	}
}

func clientFor(ctx context.Context) (*sandbox.Client, func(), error) {
	if domain := os.Getenv("SANDBOX_DOMAIN"); domain != "" {
		client, err := sandbox.NewClient(sandbox.Config{Domain: domain})
		return client, func() {}, err
	}

	backend := membackend.New()
	backend.Programs["python3"] = memDemoPython
	store := daemon.New(backend)
	srv := httptest.NewServer(daemon.NewHandler(store, "demo"))
	_ = ctx

	client, err := sandbox.NewClient(sandbox.Config{Domain: srv.URL})
	if err != nil {
		srv.Close()
		return nil, nil, err
	}
	fmt.Fprintln(os.Stderr, "agent-tool: no SANDBOX_DOMAIN set; using the in-memory demo server")
	return client, srv.Close, nil
}

func memDemoPython(_ context.Context, args []string) (string, string, int) {
	if len(args) == 2 && args[0] == "-c" && args[1] == "print(2 ** 10)" {
		return "1024\n", "", 0
	}
	return "", "name 'unknown' is not defined\n", 1
}

func printJSON(v any) {
	data, err := json.Marshal(v, jsontext.WithIndent("  "), json.Deterministic(true))
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-tool: encode: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(string(data))
}
