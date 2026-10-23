package local

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"

	atrelay "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/relay"
	contract "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/relay"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/clock"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/debounce"
)

const (
	FlushDelay = time.Second

	CrawlTimeout = 15 * time.Second

	MinCrawlBackoff = time.Second

	MaxCrawlBackoff = 30 * time.Second
)

type Config struct {
	Hostname string

	PLCURL string

	InsecureHTTP bool

	AllowedHosts []string

	StateFile string

	CrawlInterval time.Duration

	TLS *tls.Config
}

type Deps struct {
	Log *slog.Logger

	HTTPClient *http.Client

	DialContext func(ctx context.Context, network, address string) (net.Conn, error)

	BaseContext context.Context
}

type Relay struct {
	cfg Config

	log *slog.Logger

	core *atrelay.Relay

	store atrelay.Store

	file *fileStore

	flush *debounce.Debounce

	mu sync.Mutex

	ctx context.Context

	hosts map[string]context.CancelFunc

	crawls sync.WaitGroup
}

var _ contract.Relay = (*Relay)(nil)

func New(cfg Config, deps Deps) (*Relay, error) {
	log := deps.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	r := &Relay{cfg: cfg, log: log, hosts: map[string]context.CancelFunc{}}
	r.store = atrelay.NewMemoryStore()
	if path := strings.TrimSpace(cfg.StateFile); path != "" {
		fs, err := openFileStore(path)
		if err != nil {
			return nil, err
		}
		r.file = fs
		r.store = fs
		r.flush = debounce.New(FlushDelay, r.flushFile)
		fs.changed = r.flush.Mark
	}
	var directory identity.Directory
	plcURL := strings.TrimSuffix(strings.TrimSpace(cfg.PLCURL), "/")
	if plcURL != "" && deps.HTTPClient != nil {
		base := &identity.BaseDirectory{
			PLCURL:                 plcURL,
			HTTPClient:             *deps.HTTPClient,
			PLCClient:              deps.HTTPClient,
			SkipHandleVerification: true,
		}
		directory = identity.NewCacheDirectory(base, 100_000, time.Hour, 2*time.Minute, 5*time.Minute)
	}
	r.core = atrelay.New(atrelay.Config{
		Hostname:     cfg.Hostname,
		Logger:       log,
		Store:        r.store,
		Directory:    directory,
		PLCURL:       cfg.PLCURL,
		InsecureHTTP: cfg.InsecureHTTP,
		AllowedHosts: append([]string(nil), cfg.AllowedHosts...),
		HTTPClient:   deps.HTTPClient,
		DialContext:  deps.DialContext,
		TLSConfig:    cfg.TLS,
		BaseContext:  deps.BaseContext,
	})
	return r, nil
}

func (r *Relay) Handler() http.Handler { return r.core.Handler() }

func (r *Relay) Core() *atrelay.Relay { return r.core }

func (r *Relay) Store() atrelay.Store { return r.store }

func (r *Relay) Crawl(hostname string) {
	hostname = strings.TrimSpace(hostname)
	if hostname == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if cancel := r.hosts[hostname]; cancel != nil {
		cancel()
	}
	r.hosts[hostname] = nil
	if r.ctx != nil {
		r.startLocked(hostname)
	}
}

func (r *Relay) Forget(hostname string) {
	hostname = strings.TrimSpace(hostname)
	r.mu.Lock()
	defer r.mu.Unlock()
	if cancel := r.hosts[hostname]; cancel != nil {
		cancel()
	}
	delete(r.hosts, hostname)
}

func (r *Relay) Run(ctx context.Context) error {
	r.mu.Lock()
	r.ctx = ctx
	for hostname := range r.hosts {
		r.startLocked(hostname)
	}
	r.mu.Unlock()
	if r.cfg.CrawlInterval > 0 {
		r.refresh(ctx, r.cfg.CrawlInterval)
	} else {
		<-ctx.Done()
	}
	r.mu.Lock()
	r.ctx = nil
	for hostname, cancel := range r.hosts {
		if cancel != nil {
			cancel()
		}
		r.hosts[hostname] = nil
	}
	r.mu.Unlock()
	r.crawls.Wait()
	r.core.Close()
	if r.file != nil {
		r.flush.Stop()
		r.flushFile()
	}
	return nil
}

func (r *Relay) refresh(ctx context.Context, period time.Duration) {
	ticker := time.NewTicker(period)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.mu.Lock()
			hostnames := make([]string, 0, len(r.hosts))
			for hostname := range r.hosts {
				hostnames = append(hostnames, hostname)
			}
			r.mu.Unlock()
			for _, hostname := range hostnames {
				r.Crawl(hostname)
			}
		}
	}
}

func (r *Relay) startLocked(hostname string) {
	ctx, cancel := context.WithCancel(r.ctx)
	r.hosts[hostname] = cancel
	r.crawls.Go(func() {
		defer cancel()
		r.crawl(ctx, hostname)
	})
}

func (r *Relay) crawl(ctx context.Context, hostname string) {
	backoff := time.Duration(0)
	for {
		reqCtx, cancel := context.WithTimeout(ctx, CrawlTimeout)
		err := r.core.RequestCrawl(reqCtx, hostname)
		cancel()
		if err == nil {
			r.log.Info("relay_crawl_started", "hostname", hostname)
			return
		}
		if ctx.Err() != nil {
			return
		}
		backoff = min(max(backoff*2, MinCrawlBackoff), MaxCrawlBackoff)
		r.log.Warn("relay_crawl_failed", "hostname", hostname, "err", err, "retry", backoff)
		if !clock.Sleep(ctx, backoff) {
			return
		}
	}
}

func (r *Relay) flushFile() {
	if err := r.file.flush(); err != nil {
		r.log.Warn("relay_state_flush_failed", "path", r.file.path, "err", err)
		r.flush.Mark()
	}
}
