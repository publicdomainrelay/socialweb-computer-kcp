package requester

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/serviceauth"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/xrpc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/tunnel"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/ttlcache"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/market/participant"
)

const (
	marketReplyLimit = 4 << 20

	bidderDiscoveryTimeout = 10 * time.Second

	defaultMarketServiceID = market.DefaultMarketServiceID
)

var ErrBidderUnreachable = errors.New("requester: the bidder's market endpoint is not reachable yet")

const bidderSuspendedError = "Suspended"

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

	Ingress []market.StrongRef

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

const (
	dropDenied         = "denied by the requester"
	dropContractDenied = "denied by the contract"
	dropNoHeartbeat    = "no fresh heartbeat"
	dropNoOffering     = "no compute.vm offering"
	dropNoEndpoint     = "the offering names no endpoint"
	dropOfferingsRead  = "offerings read failed: "
	dropEndpoint       = "offering endpoint refused: "

	maxDropsInMessage = 5
)

type BidderDrop struct {
	DID string

	Reason string
}

type Discovery struct {
	Restricted bool

	Candidates []string

	Bidders []Bidder

	Dropped []BidderDrop
}

func (d Discovery) clone() Discovery {
	d.Candidates = slices.Clone(d.Candidates)
	d.Bidders = slices.Clone(d.Bidders)
	d.Dropped = slices.Clone(d.Dropped)
	return d
}

func (d Discovery) Without(deny []string) Discovery {
	if len(deny) == 0 {
		return d
	}
	out := d.clone()
	out.Bidders = out.Bidders[:0]
	denied := map[string]bool{}
	for _, bidder := range d.Bidders {
		if !slices.Contains(deny, bidder.DID) {
			out.Bidders = append(out.Bidders, bidder)
			continue
		}
		if !denied[bidder.DID] {
			denied[bidder.DID] = true
			out.Dropped = append(out.Dropped, BidderDrop{DID: bidder.DID, Reason: dropContractDenied})
		}
	}
	return out
}

func (d Discovery) DroppedText() []string {
	out := make([]string, 0, len(d.Dropped))
	for _, drop := range d.Dropped {
		out = append(out, drop.DID+": "+drop.Reason)
	}
	return out
}

func (d Discovery) Why() string {
	if len(d.Dropped) == 0 {
		if d.Restricted {
			return "the contract's bidderDids name no bidder"
		}
		return "no candidate bidder DIDs: the requester has no bidderDids and no relay found an offering"
	}
	parts := d.DroppedText()
	more := ""
	if len(parts) > maxDropsInMessage {
		more = fmt.Sprintf(" and %d more", len(parts)-maxDropsInMessage)
		parts = parts[:maxDropsInMessage]
	}
	return "no bidder with a market endpoint among " + strconv.Itoa(len(d.Dropped)) + " candidates: " + strings.Join(parts, "; ") + more
}

func BidderDIDList(dids []string) []string {
	out := make([]string, 0, len(dids))
	for _, did := range dids {
		if did = strings.TrimSpace(did); did != "" {
			out = append(out, did)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

type discoveryKey struct {
	only string
}

type ServiceAuthSource interface {
	ServiceAuth(ctx context.Context, audience, lxm string) (string, error)
}

type MarketClient interface {
	Discover(ctx context.Context, now time.Time, only []string) (Discovery, error)

	SubmitRFP(ctx context.Context, bidder Bidder, rfp SubmitRFPRequest) ([]BidRecord, error)

	SubmitAccept(ctx context.Context, bidder Bidder, acceptURI, acceptCID string) (*AcceptReply, error)

	SubmitEvent(ctx context.Context, bidder Bidder, event SubmitEventRequest) (int, error)

	FindReceipt(ctx context.Context, bidderDID, acceptURI, acceptCID string) (*market.ResolvedRecord, error)

	FindTermination(ctx context.Context, bidderDID, receiptURI string) (*MarketEvent, error)
}

type HTTPMarket struct {
	Directory identity.Directory

	Reader *market.RepoStore

	RepoDID string

	Signer *market.Signer

	ServiceKey atcrypto.PrivateKey

	ServiceAuth ServiceAuthSource

	RelayURLs []string

	BidderDIDs []string

	OperatorDID string

	DenyBidderDIDs []string

	Ingress tunnel.Service

	IngressRef func(ctx context.Context) *market.RecordRef

	IngressWritable func(ctx context.Context) bool

	IngressSTUN []string

	HTTPClient *http.Client

	EndpointKinds []string

	Timeout time.Duration

	Log *slog.Logger

	StartedAt time.Time

	firstDiscovery atomic.Bool

	discoveryOnce sync.Once

	discovered *ttlcache.Cache[discoveryKey, Discovery]
}

var _ MarketClient = (*HTTPMarket)(nil)

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

func (m *HTTPMarket) Discover(ctx context.Context, now time.Time, only []string) (Discovery, error) {
	if m == nil || m.Reader == nil {
		return Discovery{}, errors.New("requester: the market has no record reader")
	}
	only = BidderDIDList(only)
	m.discoveryOnce.Do(func() {
		m.discovered = ttlcache.New[discoveryKey, Discovery](ttlcache.Options{TTL: 20 * time.Second, MaxEntries: 256})
	})
	found, err := m.discovered.GetOrLoad(ctx, discoveryKey{only: strings.Join(only, ",")}, func(ctx context.Context) (Discovery, time.Duration, error) {
		found := m.discover(ctx, now, only)
		if len(found.Bidders) == 0 {
			return found, -1, nil
		}
		return found, 0, nil
	})
	if err != nil {
		return Discovery{}, err
	}
	return found.clone(), nil
}

func (m *HTTPMarket) discover(ctx context.Context, now time.Time, only []string) Discovery {
	discoveryStart := time.Now()
	found := Discovery{Restricted: len(only) > 0}
	seen := map[string]bool{}
	explicit := map[string]bool{}
	add := func(did string, named bool) {
		did = strings.TrimSpace(did)
		if did == "" || seen[did] {
			return
		}
		seen[did] = true
		if slices.Contains(m.DenyBidderDIDs, did) {
			found.Dropped = append(found.Dropped, BidderDrop{DID: did, Reason: dropDenied})
			return
		}
		explicit[did] = named
		found.Candidates = append(found.Candidates, did)
	}
	source, counts := "contract", ""
	if found.Restricted {
		for _, did := range only {
			add(did, true)
		}
	} else {
		source = "requester"
		sources, auto := m.candidateSources(ctx)
		for _, candidates := range sources {
			for _, did := range candidates.dids {
				add(did, candidates.named)
			}
		}
		counts = sourceCounts(sources, auto)
	}
	unnamed := []string{}
	for _, did := range found.Candidates {
		if !explicit[did] {
			unnamed = append(unnamed, did)
		}
	}
	fresh := []string{}
	if len(unnamed) > 0 {
		fresh = market.NewHeartbeatCache(m.Reader).FreshDIDs(ctx, unnamed, now)
	}
	seenEndpoint := map[string]bool{}
	for _, did := range found.Candidates {
		if !explicit[did] && !slices.Contains(fresh, did) {
			found.Dropped = append(found.Dropped, BidderDrop{DID: did, Reason: dropNoHeartbeat})
			continue
		}
		bidders, reason := m.offeredBidders(ctx, did)
		added := 0
		for _, bidder := range bidders {
			key := did + "::" + bidder.Endpoint
			if seenEndpoint[key] {
				continue
			}
			seenEndpoint[key] = true
			found.Bidders = append(found.Bidders, bidder)
			added++
		}
		if added == 0 {
			found.Dropped = append(found.Dropped, BidderDrop{DID: did, Reason: reason})
		}
	}
	bidderDIDs := make([]string, 0, len(found.Bidders))
	for _, bidder := range found.Bidders {
		bidderDIDs = append(bidderDIDs, bidder.DID)
	}
	discoveryMs := time.Since(discoveryStart).Milliseconds()
	m.log().Info("requester_bidder_discovery", "source", source, "sources", counts, "candidates", len(found.Candidates), "bidders", bidderDIDs,
		"dropped", len(found.Dropped), "discoveryMs", discoveryMs)
	if !m.StartedAt.IsZero() && !m.firstDiscovery.Swap(true) {
		m.log().Info("requester_start_timing", "step", "firstBidderDiscovery", "sinceStartMs", time.Since(m.StartedAt).Milliseconds(),
			"discoveryMs", discoveryMs, "bidders", len(found.Bidders))
	}
	return found
}

func (m *HTTPMarket) offeredBidders(ctx context.Context, did string) ([]Bidder, string) {
	offering, _, err := market.ReadOffering(ctx, m.Reader, did)
	if errors.Is(err, market.ErrNotFound) {
		return nil, dropNoOffering
	}
	if err != nil {
		m.log().Warn("requester_offerings_failed", "did", did, "err", err)
		return nil, dropOfferingsRead + err.Error()
	}
	if !market.OfferingApplies(offering, market.ComputeVMNSID) {
		return nil, dropNoOffering
	}
	bidder, ok := BidderFor(did, offering.Endpoints, m.EndpointKinds)
	if !ok {
		return nil, dropNoEndpoint
	}
	return []Bidder{bidder}, dropNoOffering
}

func BidderFor(did string, endpoints []market.Endpoint, kinds []string) (Bidder, bool) {
	for _, kind := range kinds {
		for _, endpoint := range endpoints {
			if endpoint.Kind == kind && endpoint.Validate() == nil {
				return Bidder{DID: did, Endpoint: endpoint.URL(), Audience: did + "#" + market.DefaultMarketServiceID}, true
			}
		}
	}
	return Bidder{}, false
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
	timeout = opt.Positive(timeout, DefaultReceiptTimeout)
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	auth := xrpc.NewServiceAuth(xrpc.MinterFunc(m.token), audience, lxm)
	reply, err := xrpc.Procedure[any, xrpc.Reply](cctx, &xrpc.Client{Host: endpoint, HTTP: client, Auth: auth, Limit: marketReplyLimit}, lxm, body)
	if xerr, ok := errors.AsType[*xrpc.Error](err); ok {
		reply = xrpc.Reply{Status: xerr.Status, Body: xerr.Body}
	} else if err != nil {
		return 0, nil, err
	}
	out := map[string]any{}
	if len(bytes.TrimSpace(reply.Body)) > 0 {
		_ = json.Unmarshal(reply.Body, &out)
	}
	return reply.Status, out, nil
}

func (m *HTTPMarket) SubmitRFP(ctx context.Context, bidder Bidder, rfp SubmitRFPRequest) ([]BidRecord, error) {
	body := map[string]any{
		"rfpUri": rfp.URI,
		"rfpCid": rfp.CID,
	}
	var offer tunnel.Offer
	offered := false
	if m.Ingress != nil && (m.IngressWritable == nil || m.IngressWritable(ctx)) {
		current, err := m.Ingress.Offer(ctx, m.RepoDID, bidder.DID)
		if err != nil {
			m.log().Warn("requester_webrtc_offer_failed", "bidder", bidder.DID, "err", err.Error())
		} else if ref, err := m.createIngressOffer(ctx, current, bidder.DID); err != nil {
			m.log().Warn("requester_webrtc_offer_record_failed", "bidder", bidder.DID, "err", err.Error())
		} else {
			body["ingress"] = []map[string]any{ref.Map()}
			offer = current
			offered = true
		}
	}
	status, reply, err := m.post(ctx, bidder.Endpoint, market.SubmitRFPNSID, bidder.Audience, body, m.Timeout)
	if errors.Is(err, xrpc.ErrTooLarge) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBidderUnreachable, err)
	}
	if status < 200 || status > 299 {
		detail := stringOf(reply["error"])
		if message := stringOf(reply["message"]); message != "" {
			detail = strings.TrimSpace(detail + ": " + message)
		}
		err := fmt.Errorf("requester: submitRfp to %s (aud %s) answered HTTP %d", bidder.Endpoint, bidder.Audience, status)
		if detail != "" {
			err = fmt.Errorf("requester: submitRfp to %s (aud %s) answered HTTP %d: %s", bidder.Endpoint, bidder.Audience, status, detail)
		}
		if stringOf(reply["error"]) == bidderSuspendedError {
			return nil, err
		}
		if status == http.StatusNotFound || status == http.StatusTooManyRequests || status >= 500 {
			return nil, fmt.Errorf("%w: %w", ErrBidderUnreachable, err)
		}
		return nil, err
	}
	if offered {
		m.acceptIngress(ctx, bidder.DID, offer, reply)
	}
	return bidsFromReply(bidder.DID, reply), nil
}

func (m *HTTPMarket) ingressList(ctx context.Context) []market.StrongRef {
	if m.IngressRef == nil {
		return nil
	}
	ref := m.IngressRef(ctx)
	if ref == nil || ref.URI == "" {
		return nil
	}
	return []market.StrongRef{market.NewStrongRef(ref.URI, ref.CID)}
}

func (m *HTTPMarket) createIngressOffer(ctx context.Context, offer tunnel.Offer, bidderDID string) (market.StrongRef, error) {
	if m.Reader == nil {
		return market.StrongRef{}, errors.New("requester: no record store for the ingress offer")
	}
	var descriptor *market.StrongRef
	if m.IngressRef != nil {
		if ref := m.IngressRef(ctx); ref != nil && ref.URI != "" {
			strong := market.NewStrongRef(ref.URI, ref.CID)
			descriptor = &strong
		}
	}
	value, err := market.EncodeRecord(market.IngressWebRTC{
		Kind:       tunnel.KindWebRTC,
		Mode:       market.IngressModeOffer,
		ID:         offer.ID,
		Peer:       bidderDID,
		Session:    offer.Session,
		Sdp:        offer.SDP,
		Descriptor: descriptor,
		StunUrls:   m.IngressSTUN,
		CreatedAt:  market.NowISO(),
	})
	if err != nil {
		return market.StrongRef{}, err
	}
	return m.Reader.CreateRecord(ctx, m.RepoDID, market.IngressWebRTCNSID, value)
}

func (m *HTTPMarket) acceptIngress(ctx context.Context, bidderDID string, offer tunnel.Offer, body map[string]any) {
	if m.Ingress == nil || m.Reader == nil || body == nil {
		return
	}
	raw, ok := body["ingress"]
	if !ok {
		return
	}
	refs, err := ingressRefsOf(raw)
	if err != nil {
		m.log().Warn("requester_webrtc_answer_invalid", "bidder", bidderDID, "err", err.Error())
		return
	}
	for _, ref := range refs {
		if market.ATURIAuthority(ref.URI) != bidderDID || market.NSIDFromURI(ref.URI) != market.IngressWebRTCNSID {
			continue
		}
		resolved, err := m.Reader.Resolve(ctx, market.RecordRef{URI: ref.URI, CID: ref.CID})
		if err != nil {
			m.log().Warn("requester_webrtc_answer_unresolved", "bidder", bidderDID, "answer", ref.URI, "err", err.Error())
			continue
		}
		record, err := market.ParseIngressWebRTC(resolved.Value)
		if err != nil {
			m.log().Warn("requester_webrtc_answer_invalid", "bidder", bidderDID, "err", err.Error())
			continue
		}
		if err := record.Validate(); err != nil {
			m.log().Warn("requester_webrtc_answer_invalid", "bidder", bidderDID, "err", err.Error())
			continue
		}
		if record.Mode != market.IngressModeAnswer {
			continue
		}
		if record.ID != offer.ID || record.Session != offer.Session || record.Peer != m.RepoDID {
			m.log().Warn("requester_webrtc_answer_mismatch", "bidder", bidderDID, "answer", ref.URI)
			continue
		}
		answer := tunnel.Answer{Kind: tunnel.KindWebRTC, ID: record.ID, Session: record.Session, Peer: bidderDID, Type: tunnel.SignalAnswer, SDP: record.Sdp}
		if err := m.Ingress.Accept(ctx, answer); err != nil {
			m.log().Warn("requester_webrtc_accept_failed", "bidder", bidderDID, "err", err.Error())
			continue
		}
		m.log().Info("requester_webrtc_connected", "bidder", bidderDID, "id", answer.ID, "session", answer.Session)
		return
	}
	m.log().Warn("requester_webrtc_answer_missing", "bidder", bidderDID, "session", offer.Session)
}

func ingressRefsOf(raw any) ([]market.StrongRef, error) {
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var refs []market.StrongRef
	if err := json.Unmarshal(encoded, &refs); err != nil {
		return nil, err
	}
	out := make([]market.StrongRef, 0, len(refs))
	for _, ref := range refs {
		if ref.URI == "" || ref.CID == "" {
			continue
		}
		out = append(out, ref)
	}
	if len(out) == 0 {
		return nil, errors.New("the ingress list names no record")
	}
	return out, nil
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
	request := map[string]any{
		"acceptUri": acceptURI,
		"acceptCid": acceptCID,
	}
	if refs := m.ingressList(ctx); len(refs) > 0 {
		request["ingress"] = refs
	}
	status, body, err := m.postWithClient(ctx, &client, bidder.Endpoint, market.SubmitAcceptNSID, bidder.Audience, request, submitAcceptProvisionTimeout)
	if err != nil {
		return nil, err
	}
	reply := &AcceptReply{Status: status, Body: body}
	if body != nil {
		reply.URI, _ = body["uri"].(string)
		reply.CID, _ = body["cid"].(string)
		reply.SubmitEvent, _ = body["submitEvent"].(string)
		if raw, ok := body["ingress"]; ok {
			reply.Ingress, _ = ingressRefsOf(raw)
		}
	}
	return reply, nil
}

func (m *HTTPMarket) SubmitEvent(ctx context.Context, bidder Bidder, event SubmitEventRequest) (int, error) {
	request := map[string]any{
		"uri":    event.URI,
		"cid":    event.CID,
		"record": event.Record,
	}
	if refs := m.ingressList(ctx); len(refs) > 0 {
		request["ingress"] = refs
	}
	status, body, err := m.post(ctx, bidder.Endpoint, market.SubmitEventNSID, bidder.Audience, request, m.Timeout)
	if err == nil && (status < 200 || status > 299) {
		err = &participant.CallError{Status: status, Err: &xrpc.Error{Status: status, Name: stringOf(body["error"]), Message: stringOf(body["message"])}}
	}
	return status, err
}

func (m *HTTPMarket) FindReceipt(ctx context.Context, bidderDID, acceptURI, acceptCID string) (*market.ResolvedRecord, error) {
	if m == nil || m.Reader == nil {
		return nil, errors.New("requester: the market has no record reader")
	}
	if strings.TrimSpace(acceptCID) == "" {
		return nil, fmt.Errorf("requester: the accept %s names no cid, so its receipt rkey is unknown", acceptURI)
	}
	record, err := m.Reader.GetRecord(ctx, bidderDID, market.ReceiptNSID, market.ReceiptRKey(acceptURI, acceptCID))
	if errors.Is(err, market.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	accept, _ := record.Value["accept"].(map[string]any)
	if accept == nil || stringOf(accept["uri"]) != acceptURI {
		m.log().Warn("requester_receipt_accept_mismatch", "receipt", record.URI, "accept", acceptURI)
		return nil, nil
	}
	return &record, nil
}

func (m *HTTPMarket) FindTermination(ctx context.Context, bidderDID, receiptURI string) (*MarketEvent, error) {
	if m == nil || m.Reader == nil {
		return nil, errors.New("requester: the market has no record reader")
	}
	_, eventRKey := market.EventRKeys(market.TerminationKey(receiptURI))
	record, err := m.Reader.GetRecord(ctx, bidderDID, market.EventNSID, eventRKey)
	if errors.Is(err, market.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	receipt, _ := record.Value["receipt"].(map[string]any)
	payload, _ := record.Value["payload"].(map[string]any)
	var ref market.RecordRef
	if payload != nil {
		ref = market.RecordRef{URI: stringOf(payload["uri"]), CID: stringOf(payload["cid"])}
	}
	if receipt == nil || stringOf(receipt["uri"]) != receiptURI || market.NSIDFromURI(ref.URI) != market.ComputeEventsVMTerminatedNSID {
		m.log().Warn("requester_termination_event_mismatch", "event", record.URI, "receipt", receiptURI, "payload", ref.URI)
		return nil, nil
	}
	resolved, err := m.Reader.Resolve(ctx, ref)
	if errors.Is(err, market.ErrNotFound) {
		m.log().Warn("requester_termination_payload_missing", "event", record.URI, "payload", ref.URI)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("requester: the termination payload %s: %w", ref.URI, err)
	}
	return &MarketEvent{Event: record, Payload: resolved}, nil
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

func payloadRef(value map[string]any) market.RecordRef {
	ref, _ := value["payload"].(map[string]any)
	if ref == nil {
		return market.RecordRef{}
	}
	return market.RecordRef{URI: stringOf(ref["uri"]), CID: stringOf(ref["cid"])}
}

var ErrParticipationSuspended = errors.New("requester: participation is suspended; no new RFP or accept is published")
