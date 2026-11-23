package main

import (
	"context"
	"io"
	"net"
	"os"
	"time"
)

const egressSocket = "/run/tiny-sandbox/egress.sock"

func startProxyListener() error {
	local, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp4", "127.0.0.1:8888")
	if err != nil {
		return err
	}
	control, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", egressSocket)
	if err != nil {
		_ = local.Close()
		return err
	}
	go func() {
		defer func() { _ = local.Close() }()
		defer func() { _ = control.Close() }()
		for {
			app, err := local.Accept()
			if err != nil {
				return
			}
			_ = control.(*net.UnixListener).SetDeadline(time.Now().Add(8 * time.Second))
			host, err := control.Accept()
			if err != nil {
				_ = app.Close()
				continue
			}
			go relayProxy(app, host)
		}
	}()
	return nil
}

func relayProxy(app, host net.Conn) {
	defer func() { _ = app.Close() }()
	defer func() { _ = host.Close() }()
	if _, err := host.Write([]byte{1}); err != nil {
		return
	}
	done := make(chan struct{})
	go func() { _, _ = io.Copy(host, app); _ = host.(*net.UnixConn).CloseWrite(); close(done) }()
	_, _ = io.Copy(app, host)
	_ = app.Close()
	<-done
}

func proxyAccept() error {
	conn, err := (&net.Dialer{}).DialContext(context.Background(), "unix", egressSocket)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	go func() { _, _ = io.Copy(conn, os.Stdin); _ = conn.Close() }()
	_, err = io.Copy(os.Stdout, conn)
	return err
}
