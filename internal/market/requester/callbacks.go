package requester

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/market/participant"
)

const serviceAuthTimeout = 5 * time.Second

type RequestError struct {
	Status int

	Code string

	Message string
}

func (e *RequestError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return e.Code
}

type SubmittedRecord struct {
	URI string `json:"uri"`

	CID string `json:"cid"`

	Record map[string]any `json:"record"`

	Ingress []market.StrongRef `json:"ingress,omitempty"`
}

func invalidRequest(err error) *RequestError {
	return &RequestError{Status: http.StatusBadRequest, Code: "InvalidRequest", Message: err.Error()}
}

func (q *Requester) CallbackHostname(host string) string {
	if host = strings.TrimSpace(host); host != "" {
		return host
	}
	raw := strings.TrimSpace(q.cfg.Network().IngressProxyHost)
	if raw == "" {
		return ""
	}
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	if !strings.Contains(raw, "://") {
		return strings.Trim(raw, "[]")
	}
	return ""
}

func (q *Requester) AuthorizeCallback(ctx context.Context, host, authorization, lxm, serviceID string) (string, error) {
	hostname := q.CallbackHostname(host)
	if hostname == "" {
		return "", &RequestError{Status: http.StatusBadRequest, Code: "InvalidRequest", Message: "no callback hostname"}
	}
	ctx, cancel := context.WithTimeout(ctx, serviceAuthTimeout)
	defer cancel()
	audiences := []string{q.marketDID, q.repoDID, q.did}
	for _, raw := range []string{q.marketAddress(), q.cfg.Network().IngressProxyHost} {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		host := raw
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			host = u.Host
		}
		audiences = append(audiences, market.ServiceAudienceDIDs(host)...)
	}
	audiences = append(audiences, market.ServiceAudienceDIDs(hostname)...)
	verifier := &participant.Authorizer{Directory: q.deps.Directory, Hostname: hostname, ServiceIDs: []string{serviceID}, AudienceDIDs: audiences}
	issuer, err := verifier.Verify(ctx, authorization, lxm)
	if err != nil {
		return "", &RequestError{Status: http.StatusUnauthorized, Code: "Unauthorized", Message: err.Error()}
	}
	return issuer, nil
}

func (q *Requester) SubmitBid(ctx context.Context, issuer string, body SubmittedRecord) error {
	if err := market.AuthorizeRecord(issuer, body.URI); err != nil {
		return &RequestError{Status: http.StatusForbidden, Code: "Forbidden", Message: err.Error()}
	}
	bid, err := market.ParseBid(body.Record)
	if err != nil {
		return invalidRequest(err)
	}
	if err := q.storeBidMailbox(ctx, issuer, body.URI, body.CID, body.Record); err != nil {
		q.log.Warn("requester_bid_enqueue_failed", "uri", body.URI, "err", err)
		return &RequestError{Status: http.StatusServiceUnavailable, Code: "Unavailable", Message: err.Error()}
	}
	q.queueBid(bid.RFP.URI, BidRecord{DID: issuer, URI: body.URI, CID: body.CID, Record: body.Record, ArrivedAt: q.now()})
	q.log.Info("requester_bid_received", "bidder", issuer, "uri", body.URI, "rfp", bid.RFP.URI)
	return nil
}

func (q *Requester) SubmitEvent(ctx context.Context, issuer string, body SubmittedRecord) error {
	if body.URI == "" || body.CID == "" || body.Record == nil {
		return &RequestError{Status: http.StatusBadRequest, Code: "InvalidRequest", Message: "missing uri, cid, or record"}
	}
	if err := market.AuthorizeRecord(issuer, body.URI); err != nil {
		return &RequestError{Status: http.StatusForbidden, Code: "Forbidden", Message: err.Error()}
	}
	event, err := market.ParseEvent(body.Record)
	if err != nil {
		return invalidRequest(err)
	}
	if !isGuestEventNSID(market.NSIDFromURI(event.Payload.URI)) {
		return &RequestError{Status: http.StatusBadRequest, Code: "InvalidRequest", Message: "the payload is not a guest event"}
	}
	ref := v1alpha1.ComputeContractEvent{URI: body.URI, CID: body.CID, ReceiptURI: event.Receipt.URI, ReceivedAt: q.nowISO()}
	if err := q.enqueueEvent(ctx, ref, issuer, body.Record); err != nil {
		q.log.Warn("requester_event_enqueue_failed", "uri", body.URI, "err", err)
		return &RequestError{Status: http.StatusServiceUnavailable, Code: "Unavailable", Message: err.Error()}
	}
	q.log.Info("requester_event_queued", "uri", body.URI, "receipt", event.Receipt.URI)
	q.noteEventReceipt(ctx, issuer, market.RecordRef{URI: event.Receipt.URI, CID: event.Receipt.CID})
	return nil
}

func (q *Requester) noteEventReceipt(ctx context.Context, issuer string, ref market.RecordRef) {
	if ref.URI == "" || market.ATURIAuthority(ref.URI) != issuer || market.NSIDFromURI(ref.URI) != market.ReceiptNSID {
		return
	}
	resolved, err := q.repo.Resolve(ctx, ref)
	if err != nil {
		q.log.Warn("requester_event_receipt_unresolved", "receipt", ref.URI, "err", err)
		return
	}
	if _, err := market.ParseReceipt(resolved.Value); err != nil {
		q.log.Warn("requester_event_receipt_invalid", "receipt", ref.URI, "err", err)
		return
	}
	q.noteReceipt(resolved)
}
