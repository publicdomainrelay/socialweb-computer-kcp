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

	crawlTimeout = 10 * time.Second
)

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
	period := c.Period
	if period <= 0 {
		period = CrawlPeriod
	}
	if len(c.Relays) == 0 || c.Address == nil {
		log.Warn("the PDS is not registered with a relay", "relays", len(c.Relays))
		return
	}
	ticker := time.NewTicker(period)
	defer ticker.Stop()
	for {
		c.once(ctx, log)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (c Crawl) once(ctx context.Context, log *slog.Logger) {
	raw := strings.TrimSpace(c.Address())
	if raw == "" {
		return
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		log.Warn("the PDS Exposure has no URL", "url", raw)
		return
	}
	registered := false
	for _, relay := range c.Relays {
		if xrpc.RequestCrawl(ctx, &xrpc.Client{Host: relay, HTTP: &http.Client{Timeout: crawlTimeout}}, parsed.Host) == nil {
			registered = true
		}
	}
	if registered && c.Crawled != nil {
		c.Crawled(ctx)
	}
}
