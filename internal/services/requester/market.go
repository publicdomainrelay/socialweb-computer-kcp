package requester

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/serviceauth"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/ttlcache"
)

const (
	marketReplyLimit = 4 << 20

	bidderDiscoveryTimeout = 10 * time.Second

	defaultMarketServiceID = market.DefaultMarketServiceID

	defaultComputeEventServiceID = market.DefaultComputeEventServiceID

	maxBidderServiceRefHops = 8
)

type Bidder struct {
	DID string

	Endpoint string

	Audience string
}

type BidRecord struct {
	DID string

	URI string

	CID string

	Free bool

	Reason string

	Cost float64

	Record map[string]any

	ArrivedAt time.Time
}

type SubmitRFPRequest struct {
	URI string

	CID string
}

type AcceptReply struct {
	URI string

	CID string

	SubmitEvent string

	Status int

	Body map[string]any
}

type SubmitEventRequest struct {
	URI string

	CID string

	Record map[string]any
}

type MarketEvent struct {
	Event market.ResolvedRecord

	Payload market.ResolvedRecord
}

type ServiceAuthSource interface {
	ServiceAuth(ctx context.Context, audience, lxm string) (string, error)
}

type MarketClient interface {
	Discover(ctx context.Context, now time.Time) ([]Bidder, error)

	SubmitRFP(ctx context.Context, bidder Bidder, rfp SubmitRFPRequest) ([]BidRecord, error)

	SubmitAccept(ctx context.Context, bidder Bidder, acceptURI, acceptCID string) (*AcceptReply, error)

	SubmitEvent(ctx context.Context, bidder Bidder, event SubmitEventRequest) (int, error)

	FindBids(ctx context.Context, bidderDID, rfpURI string) ([]BidRecord, error)

	FindReceipt(ctx context.Context, bidderDID, acceptURI string) (*market.ResolvedRecord, error)

	FindEvents(ctx context.Context, bidderDID, receiptURI string) ([]MarketEvent, error)

	FindTerminations(ctx context.Context, bidderDID, receiptURI string) ([]MarketEvent, error)
}

type HTTPMarket struct {
	Directory identity.Directory

	Reader RecordStore

	RepoDID string

	Signer *market.Signer

	ServiceKey atcrypto.PrivateKey

	ServiceAuth ServiceAuthSource

	RelayURLs []string

	BidderDIDs []string

	DenyBidderDIDs []string

	HTTPClient *http.Client

	Timeout time.Duration

	Log *slog.Logger

	discoveryOnce sync.Once

	discovered *ttlcache.Cache[struct{}, []Bidder]
}

func (m *HTTPMarket) client() *http.Client {
	if m.HTTPClient != nil {
		return m.HTTPClient
	}
	return &http.Client{Timeout: DefaultHTTPTimeout}
}

func (m *HTTPMarket) log() *slog.Logger {
	if m.Log != nil {
		return m.Log
	}
	return slog.New(slog.DiscardHandler)
}

func (m *HTTPMarket) Discover(ctx context.Context, now time.Time) ([]Bidder, error) {
	if m == nil || m.Reader == nil {
		return nil, errors.New("requester: the market has no record reader")
	}
	m.discoveryOnce.Do(func() {
		m.discovered = ttlcache.New[struct{}, []Bidder](ttlcache.Options{TTL: 20 * time.Second})
	})
	bidders, err := m.discovered.GetOrLoad(ctx, struct{}{}, func(ctx context.Context) ([]Bidder, time.Duration, error) {
		bidders := m.discover(ctx, now)
		if len(bidders) == 0 {
			return bidders, -1, nil
		}
		return bidders, 0, nil
	})
	if err != nil {
		return nil, err
	}
	return append(make([]Bidder, 0, len(bidders)), bidders...), nil
}

func (m *HTTPMarket) discover(ctx context.Context, now time.Time) []Bidder {
	seen := map[string]bool{}
	candidates := []string{}
	add := func(did string) {
		did = strings.TrimSpace(did)
		if did == "" || seen[did] || containsString(m.DenyBidderDIDs, did) {
			return
		}
		seen[did] = true
		candidates = append(candidates, did)
	}
	explicit := map[string]bool{}
	for _, did := range m.BidderDIDs {
		add(did)
		explicit[strings.TrimSpace(did)] = true
	}
	if len(m.RelayURLs) > 0 {
		for _, did := range market.DiscoverBiddersFromRelays(ctx, m.RelayURLs, market.OfferingNSID, bidderDiscoveryTimeout, m.client(), market.NoopLog) {
			add(did)
		}
	}
	heartbeat := market.NewHeartbeatCache(m.Reader)
	fresh := heartbeat.FreshDIDs(ctx, candidates, now)
	for _, did := range candidates {
		if explicit[did] && !containsString(fresh, did) {
			fresh = append(fresh, did)
		}
	}
	bidders := make([]Bidder, 0, len(fresh))
	seenEndpoint := map[string]bool{}
	for _, did := range fresh {
		offerings, err := market.ListOfferings(ctx, m.Reader, did)
		if err != nil {
			m.log().Warn("requester_offerings_failed", "did", did, "err", err)
			continue
		}
		for _, offering := range offerings {
			if !market.OfferingApplies(offering, market.ComputeVMNSID) {
				continue
			}
			endpoint, audience, err := resolveBidderEndpoint(ctx, m.Directory, offering.EndpointURL)
			if err != nil {
				m.log().Warn("requester_bidder_endpoint_refused", "did", did, "endpoint", offering.EndpointURL, "err", err)
				continue
			}
			key := did + "::" + endpoint
			if seenEndpoint[key] {
				continue
			}
			seenEndpoint[key] = true
			bidders = append(bidders, Bidder{DID: did, Endpoint: endpoint, Audience: audience})
		}
	}
	return bidders
}

func resolveBidderEndpoint(ctx context.Context, dir identity.Directory, raw string) (string, string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", nil
	}
	return resolveBidderServiceRef(ctx, dir, raw)
}

func resolveBidderServiceRef(ctx context.Context, dir identity.Directory, raw string) (string, string, error) {
	if dir == nil {
		return "", "", fmt.Errorf("offering endpoint %q needs an identity directory", raw)
	}
	seen := map[string]bool{}
	current := strings.TrimSpace(raw)
	for range maxBidderServiceRefHops {
		if !strings.HasPrefix(current, "did:") {
			u, err := url.Parse(current)
			if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
				return "", "", fmt.Errorf("offering endpoint %q is not an absolute http or https URL or a DID service ref", current)
			}
			base := strings.TrimRight(current, "/")
			if !strings.HasSuffix(base, "/xrpc") {
				base += "/xrpc"
			}
			return base, market.ServiceDIDForHost(u.Host), nil
		}
		didText := current
		fragment := defaultMarketServiceID
		if before, after, found := strings.Cut(current, "#"); found {
			didText = before
			if strings.TrimSpace(after) != "" {
				fragment = strings.TrimSpace(after)
			}
		}
		key := didText + "#" + fragment
		if seen[key] {
			return "", "", fmt.Errorf("offering endpoint %q repeats the service reference %s", raw, key)
		}
		seen[key] = true
		did, err := syntax.ParseDID(strings.TrimSpace(didText))
		if err != nil {
			return "", "", fmt.Errorf("offering endpoint %q: %w", raw, err)
		}
		doc, err := dir.LookupDID(ctx, did)
		if err != nil || doc == nil {
			return "", "", fmt.Errorf("offering endpoint %q cannot be resolved", raw)
		}
		service, ok := doc.Services[fragment]
		if !ok || strings.TrimSpace(service.URL) == "" {
			return "", "", fmt.Errorf("offering endpoint %q names no service %s", raw, fragment)
		}
		current = strings.TrimSpace(service.URL)
	}
	return "", "", fmt.Errorf("offering endpoint %q exceeds %d service reference hops", raw, maxBidderServiceRefHops)
}

func (m *HTTPMarket) token(ctx context.Context, audience string, lxm string) (string, error) {
	if m.ServiceAuth != nil {
		return m.ServiceAuth.ServiceAuth(ctx, audience, lxm)
	}
	if m.ServiceKey == nil {
		return "", errors.New("requester: no service auth key")
	}
	if m.RepoDID == "" {
		return "", errors.New("requester: no repo DID for service auth")
	}
	nsid, err := syntax.ParseNSID(lxm)
	if err != nil {
		return "", err
	}
	issuer, err := syntax.ParseDID(m.RepoDID)
	if err != nil {
		return "", err
	}
	return serviceauth.Sign(issuer, audience, nsid, m.ServiceKey, ServiceAuthTTL)
}

func (m *HTTPMarket) post(ctx context.Context, endpoint, lxm, audience string, body any, timeout time.Duration) (int, map[string]any, error) {
	return m.postWithClient(ctx, m.client(), endpoint, lxm, audience, body, timeout)
}

func (m *HTTPMarket) postWithClient(ctx context.Context, client *http.Client, endpoint, lxm, audience string, body any, timeout time.Duration) (int, map[string]any, error) {
	if strings.TrimSpace(endpoint) == "" {
		return 0, nil, fmt.Errorf("requester: no inbound endpoint is advertised for %s", lxm)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	if timeout <= 0 {
		timeout = DefaultReceiptTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var resp *http.Response
	for attempt := range 2 {
		token, err := m.token(cctx, audience, lxm)
		if err != nil {
			return 0, nil, err
		}
		req, err := http.NewRequestWithContext(cctx, http.MethodPost, endpoint+"/"+lxm, bytes.NewReader(raw))
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err = client.Do(req)
		if err != nil {
			return 0, nil, err
		}
		if attempt == 0 && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, marketReplyLimit))
			resp.Body.Close()
			continue
		}
		break
	}
	defer resp.Body.Close()
	reply, err := io.ReadAll(io.LimitReader(resp.Body, marketReplyLimit))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	out := map[string]any{}
	if len(bytes.TrimSpace(reply)) > 0 {
		_ = json.Unmarshal(reply, &out)
	}
	return resp.StatusCode, out, nil
}

func (m *HTTPMarket) SubmitRFP(ctx context.Context, bidder Bidder, rfp SubmitRFPRequest) ([]BidRecord, error) {
	status, body, err := m.post(ctx, bidder.Endpoint, market.SubmitRFPNSID, bidder.Audience, map[string]any{
		"rfpUri": rfp.URI,
		"rfpCid": rfp.CID,
	}, m.Timeout)
	if err != nil {
		return nil, err
	}
	if status < 200 || status > 299 {
		detail := stringOf(body["error"])
		if message := stringOf(body["message"]); message != "" {
			detail = strings.TrimSpace(detail + ": " + message)
		}
		if detail == "" {
			return nil, fmt.Errorf("requester: submitRfp to %s answered HTTP %d", bidder.Endpoint, status)
		}
		return nil, fmt.Errorf("requester: submitRfp to %s answered HTTP %d: %s", bidder.Endpoint, status, detail)
	}
	return bidsFromReply(bidder.DID, body), nil
}

func bidsFromReply(did string, body map[string]any) []BidRecord {
	if body == nil {
		return nil
	}
	candidates := []any{body["bid"], body["bids"], body["record"], body}
	out := []BidRecord{}
	for _, candidate := range candidates {
		switch value := candidate.(type) {
		case map[string]any:
			if rec := bidRecordOf(did, value); rec != nil {
				out = append(out, *rec)
			}
		case []any:
			for _, item := range value {
				if obj, ok := item.(map[string]any); ok {
					if rec := bidRecordOf(did, obj); rec != nil {
						out = append(out, *rec)
					}
				}
			}
		}
	}
	return dedupeBids(out)
}

func bidRecordOf(did string, value map[string]any) *BidRecord {
	uri, _ := value["uri"].(string)
	cid, _ := value["cid"].(string)
	record, _ := value["record"].(map[string]any)
	if record == nil {
		record = value
	}
	if uri == "" {
		return nil
	}
	if _, ok := record["rfp"]; !ok {
		return nil
	}
	return &BidRecord{DID: did, URI: uri, CID: cid, Record: record, ArrivedAt: time.Now()}
}

func (m *HTTPMarket) SubmitAccept(ctx context.Context, bidder Bidder, acceptURI, acceptCID string) (*AcceptReply, error) {
	client := *m.client()
	client.Timeout = 0
	status, body, err := m.postWithClient(ctx, &client, bidder.Endpoint, market.SubmitAcceptNSID, bidder.Audience, map[string]any{
		"acceptUri": acceptURI,
		"acceptCid": acceptCID,
	}, submitAcceptProvisionTimeout)
	if err != nil {
		return nil, err
	}
	reply := &AcceptReply{Status: status, Body: body}
	if body != nil {
		reply.URI, _ = body["uri"].(string)
		reply.CID, _ = body["cid"].(string)
		reply.SubmitEvent, _ = body["submitEvent"].(string)
	}
	return reply, nil
}

func (m *HTTPMarket) SubmitEvent(ctx context.Context, bidder Bidder, event SubmitEventRequest) (int, error) {
	status, _, err := m.post(ctx, bidder.Endpoint, market.SubmitEventNSID, bidder.Audience, map[string]any{
		"uri":    event.URI,
		"cid":    event.CID,
		"record": event.Record,
	}, m.Timeout)
	return status, err
}

func (m *HTTPMarket) FindBids(ctx context.Context, bidderDID, rfpURI string) ([]BidRecord, error) {
	if m == nil || m.Reader == nil {
		return nil, errors.New("requester: the market has no record reader")
	}
	out := []BidRecord{}
	for _, collection := range []string{market.BidNSID, market.BidsFreeNSID, market.BidsX402NSID} {
		records, err := listAll(ctx, m.Reader, bidderDID, collection, 100)
		if err != nil {
			continue
		}
		for _, record := range records {
			rfp, _ := record.Value["rfp"].(map[string]any)
			if rfp == nil || stringOf(rfp["uri"]) != rfpURI {
				continue
			}
			bid := BidRecord{DID: bidderDID, URI: record.URI, CID: record.CID, Record: record.Value, ArrivedAt: time.Now()}
			if collection == market.BidsFreeNSID {
				bid.Free = true
				bid.Reason = stringOf(record.Value["reason"])
			}
			if collection == market.BidsX402NSID {
				bid.Cost = numberValue(record.Value["cost"])
			}
			out = append(out, bid)
		}
	}
	return dedupeBids(out), nil
}

func (m *HTTPMarket) FindReceipt(ctx context.Context, bidderDID, acceptURI string) (*market.ResolvedRecord, error) {
	if m == nil || m.Reader == nil {
		return nil, errors.New("requester: the market has no record reader")
	}
	records, err := listAll(ctx, m.Reader, bidderDID, market.ReceiptNSID, 100)
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		accept, _ := record.Value["accept"].(map[string]any)
		if accept == nil || stringOf(accept["uri"]) != acceptURI {
			continue
		}
		found := market.ResolvedRecord{URI: record.URI, CID: record.CID, Value: record.Value}
		return &found, nil
	}
	return nil, nil
}

func (m *HTTPMarket) FindEvents(ctx context.Context, bidderDID, receiptURI string) ([]MarketEvent, error) {
	return m.findEvents(ctx, bidderDID, receiptURI, "")
}

func (m *HTTPMarket) FindTerminations(ctx context.Context, bidderDID, receiptURI string) ([]MarketEvent, error) {
	return m.findEvents(ctx, bidderDID, receiptURI, market.ComputeEventsVMTerminatedNSID)
}

func (m *HTTPMarket) findEvents(ctx context.Context, bidderDID, receiptURI, payloadNSID string) ([]MarketEvent, error) {
	if m == nil || m.Reader == nil {
		return nil, errors.New("requester: the market has no record reader")
	}
	records, err := listAll(ctx, m.Reader, bidderDID, market.EventNSID, 100)
	if err != nil {
		return nil, err
	}
	out := []MarketEvent{}
	for _, record := range records {
		event, ok := m.receiptEvent(ctx, market.ResolvedRecord{URI: record.URI, CID: record.CID, Value: record.Value}, receiptURI, payloadNSID)
		if ok {
			out = append(out, *event)
		}
	}
	return out, nil
}

func (m *HTTPMarket) receiptEvent(ctx context.Context, record market.ResolvedRecord, receiptURI, payloadNSID string) (*MarketEvent, bool) {
	receipt, _ := record.Value["receipt"].(map[string]any)
	if receipt == nil || stringOf(receipt["uri"]) != receiptURI {
		return nil, false
	}
	payload, _ := record.Value["payload"].(map[string]any)
	if payload == nil {
		return nil, false
	}
	ref := market.RecordRef{URI: stringOf(payload["uri"]), CID: stringOf(payload["cid"])}
	if ref.URI == "" {
		return nil, false
	}
	if payloadNSID != "" && market.NSIDFromURI(ref.URI) != payloadNSID {
		return nil, false
	}
	resolved, err := m.Reader.Resolve(ctx, ref)
	if err != nil {
		m.log().Warn("requester_event_payload_failed", "uri", ref.URI, "err", err)
		return nil, false
	}
	return &MarketEvent{Event: record, Payload: resolved}, true
}

func listAll(ctx context.Context, lister market.RecordLister, repoDID, collection string, limit int) ([]market.ListedRecord, error) {
	out := []market.ListedRecord{}
	cursor := ""
	seen := map[string]bool{}
	for range 20 {
		found, err := lister.ListRecords(ctx, repoDID, collection, limit, cursor)
		if err != nil {
			return out, err
		}
		out = append(out, found.Records...)
		if found.Cursor == "" || seen[found.Cursor] {
			return out, nil
		}
		seen[found.Cursor] = true
		cursor = found.Cursor
	}
	return out, nil
}

func dedupeBids(in []BidRecord) []BidRecord {
	seen := map[string]bool{}
	out := make([]BidRecord, 0, len(in))
	for _, bid := range in {
		if bid.URI == "" || seen[bid.URI] {
			continue
		}
		seen[bid.URI] = true
		out = append(out, bid)
	}
	return out
}

func stringOf(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

func numberValue(value any) float64 {
	switch v := value.(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case json.Number:
		f, _ := v.Float64()
		return f
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err == nil {
			return f
		}
	}
	return 0
}

func containsString(values []string, want string) bool {
	return slices.Contains(values, want)
}

func withoutDeniedBidders(bidders []Bidder, deny []string) []Bidder {
	if len(deny) == 0 {
		return bidders
	}
	out := bidders[:0]
	for _, bidder := range bidders {
		if containsString(deny, bidder.DID) {
			continue
		}
		out = append(out, bidder)
	}
	return out
}

func payloadRef(value map[string]any) market.RecordRef {
	ref, _ := value["payload"].(map[string]any)
	if ref == nil {
		return market.RecordRef{}
	}
	return market.RecordRef{URI: stringOf(ref["uri"]), CID: stringOf(ref["cid"])}
}

func bidRef(value map[string]any) market.RecordRef {
	return market.RecordRef{URI: stringOf(value["uri"]), CID: stringOf(value["cid"])}
}
