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

	RecrawlParallel = 8
)

type Config struct {
	Hostname string

	PLCURL string

	InsecureHTTP bool

	AllowedHosts []string

	StateFile string

	CrawlInterval time.Duration

	TLS *tls.Config

	SeedHostsFile string

	Recrawl bool

	RecrawlWindow time.Duration

	IdleAfter time.Duration

	MaxBacklog int

	StartSeq int64
}

type Deps struct {
	Log *slog.Logger

	HTTPClient *http.Client

	DialContext func(ctx context.Context, network, address string) (net.Conn, error)
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

func New(ctx context.Context, cfg Config, deps Deps) (*Relay, error) {
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
	if path := strings.TrimSpace(cfg.SeedHostsFile); path != "" {
		added, err := seedHosts(r.store, path)
		if err != nil {
			return nil, err
		}
		log.Info("relay_seed_hosts", "path", path, "added", added)
	}
	startSeq := cfg.StartSeq
	if r.file != nil {
		startSeq = max(startSeq, r.file.seq)
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
	r.core = atrelay.New(ctx, atrelay.Config{
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
		MaxBacklog:   cfg.MaxBacklog,
		StartSeq:     startSeq,
		IdleAfter:    cfg.IdleAfter,
	})
	if r.file != nil {
		r.file.head = r.core.Sequencer().Head
	}
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
	if r.cfg.Recrawl {
		r.recrawl(ctx)
	}
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

func (r *Relay) recrawl(ctx context.Context) {
	cutoff := time.Now().Add(-r.cfg.RecrawlWindow).UnixMilli()
	known := r.store.ListHosts()
	var due []string
	r.mu.Lock()
	for _, h := range known {
		if _, managed := r.hosts[h.Hostname]; managed {
			continue
		}
		switch h.State {
		case atrelay.HostTakedown:
		case atrelay.HostIdle:
			if r.cfg.RecrawlWindow > 0 && h.LastSeen >= cutoff {
				due = append(due, h.Hostname)
			}
		default:
			due = append(due, h.Hostname)
		}
	}
	r.mu.Unlock()
	r.log.Info("relay_recrawl", "known", len(known), "due", len(due), "window", r.cfg.RecrawlWindow.String())
	slots := make(chan struct{}, RecrawlParallel)
	for _, hostname := range due {
		r.crawls.Go(func() {
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-slots }()
			reqCtx, cancel := context.WithTimeout(ctx, CrawlTimeout)
			err := r.core.RequestCrawl(reqCtx, hostname)
			cancel()
			if err != nil {
				if ctx.Err() == nil {
					r.core.SetHostState(hostname, atrelay.HostIdle)
					r.log.Warn("relay_recrawl_failed", "hostname", hostname, "err", err)
				}
				return
			}
			r.log.Info("relay_recrawl_started", "hostname", hostname)
		})
	}
}

func (r *Relay) flushFile() {
	if err := r.file.flush(); err != nil {
		r.log.Warn("relay_state_flush_failed", "path", r.file.path, "err", err)
		r.flush.Mark()
	}
}
