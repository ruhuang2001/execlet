package main

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	sandbox "github.com/ruhuang2001/tiny-sandbox/sandbox-go"
)

const guestWorkspace = "/tmp/tiny-sandbox-workspace"

type sandboxTools struct {
	client           *sandbox.Client
	workspace        *os.Root
	cache            string
	images, networks []string
	ttl              int
	scope            string
}

func (in *invocation) mcpServer(args []string) int {
	flags := flag.NewFlagSet("mcp", flag.ContinueOnError)
	flags.SetOutput(in.stderr)
	dir := flags.String("workspace", "", "host directory for sync_workspace (required)")
	tools := sandboxTools{networks: []string{"none"}}
	flags.Func("image", "allowed image (required, repeatable)", func(image string) error {
		if strings.TrimSpace(image) == "" {
			return errors.New("image cannot be empty")
		}
		tools.images = append(tools.images, image)
		return nil
	})
	flags.Func("allow-network", "allowed network: none, filtered or bridge (repeatable)", func(network string) error {
		if !slices.Contains([]string{"none", "filtered", "bridge"}, network) {
			return fmt.Errorf("unsupported network %q", network)
		}
		if !slices.Contains(tools.networks, network) {
			tools.networks = append(tools.networks, network)
		}
		return nil
	})
	flags.IntVar(&tools.ttl, "sandbox-ttl", 1800, "lifetime from creation or last tool call, in seconds")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 || *dir == "" || len(tools.images) == 0 || tools.ttl <= 0 || int64(tools.ttl) > int64((1<<63-1)/time.Second) {
		_, _ = fmt.Fprintln(in.stderr, "usage: sandboxctl mcp --workspace DIR --image IMAGE [--allow-network MODE] [--sandbox-ttl SECONDS]")
		return 2
	}
	root, err := os.OpenRoot(*dir)
	if err != nil {
		return in.fail(err)
	}
	defer func() { _ = root.Close() }()
	tools.workspace = root
	home, err := os.UserHomeDir()
	if err != nil {
		return in.fail(err)
	}
	tools.cache = filepath.Join(home, ".cache", "tiny-sandbox", "mcp")
	tools.client, err = in.client()
	if err != nil {
		return in.fail(err)
	}
	tools.scope, err = tools.ownerScope(in)
	if err != nil {
		return in.fail(err)
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "sandboxctl", Version: version}, nil)
	tools.register(server)
	ctx, stop := contextWithSignals()
	defer stop()
	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
		return in.fail(err)
	}
	return 0
}

type sandboxInput struct {
	SandboxID string `json:"sandbox_id"`
}

func (in sandboxInput) sandboxID() string { return in.SandboxID }

type createInput struct {
	Image    string `json:"image,omitempty"`
	MemoryMB *int   `json:"memory_mb,omitempty"`
	Network  string `json:"network,omitempty"`
}
type fileInput struct {
	sandboxInput
	Path string `json:"path" jsonschema:"absolute path in the sandbox"`
}
type writeInput struct {
	fileInput
	Content string `json:"content"`
}
type runInput struct {
	sandboxInput
	Command []string `json:"command" jsonschema:"executable and its arguments"`
	Cwd     string   `json:"cwd,omitempty"`
	Timeout *int     `json:"timeout_seconds,omitempty"`
	Key     string   `json:"idempotency_key,omitempty"`
}
type executionInput struct {
	sandboxInput
	ExecutionID string `json:"execution_id"`
}
type getInput struct {
	executionInput
	Offset *int `json:"offset,omitempty" jsonschema:"byte position in each retained output stream; use next_offset for another page"`
}
type syncInput struct {
	sandboxInput
	Reset bool `json:"reset,omitempty"`
}

type toolOutput struct {
	summary string
	value   any
}

func (s *sandboxTools) register(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{Name: "create_sandbox", Description: fmt.Sprintf("Create a sandbox. Images: %s. Networks: %s (default none, offline). Lifetime %d seconds from each tool call.", strings.Join(s.images, ", "), strings.Join(s.networks, ", "), s.ttl)}, func(ctx context.Context, _ *mcp.CallToolRequest, in createInput) (*mcp.CallToolResult, any, error) {
		out, err := s.create(ctx, in)
		return respond(out, err)
	})
	s.add(server, "sync_workspace", "Sync host files into "+guestWorkspace+". Edit sources on the host; changes inside the sandbox stay there. Reset uploads all host files.", s.syncWorkspace)
	s.add(server, "run_command", "Run argv in the sandbox. Defaults to the synced workspace. Wait up to 60 seconds. Output defaults to the last 64 KiB per stream; the daemon retains up to 1 MiB total.", s.runTool)
	s.add(server, "get_execution", "Inspect an execution and read its retained output. Pass offset 0 to start paging from the beginning.", s.getTool)
	s.add(server, "cancel_execution", "Cancel an execution and inspect its termination reason.", s.cancelTool)
	s.add(server, "read_file", "Read a UTF-8 text file in the sandbox.", s.readTool)
	s.add(server, "write_file", "Write UTF-8 text inside the sandbox and return the resulting file entry. Changes stay in the sandbox.", s.writeTool)
	s.add(server, "destroy_sandbox", "Destroy the sandbox and discard its local sync manifest.", s.destroyTool)
}

func (s *sandboxTools) add[In interface{ sandboxID() string }](server *mcp.Server, name, description string, handle func(context.Context, In) (toolOutput, error)) {
	mcp.AddTool(server, &mcp.Tool{Name: name, Description: description}, func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		id := in.sandboxID()
		if !filepath.IsLocal(id) || id == "." || filepath.Base(id) != id || strings.ContainsAny(id, "/\\") {
			return respond(toolOutput{}, errors.New("invalid_request: invalid sandbox ID"))
		}
		receipt, err := s.openReceipt(ctx, id)
		if err != nil {
			return respond(toolOutput{}, err)
		}
		defer func() { _ = receipt.Close() }()
		if err := s.renew(ctx, id); err != nil {
			return respond(toolOutput{}, err)
		}
		if name != "sync_workspace" && name != "destroy_sandbox" {
			if err := syscall.Flock(int(receipt.Fd()), syscall.LOCK_UN); err != nil {
				return respond(toolOutput{}, err)
			}
		}
		out, err := handle(ctx, in)
		return respond(out, err)
	})
}

func respond(out toolOutput, err error) (*mcp.CallToolResult, any, error) {
	if err != nil {
		if isConnectionRefused(err) {
			err = fmt.Errorf("daemon_unreachable: run sandboxctl doctor: %w", err)
		} else if sdk, ok := errors.AsType[*sandbox.Error](err); ok {
			err = fmt.Errorf("%s: %w", sdk.Code, err)
		}
		return nil, nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: out.summary}}}, out.value, nil
}

func (s *sandboxTools) create(ctx context.Context, in createInput) (toolOutput, error) {
	image := cmp.Or(in.Image, s.images[0])
	network := cmp.Or(in.Network, "none")
	if !slices.Contains(s.images, image) || !slices.Contains(s.networks, network) {
		return toolOutput{}, errors.New("policy_denied: image or network is outside the MCP configuration")
	}
	sb, err := s.client.CreateSandbox(ctx, sandbox.CreateSandboxRequest{Image: &image, MemoryMB: in.MemoryMB, Network: &network, TimeoutSeconds: &s.ttl})
	if err != nil {
		return toolOutput{}, err
	}
	if err := s.saveReceipt(sb.ID); err != nil {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_, killErr := s.client.KillSandbox(cleanup, sb.ID)
		return toolOutput{}, errors.Join(fmt.Errorf("cannot save ownership receipt for %s: %w", sb.ID, err), killErr)
	}
	return toolOutput{"Created " + sb.ID + " with network " + sb.Network, map[string]any{"sandbox_id": sb.ID, "network": sb.Network, "expires_at": sb.ExpiresAt, "protected": sb.Protected}}, nil
}

func (s *sandboxTools) readTool(ctx context.Context, in fileInput) (toolOutput, error) {
	data, err := s.client.ReadFile(ctx, in.SandboxID, in.Path)
	if err != nil {
		return toolOutput{}, err
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return toolOutput{}, errors.New("payload_too_large: binary file cannot be returned as MCP text")
	}
	return toolOutput{fmt.Sprintf("Read %d bytes from %s", len(data), in.Path), map[string]string{"content": string(data)}}, nil
}

func (s *sandboxTools) writeTool(ctx context.Context, in writeInput) (toolOutput, error) {
	entry, err := s.client.WriteFileInfo(ctx, in.SandboxID, in.Path, []byte(in.Content))
	return toolOutput{"Wrote " + in.Path, entry}, err
}

func (s *sandboxTools) destroyTool(ctx context.Context, in sandboxInput) (toolOutput, error) {
	sb, err := s.client.KillSandbox(ctx, in.SandboxID)
	if err != nil {
		return toolOutput{}, err
	}
	if err := s.forgetReceipt(sb.ID); err != nil {
		return toolOutput{}, err
	}
	return toolOutput{"Destroyed " + sb.ID, map[string]any{"state": sb.State}}, nil
}
