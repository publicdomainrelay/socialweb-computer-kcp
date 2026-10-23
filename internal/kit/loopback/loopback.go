package loopback

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/ingress"
)

const DefaultHost = "127.0.0.1"

type Config struct {
	Host string

	Protocol string
}

type Bridge struct {
	cfg Config

	target ingress.Target

	listener net.Listener

	server *http.Server

	ctx context.Context

	cancel context.CancelFunc

	done chan struct{}

	closeOnce sync.Once

	conns sync.WaitGroup
}

func Start(ctx context.Context, cfg Config, target ingress.Target) (*Bridge, error) {
	if target == nil {
		return nil, errors.New("loopback: the target is not configured")
	}
	host := strings.TrimSpace(cfg.Host)
	host = cmp.Or(host, DefaultHost)
	listener, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return nil, fmt.Errorf("loopback: listening on %s: %w", host, err)
	}
	ctx, cancel := context.WithCancel(ctx)
	bridge := &Bridge{cfg: cfg, target: target, listener: listener, ctx: ctx, cancel: cancel, done: make(chan struct{})}
	switch strings.ToLower(strings.TrimSpace(cfg.Protocol)) {
	case component.ProtocolHTTP, component.ProtocolWS:
		bridge.server = &http.Server{Handler: target, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			defer close(bridge.done)
			_ = bridge.server.Serve(listener)
		}()
	case component.ProtocolTCP, component.ProtocolSSH:
		go bridge.serveStreams()
	default:
		cancel()
		_ = listener.Close()
		return nil, fmt.Errorf("loopback: unknown protocol %q", cfg.Protocol)
	}
	return bridge, nil
}

func (b *Bridge) Address() string {
	return b.listener.Addr().String()
}

func (b *Bridge) serveStreams() {
	defer close(b.done)
	for {
		conn, err := b.listener.Accept()
		if err != nil {
			return
		}
		b.conns.Go(func() { b.bridge(conn) })
	}
}

func (b *Bridge) bridge(conn net.Conn) {
	defer conn.Close()
	stop := context.AfterFunc(b.ctx, func() { _ = conn.Close() })
	defer stop()
	stream, err := b.target.DialStream(b.ctx)
	if err != nil {
		return
	}
	defer stream.Close()
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(stream, conn)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(conn, stream)
		done <- struct{}{}
	}()
	<-done
	_ = conn.Close()
	_ = stream.Close()
	<-done
}

func (b *Bridge) Close() error {
	b.closeOnce.Do(func() {
		b.cancel()
		if b.server != nil {
			_ = b.server.Close()
			return
		}
		_ = b.listener.Close()
	})
	<-b.done
	b.conns.Wait()
	return nil
}
