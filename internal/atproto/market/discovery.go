package market

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

const (
	RelayDiscoveryCacheTTL = 180 * time.Second
	OfferingsCacheTTL      = 120 * time.Second
	HeartbeatMaxSkew       = 60 * time.Second
)

type RelayFailure struct {
	URL    string
	Reason string
}

type RelayVisibilityResult struct {
	OK            bool
	CapableRelays []string
	IndexedBy     []string
	Failures      []RelayFailure
}

type RelayVisibilityOptions struct {
	RelayURLs    []string
	BidderDID    string
	Collection   string
	Log          LogFunc
	PollTimeout  time.Duration
	PollInterval time.Duration
	Client       HTTPDoer
}

func relayCollectionURL(relayURL, collection string) string {
	return strings.TrimRight(relayURL, "/") +
		"/xrpc/com.atproto.sync.listReposByCollection?collection=" +
		url.QueryEscape(collection) + "&limit=1000"
}

func relayGET(ctx context.Context, client HTTPDoer, rawURL string) (int, []byte, error) {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, nil, err
	}
	res, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<22))
	if err != nil {
		return res.StatusCode, nil, err
	}
	return res.StatusCode, body, nil
}

func relayDIDs(body []byte) []string {
	var parsed struct {
		Repos []struct {
			DID string `json:"did"`
		} `json:"repos"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil
	}
	out := make([]string, 0, len(parsed.Repos))
	for _, repo := range parsed.Repos {
		if repo.DID != "" {
			out = append(out, repo.DID)
		}
	}
	return out
}

func VerifyRelayVisibility(ctx context.Context, opts RelayVisibilityOptions) RelayVisibilityResult {
	logf := opts.Log
	if logf == nil {
		logf = NoopLog
	}
	client := opts.Client
	if client == nil {
		client = http.DefaultClient
	}
	timeout := opts.PollTimeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	interval := opts.PollInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	collectionPath := "/xrpc/com.atproto.sync.listReposByCollection?collection=" +
		url.QueryEscape(opts.Collection) + "&limit=1000"
	result := RelayVisibilityResult{CapableRelays: []string{}, IndexedBy: []string{}, Failures: []RelayFailure{}}
	for _, relay := range opts.RelayURLs {
		status, _, err := relayGET(ctx, client, strings.TrimRight(relay, "/")+collectionPath)
		switch {
		case err != nil:
			result.Failures = append(result.Failures, RelayFailure{URL: relay, Reason: err.Error()})
		case status == http.StatusOK:
			result.CapableRelays = append(result.CapableRelays, relay)
			logf("info", "relay_capable", map[string]any{"url": relay})
		case status == http.StatusNotFound:
			logf("info", "relay_no_collection_support", map[string]any{"url": relay})
			result.Failures = append(result.Failures, RelayFailure{URL: relay, Reason: "listReposByCollection not supported (404)"})
		default:
			result.Failures = append(result.Failures, RelayFailure{URL: relay, Reason: fmt.Sprintf("HTTP %d", status)})
		}
	}
	if len(result.CapableRelays) == 0 {
		logf("warn", "relay_no_capable_relays", map[string]any{"total": len(opts.RelayURLs), "failures": result.Failures})
		return result
	}
	logf("info", "relay_capable_relays", map[string]any{"count": len(result.CapableRelays), "relays": result.CapableRelays})

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) && len(result.IndexedBy) == 0 {
		for _, relay := range result.CapableRelays {
			status, body, err := relayGET(ctx, client, strings.TrimRight(relay, "/")+collectionPath)
			if err != nil || status != http.StatusOK {
				continue
			}
			if slices.Contains(relayDIDs(body), opts.BidderDID) {
				result.IndexedBy = append(result.IndexedBy, relay)
			}
		}
		if len(result.IndexedBy) == 0 {
			select {
			case <-ctx.Done():
				deadline = time.Now()
			case <-time.After(interval):
			}
		}
	}
	result.OK = len(result.IndexedBy) > 0
	if !result.OK {
		logf("warn", "relay_visibility_timeout", map[string]any{"capableRelays": result.CapableRelays, "pollTimeoutMs": timeout.Milliseconds(), "failures": result.Failures})
	} else {
		logf("info", "relay_visibility_confirmed", map[string]any{"indexedBy": result.IndexedBy, "capableRelays": result.CapableRelays})
	}
	return result
}

func DiscoverBiddersFromRelay(ctx context.Context, relayURL, collection string, timeout time.Duration, client HTTPDoer, logf LogFunc) []string {
	if logf == nil {
		logf = NoopLog
	}
	rawURL := relayCollectionURL(relayURL, collection)
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	logf("info", "relay_discovery_query", map[string]any{"url": rawURL, "collection": collection})
	status, body, err := relayGET(ctx, client, rawURL)
	if err != nil {
		logf("warn", "relay_discovery_error", map[string]any{"relayUrl": relayURL, "error": err.Error()})
		return nil
	}
	if status != http.StatusOK {
		logf("warn", "relay_discovery_http_error", map[string]any{"relayUrl": relayURL, "status": status, "collection": collection})
		return nil
	}
	seen := map[string]bool{}
	out := []string{}
	for _, did := range relayDIDs(body) {
		if seen[did] {
			continue
		}
		seen[did] = true
		out = append(out, did)
	}
	logf("info", "relay_discovery_result", map[string]any{"relayUrl": relayURL, "collection": collection, "count": len(out)})
	return out
}

func DiscoverBiddersFromRelays(ctx context.Context, relayURLs []string, collection string, timeout time.Duration, client HTTPDoer, logf LogFunc) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, relay := range relayURLs {
		for _, did := range DiscoverBiddersFromRelay(ctx, relay, collection, timeout, client, logf) {
			if seen[did] {
				continue
			}
			seen[did] = true
			out = append(out, did)
		}
	}
	return out
}

func AutoDiscoverRelayURLs(ctx context.Context, lister RecordLister, did string) []string {
	if lister == nil {
		return nil
	}
	urls := []string{}
	seen := map[string]bool{}
	cursor := ""
	for {
		page, err := lister.ListRecords(ctx, did, RelaysNSID, 100, cursor)
		if err != nil {
			return nil
		}
		for _, rec := range page.Records {
			raw, ok := rec.Value["relays"].([]any)
			if !ok {
				continue
			}
			for _, item := range raw {
				s, ok := item.(string)
				if !ok {
					continue
				}
				s = strings.TrimSpace(s)
				if !(strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://")) {
					continue
				}
				if seen[s] {
					continue
				}
				seen[s] = true
				urls = append(urls, s)
			}
		}
		if len(page.Records) == 0 || page.Cursor == "" {
			return urls
		}
		cursor = page.Cursor
	}
}

func ListOfferings(ctx context.Context, lister RecordLister, did string) ([]Offering, error) {
	if lister == nil {
		return nil, fmt.Errorf("%w: no record lister", ErrInvalidRecord)
	}
	out := []Offering{}
	cursor := ""
	for {
		page, err := lister.ListRecords(ctx, did, OfferingNSID, 100, cursor)
		if err != nil {
			return nil, err
		}
		for _, rec := range page.Records {
			offering, err := ParseOffering(rec.Value)
			if err != nil {
				return nil, err
			}
			offering.URI = rec.URI
			offering.CID = rec.CID
			out = append(out, *offering)
		}
		if page.Cursor == "" || len(page.Records) == 0 {
			return out, nil
		}
		cursor = page.Cursor
	}
}

func OfferingApplies(offering Offering, payloadNSID string) bool {
	return slices.Contains(offering.AppliesTo, payloadNSID)
}

func HeartbeatFresh(createdAt time.Time, now time.Time) bool {
	return now.Sub(createdAt) <= HeartbeatTTL
}

func ReadHeartbeat(ctx context.Context, getter RecordGetter, did string, now time.Time) bool {
	if getter == nil {
		return false
	}
	rec, err := getter.GetRecord(ctx, did, HeartbeatNSID, HeartbeatRKey)
	if err != nil {
		return false
	}
	value, _ := rec.Value["createdAt"].(string)
	at, err := ParseTime(value)
	if err != nil || at.After(now.Add(HeartbeatMaxSkew)) {
		return false
	}
	return HeartbeatFresh(at, now)
}

func FreshBidderDIDs(ctx context.Context, getter RecordGetter, dids []string, now time.Time) []string {
	out := make([]string, 0, len(dids))
	for _, did := range dids {
		if ReadHeartbeat(ctx, getter, did, now) {
			out = append(out, did)
		}
	}
	return out
}

func StartOfferingRefresh(ctx context.Context, interval time.Duration, refresh func(context.Context) error, logf LogFunc) (stop func()) {
	if logf == nil {
		logf = NoopLog
	}
	if interval <= 0 {
		interval = HeartbeatInterval
	}
	runCtx, cancel := context.WithCancel(ctx)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				if err := refresh(runCtx); err != nil {
					logf("warn", "offering refresh failed", map[string]any{"err": err.Error()})
				}
			}
		}
	}()
	return cancel
}
