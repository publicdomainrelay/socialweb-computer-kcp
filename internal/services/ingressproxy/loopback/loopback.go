package loopback

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
)

const DefaultHost = "127.0.0.1"

type Config struct {
	Host string

	Protocol string

	Target component.Key

	Handler string

	Handlers *component.Handlers

	Caller commonv1.ObjectRef
}

type Bridge struct {
	cfg Config

	listener net.Listener

	server *http.Server

	ctx context.Context

	cancel context.CancelFunc

	done chan struct{}

	closeOnce sync.Once
}

func Start(cfg Config) (*Bridge, error) {
	host := strings.TrimSpace(cfg.Host)
	if host == "" {
		host = DefaultHost
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return nil, fmt.Errorf("loopback: listening on %s: %w", host, err)
	}
	ctx, cancel := context.WithCancel(bus.WithCaller(context.Background(), cfg.Caller))
	bridge := &Bridge{cfg: cfg, listener: listener, ctx: ctx, cancel: cancel, done: make(chan struct{})}
	switch strings.ToLower(strings.TrimSpace(cfg.Protocol)) {
	case component.ProtocolHTTP, component.ProtocolWS:
		if cfg.Handlers == nil {
			cancel()
			_ = listener.Close()
			return nil, errors.New("loopback: the handler registry is not configured")
		}
		bridge.server = &http.Server{Handler: bridge, ReadHeaderTimeout: 10 * time.Second}
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

func (b *Bridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	handler, ok := b.cfg.Handlers.HandlerNamed(b.cfg.Target, b.cfg.Handler)
	if !ok {
		http.Error(w, "the target handler is not registered", http.StatusServiceUnavailable)
		return
	}
	handler.ServeHTTP(w, r)
}

func (b *Bridge) serveStreams() {
	defer close(b.done)
	for {
		conn, err := b.listener.Accept()
		if err != nil {
			return
		}
		go b.bridge(conn)
	}
}

func (b *Bridge) bridge(conn net.Conn) {
	defer conn.Close()
	stream, err := b.cfg.Handlers.DialStream(b.ctx, b.cfg.Target, b.cfg.Handler)
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
	return nil
}
