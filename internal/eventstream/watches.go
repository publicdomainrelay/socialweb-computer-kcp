package eventstream

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/ttlcache"
)

const pdsCacheTTL = 2 * time.Minute

type WatchesConfig struct {
	Directory identity.Directory

	PLCDirectoryURL string

	External *http.Client

	HTTPClient *http.Client

	FlushDelay time.Duration

	Log *slog.Logger
}

type Watches struct {
	log *slog.Logger

	directory identity.Directory

	http *http.Client

	flushDelay time.Duration

	pdsCache *ttlcache.Cache[syntax.DID, pdsclient.PDS]
}

func NewWatches(cfg WatchesConfig) *Watches {
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	httpClient := httpClientWithFallback(cfg.HTTPClient, cfg.External)
	directory := cfg.Directory
	if directory == nil {
		directory = directoryAt(httpClient, cfg.PLCDirectoryURL)
	}
	return &Watches{log: log.With("component", "atprotorecordwatch-backend"), directory: directory, http: httpClient,
		flushDelay: cfg.FlushDelay, pdsCache: ttlcache.New[syntax.DID, pdsclient.PDS](ttlcache.Options{TTL: pdsCacheTTL})}
}

func directoryAt(httpClient *http.Client, plcURL string) identity.Directory {
	public := &identity.BaseDirectory{PLCURL: identity.DefaultPLCURL, HTTPClient: *httpClient, PLCClient: httpClient, SkipHandleVerification: true}
	plcURL = strings.TrimSpace(plcURL)
	if plcURL == "" {
		return fallbackDirectory{public}
	}
	local := &identity.BaseDirectory{PLCURL: plcURL, HTTPClient: *httpClient, PLCClient: httpClient, SkipHandleVerification: true}
	return fallbackDirectory{local, public}
}
