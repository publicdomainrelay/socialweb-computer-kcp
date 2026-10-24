package ingressrelaymain

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/ingress"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/notify"
)

const (
	DefaultHostname   = "xrpc.fedproxy.com"
	DefaultListen     = "0.0.0.0:8080"
	LoopbackHost      = "127.0.0.1"
	SocketMode        = 0o755
	MaxSocketPath     = 107
	ShutdownGrace     = 5 * time.Second
	readHeaderTimeout = 10 * time.Second
)

type Config struct {
	Hostname          string
	ServiceID         string
	Listen            string
	UnixSocket        string
	TLSCertFile       string
	TLSKeyFile        string
	PLCURL            string
	AdditionalHosts   []string
	AllowedDIDs       []string
	RelayTimeout      time.Duration
	KeepaliveInterval time.Duration
	KeepaliveTimeout  time.Duration
	NonceTTL          time.Duration
	MaxBodyBytes      int64
	ExpireSessions    bool
	Version           string
	Log               *slog.Logger
}

func SplitList(raw string) []string {
	var out []string
	for part := range strings.SplitSeq(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func AdditionalHosts(configured []string) []string {
	if slices.Contains(configured, "*") {
		return []string{"*"}
	}
	out := []string{LoopbackHost}
	for _, h := range configured {
		if !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	return out
}

func Run(ctx context.Context, cfg Config) error {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	if (cfg.TLSCertFile == "") != (cfg.TLSKeyFile == "") {
		return errors.New("ingress relay: tls-cert-file and tls-key-file go together")
	}
	hostname := strings.TrimSpace(cfg.Hostname)
	if hostname == "" {
		hostname = DefaultHostname
	}
	additional := AdditionalHosts(cfg.AdditionalHosts)
	server, err := ingress.NewServer(ingress.Config{
		Hostname:          hostname,
		ServiceID:         cfg.ServiceID,
		RelayTimeout:      cfg.RelayTimeout,
		KeepaliveInterval: cfg.KeepaliveInterval,
		KeepaliveTimeout:  cfg.KeepaliveTimeout,
		NonceTTL:          cfg.NonceTTL,
		AdditionalHosts:   additional,
		AllowedDIDs:       cfg.AllowedDIDs,
		PLCURL:            cfg.PLCURL,
		Logger:            log,
		Version:           cfg.Version,
		AccessLog:         true,
		OutliveToken:      !cfg.ExpireSessions,
		MaxBodyBytes:      cfg.MaxBodyBytes,
	})
	if err != nil {
		return err
	}
	defer server.Close()

	listener, address, err := listen(cfg)
	if err != nil {
		return err
	}
	httpServer := &http.Server{Handler: server.Handler(), ReadHeaderTimeout: readHeaderTimeout}
	tlsOn := cfg.UnixSocket == "" && cfg.TLSCertFile != ""
	served := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() {
		if tlsOn {
			served <- httpServer.ServeTLS(listener, cfg.TLSCertFile, cfg.TLSKeyFile)
			return
		}
		served <- httpServer.Serve(listener)
	})
	log.Info("serve listening", "address", address, "hostname", hostname, "additionalHosts", additional,
		"allowedDids", len(cfg.AllowedDIDs), "tls", tlsOn, "expireSessions", cfg.ExpireSessions, "version", cfg.Version)
	if err := notify.Ready(); err != nil {
		log.Warn("sd_notify ready failed", "err", err)
	}

	var runErr error
	select {
	case <-ctx.Done():
		log.Info("shutting down", "reason", context.Cause(ctx))
	case err := <-served:
		if !errors.Is(err, http.ErrServerClosed) {
			runErr = fmt.Errorf("ingress relay: serving %s: %w", address, err)
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ShutdownGrace)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
	server.Close()
	_ = httpServer.Close()
	wg.Wait()
	log.Info("stopped")
	return runErr
}

func listen(cfg Config) (net.Listener, string, error) {
	if path := cfg.UnixSocket; path != "" {
		if len(path) > MaxSocketPath {
			return nil, "", fmt.Errorf("ingress relay: unix socket path %s is longer than %d bytes", path, MaxSocketPath)
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, "", fmt.Errorf("ingress relay: removing the old socket %s: %w", path, err)
		}
		ln, err := net.Listen("unix", path)
		if err != nil {
			return nil, "", fmt.Errorf("ingress relay: listening on %s: %w", path, err)
		}
		if err := os.Chmod(path, SocketMode); err != nil {
			_ = ln.Close()
			return nil, "", fmt.Errorf("ingress relay: chmod %s: %w", path, err)
		}
		return ln, "unix:" + path, nil
	}
	address := cfg.Listen
	if address == "" {
		address = DefaultListen
	}
	ln, err := net.Listen("tcp", address)
	if err != nil {
		return nil, "", fmt.Errorf("ingress relay: listening on %s: %w", address, err)
	}
	return ln, ln.Addr().String(), nil
}
