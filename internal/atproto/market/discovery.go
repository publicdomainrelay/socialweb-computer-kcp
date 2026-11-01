package market

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/xrpc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/ttlcache"
)

const (
	RelayDiscoveryCacheTTL = 180 * time.Second
	OfferingsCacheTTL      = 120 * time.Second
	RelaysCacheTTL         = 60 * time.Second
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
	req.Header.Set("Accept", "application/json")
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
	timeout = opt.Positive(timeout, 15*time.Second)
	interval := opts.PollInterval
	interval = opt.Positive(interval, 2*time.Second)
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

var relaysOf = ttlcache.New[string, []string](ttlcache.Options{TTL: RelaysCacheTTL, MaxEntries: 1024})

func RelayURLsOf(ctx context.Context, getter RecordGetter, did string) []string {
	if getter == nil || did == "" {
		return nil
	}
	urls, err := relaysOf.GetOrLoad(ctx, did, func(ctx context.Context) ([]string, time.Duration, error) {
		rec, err := getter.GetRecord(ctx, did, RelaysNSID, RelaysRKey)
		if RecordAbsent(err) {
			return []string{}, RelaysCacheTTL, nil
		}
		if err != nil {
			return nil, 0, err
		}
		return relayURLsIn(rec.Value), RelaysCacheTTL, nil
	})
	if err != nil {
		return nil
	}
	return slices.Clone(urls)
}

func RecordAbsent(err error) bool {
	if !errors.Is(err, ErrNotFound) {
		return false
	}
	failure, ok := errors.AsType[*xrpc.Error](err)
	if !ok {
		return true
	}
	return failure.Status == http.StatusNotFound || (failure.Status == http.StatusBadRequest && failure.Name == "RecordNotFound")
}

func relayURLsIn(value map[string]any) []string {
	raw, _ := value["relays"].([]any)
	urls := []string{}
	for _, item := range raw {
		s, ok := item.(string)
		if !ok {
			continue
		}
		s = strings.TrimSpace(s)
		if !(strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://")) || slices.Contains(urls, s) {
			continue
		}
		urls = append(urls, s)
	}
	return urls
}

func OfferingURI(did string) string {
	return "at://" + did + "/" + OfferingNSID + "/" + OfferingRKey
}

func HeartbeatOfferingRKey(ctx context.Context, getter RecordGetter, did string) (string, error) {
	rec, err := getter.GetRecord(ctx, did, HeartbeatNSID, HeartbeatRKey)
	if err != nil {
		return "", err
	}
	heartbeat, err := ParseHeartbeat(rec.Value)
	if err != nil {
		return "", err
	}
	aturi, err := syntax.ParseATURI(heartbeat.Offering)
	if err != nil || aturi.Authority().String() != did || aturi.Collection().String() != OfferingNSID || aturi.RecordKey() == "" {
		return "", fmt.Errorf("%w: the heartbeat of %s names %q, not an offering in its own repo", ErrInvalidRecord, did, heartbeat.Offering)
	}
	return aturi.RecordKey().String(), nil
}

func ReadOffering(ctx context.Context, getter RecordGetter, did string) (Offering, string, error) {
	if getter == nil {
		return Offering{}, "", fmt.Errorf("%w: no record getter", ErrInvalidRecord)
	}
	rec, err := getter.GetRecord(ctx, did, OfferingNSID, OfferingRKey)
	if errors.Is(err, ErrNotFound) {
		rkey, herr := HeartbeatOfferingRKey(ctx, getter, did)
		switch {
		case errors.Is(herr, ErrNotFound) || rkey == OfferingRKey:
			return Offering{}, "", err
		case herr != nil:
			return Offering{}, "", herr
		}
		rec, err = getter.GetRecord(ctx, did, OfferingNSID, rkey)
	}
	if err != nil {
		return Offering{}, "", err
	}
	offering, err := ParseOffering(rec.Value)
	if err != nil {
		return Offering{}, "", err
	}
	return *offering, rec.URI, nil
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
	interval = opt.Positive(interval, HeartbeatInterval)
	runCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() {
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
	})
	return func() {
		cancel()
		wg.Wait()
	}
}
