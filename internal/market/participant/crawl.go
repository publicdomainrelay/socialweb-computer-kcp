package participant

import (
	"context"
	"log/slog"
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
}

type Crawl struct {
	Relays []string

	Address func() string

	Crawled func(ctx context.Context)

	Period time.Duration

	Log *slog.Logger
}

func (c Crawl) Run(ctx context.Context) {
	log := c.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if len(c.Relays) == 0 || c.Address == nil {
		log.Warn("the PDS is not registered with a relay", "relays", len(c.Relays))
		return
	}
	state := &crawlState{}
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

func (c Crawl) once(ctx context.Context, log *slog.Logger, state *crawlState) {
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
