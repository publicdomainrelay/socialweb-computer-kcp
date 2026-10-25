package participant

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/xrpc"
)

const (
	CrawlPeriod = 10 * time.Second

	CrawlRefresh = 10 * time.Minute

	crawlTimeout = 10 * time.Second
)

type crawlState struct {
	announced string

	at time.Time

	public map[string]crawlState
}

type Crawl struct {
	Relays []string

	Address func() string

	PublicRelays []string

	PublicAddress func(ctx context.Context) string

	Crawled func(ctx context.Context)

	Period time.Duration

	Log *slog.Logger
}

func (c Crawl) Run(ctx context.Context) {
	log := c.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if (len(c.Relays) == 0 || c.Address == nil) && (len(c.PublicRelays) == 0 || c.PublicAddress == nil) {
		log.Warn("the PDS is not registered with a relay", "relays", len(c.Relays), "publicRelays", len(c.PublicRelays))
		return
	}
	state := &crawlState{public: map[string]crawlState{}}
	if c.Period <= 0 {
		c.once(ctx, log, state)
		return
	}
	ticker := time.NewTicker(c.Period)
	defer ticker.Stop()
	for {
		c.once(ctx, log, state)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func PublicCrawlHost(raw string) (string, bool) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.Port() != "" || parsed.Path != "" && parsed.Path != "/" {
		return "", false
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" || net.ParseIP(host) != nil || !strings.Contains(host, ".") || host == "localhost" || strings.HasSuffix(host, ".localhost") ||
		strings.HasSuffix(host, ".local") {
		return "", false
	}
	return host, true
}

func (c Crawl) once(ctx context.Context, log *slog.Logger, state *crawlState) {
	c.publicOnce(ctx, log, state)
	if len(c.Relays) == 0 || c.Address == nil {
		return
	}
	raw := strings.TrimSpace(c.Address())
	if raw == "" {
		return
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		log.Warn("the PDS Exposure has no URL", "url", raw)
		return
	}
	if state.announced == parsed.Host && time.Since(state.at) < CrawlRefresh {
		return
	}
	registered := false
	for _, relay := range c.Relays {
		if xrpc.RequestCrawl(ctx, &xrpc.Client{Host: relay, HTTP: &http.Client{Timeout: crawlTimeout}}, parsed.Host) == nil {
			registered = true
		}
	}
	if !registered {
		state.announced = ""
		return
	}
	first := state.announced != parsed.Host
	state.announced, state.at = parsed.Host, time.Now()
	if first && c.Crawled != nil {
		c.Crawled(ctx)
	}
}

func (c Crawl) publicOnce(ctx context.Context, log *slog.Logger, state *crawlState) {
	if len(c.PublicRelays) == 0 || c.PublicAddress == nil {
		return
	}
	var due []string
	for _, relay := range c.PublicRelays {
		if last, ok := state.public[relay]; !ok || time.Since(last.at) >= CrawlRefresh {
			due = append(due, relay)
		}
	}
	if len(due) == 0 {
		return
	}
	raw := strings.TrimSpace(c.PublicAddress(ctx))
	if raw == "" {
		return
	}
	host, ok := PublicCrawlHost(raw)
	for _, relay := range due {
		state.public[relay] = crawlState{announced: raw, at: time.Now()}
		if !ok {
			log.Info("participant_request_crawl_skipped", "relay", relay, "pds", raw, "reason", "not a public https host without a port")
			continue
		}
		err := xrpc.RequestCrawl(ctx, &xrpc.Client{Host: relay, HTTP: &http.Client{Timeout: crawlTimeout}}, host)
		if err != nil {
			log.Warn("participant_request_crawl", "relay", relay, "hostname", host, "ok", false, "err", err)
			continue
		}
		log.Info("participant_request_crawl", "relay", relay, "hostname", host, "ok", true)
	}
}
