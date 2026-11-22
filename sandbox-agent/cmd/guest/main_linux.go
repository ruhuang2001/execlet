package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/ruhuang2001/tiny-sandbox/internal/guestprotocol"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(args []string) error {
	if len(args) == 0 {
		return errors.New("missing guest subcommand")
	}
	switch args[0] {
	case "identity":
		groups, err := os.Getgroups()
		if err != nil {
			return err
		}
		identity := protocol.Identity{UID: uint32(os.Getuid()), GID: uint32(os.Getgid()), Home: os.Getenv("HOME")}
		identity.Dir, err = os.Getwd()
		if err != nil {
			return err
		}
		for _, g := range groups {
			identity.Groups = append(identity.Groups, uint32(g))
		}
		return json.MarshalWrite(os.Stdout, identity)
	case "daemon":
		f := flag.NewFlagSet("daemon", flag.ContinueOnError)
		path := f.String("socket", protocol.Socket, "control socket")
		identity := f.String("identity", "", "workload identity")
		filtered := f.Bool("filtered", false, "start loopback proxy")
		if err := f.Parse(args[1:]); err != nil {
			return err
		}
		var id protocol.Identity
		if err := json.Unmarshal([]byte(*identity), &id); err != nil {
			return err
		}
		if *filtered {
			if err := startProxyListener(); err != nil {
				return err
			}
		}
		runtime.GOMAXPROCS(1)
		return serve(*path, id)
	case "transfer":
		return transfer(args[1:])
	case "proxy-accept":
		return proxyAccept()
	case "connect":
		runtime.GOMAXPROCS(1)
		c, err := (&net.Dialer{}).DialContext(context.Background(), "unix", protocol.Socket)
		if err != nil {
			return err
		}
		defer func() { _ = c.Close() }()
		go func() { _, _ = io.Copy(c, os.Stdin); _ = c.(*net.UnixConn).CloseWrite() }()
		_, err = io.Copy(os.Stdout, c)
		return err
	case "run":
		if len(args) < 3 {
			return errors.New("missing workload command")
		}
		if err := os.Chdir(args[1]); err != nil {
			fmt.Fprintln(os.Stderr, "tiny-sandbox: cwd:", err)
			os.Exit(126)
		}
		file, err := exec.LookPath(args[2])
		if err == nil {
			err = syscall.Exec(file, args[2:], os.Environ())
		}
		fmt.Fprintln(os.Stderr, "tiny-sandbox: exec:", err)
		os.Exit(127)
	}
	return errors.New("unknown guest subcommand")
}
func serve(path string, id protocol.Identity) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return err
	}
	socket, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return err
	}
	defer func() { _ = socket.Close() }()
	if err := os.Chmod(path, 0600); err != nil {
		return err
	}
	s := &server{identity: id, jobs: make(map[string]*job)}
	for {
		c, err := socket.Accept()
		if err != nil {
			return err
		}
		go s.handle(c)
	}
}
func (s *server) handle(c net.Conn) {
	defer func() { _ = c.Close() }()
	for {
		kind, raw, err := protocol.Read(c)
		if err != nil {
			return
		}
		var req protocol.Request
		if err = json.Unmarshal(raw, &req); err != nil {
			return
		}
		var value any
		switch kind {
		case 'H':
			value = protocol.Result{Version: protocol.Version}
		case 'S':
			value, err = stat(req.Path)
		case 'L':
			value, err = list(req.Path, req.After, req.Limit)
		case 'C', 'K', 'P':
			value, err = s.dispatch(kind, req)
		default:
			return
		}
		if err != nil {
			reply, ok := errors.AsType[*protocol.Error](err)
			if !ok {
				reply = &protocol.Error{Code: "backend_error", Message: err.Error()}
				if errors.Is(err, os.ErrNotExist) {
					reply.Code = "not_found"
				}
			}
			if protocol.Write(c, 'e', reply) != nil {
				return
			}
		} else if protocol.Write(c, kind+32, value) != nil {
			return
		}
	}
}
