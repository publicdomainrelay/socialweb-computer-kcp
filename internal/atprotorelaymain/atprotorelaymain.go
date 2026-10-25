package atprotorelaymain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/notify"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/relay/local"
)

const (
	DefaultListen     = "127.0.0.1:2584"
	StateFileName     = "relay-state.json"
	SeedHostsFileName = "seed-hosts.json"
	ShutdownGrace     = 5 * time.Second
	ClientTimeout     = 15 * time.Second
	readHeaderTimeout = 10 * time.Second
)

type Config struct {
	Listen        string
	StateDir      string
	PublicURL     string
	PLCURL        string
	AllowedHosts  []string
	SeedHostsFile string
	RecrawlWindow time.Duration
	IdleAfter     time.Duration
	MaxBacklog    int
	StartSeq      int64
	InsecureHTTP  bool
	Version       string
	Log           *slog.Logger
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

func Hostname(publicURL string) (string, error) {
	raw := strings.TrimSpace(publicURL)
	if raw == "" {
		return "", errors.New("atproto relay: --atproto-relay-public-url is required")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" && parsed.Scheme != "http" || parsed.Hostname() == "" || parsed.Path != "" && parsed.Path != "/" {
		return "", fmt.Errorf("atproto relay: --atproto-relay-public-url %q is not an http(s) origin", raw)
	}
	return strings.ToLower(parsed.Host), nil
}

func Run(ctx context.Context, cfg Config) error {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	hostname, err := Hostname(cfg.PublicURL)
	if err != nil {
		return err
	}
	stateDir := strings.TrimSpace(cfg.StateDir)
	if stateDir == "" {
		return errors.New("atproto relay: --state-dir is required")
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("atproto relay: state dir %s: %w", stateDir, err)
	}
	seed := strings.TrimSpace(cfg.SeedHostsFile)
	if seed == "" {
		seed = filepath.Join(stateDir, SeedHostsFileName)
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	relay, err := local.New(runCtx, local.Config{
		Hostname:      hostname,
		PLCURL:        cfg.PLCURL,
		InsecureHTTP:  cfg.InsecureHTTP,
		AllowedHosts:  cfg.AllowedHosts,
		StateFile:     filepath.Join(stateDir, StateFileName),
		SeedHostsFile: seed,
		Recrawl:       true,
		RecrawlWindow: cfg.RecrawlWindow,
		IdleAfter:     cfg.IdleAfter,
		MaxBacklog:    cfg.MaxBacklog,
		StartSeq:      cfg.StartSeq,
	}, local.Deps{
		Log:        log,
		HTTPClient: &http.Client{Timeout: ClientTimeout},
	})
	if err != nil {
		return fmt.Errorf("atproto relay: opening the state in %s: %w", stateDir, err)
	}

	address := strings.TrimSpace(cfg.Listen)
	if address == "" {
		address = DefaultListen
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		relay.Core().Close()
		return fmt.Errorf("atproto relay: listening on %s: %w", address, err)
	}
	httpServer := &http.Server{Handler: relay.Handler(), ReadHeaderTimeout: readHeaderTimeout}
	var wg sync.WaitGroup
	ran := make(chan struct{})
	wg.Go(func() {
		defer close(ran)
		_ = relay.Run(runCtx)
	})
	served := make(chan error, 1)
	wg.Go(func() { served <- httpServer.Serve(listener) })
	log.Info("serve listening", "address", listener.Addr().String(), "hostname", hostname, "stateDir", stateDir,
		"allowedHosts", cfg.AllowedHosts, "head", relay.Core().Sequencer().Head(), "recrawlWindow", cfg.RecrawlWindow.String(),
		"idleAfter", cfg.IdleAfter.String(), "insecureHTTP", cfg.InsecureHTTP, "version", cfg.Version)
	if err := notify.Ready(); err != nil {
		log.Warn("sd_notify ready failed", "err", err)
	}

	var runErr error
	select {
	case <-ctx.Done():
		log.Info("shutting down", "reason", context.Cause(ctx))
	case err := <-served:
		if !errors.Is(err, http.ErrServerClosed) {
			runErr = fmt.Errorf("atproto relay: serving %s: %w", address, err)
		}
	case <-ran:
		runErr = errors.New("atproto relay: the relay stopped")
	}
	shutdownCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), ShutdownGrace)
	defer stop()
	_ = httpServer.Shutdown(shutdownCtx)
	cancel()
	<-ran
	_ = httpServer.Close()
	wg.Wait()
	log.Info("stopped", "head", relay.Core().Sequencer().Head())
	return runErr
}
