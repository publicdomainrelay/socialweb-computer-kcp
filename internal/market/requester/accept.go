package requester

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
)

const (
	submitAcceptProvisionTimeout = 3 * time.Minute

	submitAcceptReceiptTimeout = 5 * time.Second

	acceptSettleDelay = 2 * time.Second

	acceptRetryCeiling = 2 * time.Second

	acceptRetryFloor = 500 * time.Millisecond

	maxQueuedBids = 100
)

type bidQueue struct {
	mu sync.Mutex

	pending map[string][]BidRecord

	changed chan struct{}
}

func newBidQueue() *bidQueue {
	return &bidQueue{pending: map[string][]BidRecord{}, changed: make(chan struct{})}
}

func (b *bidQueue) put(rfpURI string, bid BidRecord) {
	if rfpURI == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	list := b.pending[rfpURI]
	if len(list) >= maxQueuedBids {
		list = list[1:]
	}
	b.pending[rfpURI] = append(list, bid)
	close(b.changed)
	b.changed = make(chan struct{})
}

func (b *bidQueue) take(rfpURI string) []BidRecord {
	b.mu.Lock()
	defer b.mu.Unlock()
	bids := b.pending[rfpURI]
	delete(b.pending, rfpURI)
	return bids
}

func (b *bidQueue) arrived() <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.changed
}

func (q *Requester) queueBid(rfpURI string, bid BidRecord) {
	q.bids.put(rfpURI, bid)
}

func (q *Requester) noteReceipt(record market.ResolvedRecord) {
	accept, _ := record.Value["accept"].(map[string]any)
	acceptURI := stringOf(accept["uri"])
	if acceptURI == "" || record.URI == "" {
		return
	}
	q.receipts.Put(acceptURI, record)
}

func (q *Requester) inboxReceipt(bidderDID, acceptURI string) (*market.ResolvedRecord, bool) {
	record, ok := q.receipts.Get(acceptURI)
	if !ok || market.ATURIAuthority(record.URI) != bidderDID {
		return nil, false
	}
	return &record, true
}

func (q *Requester) awaitReceipt(ctx context.Context, bidderDID, acceptURI string, until time.Time) (*market.ResolvedRecord, error) {
	waitCtx, cancel := context.WithDeadline(ctx, until)
	defer cancel()
	var found *market.ResolvedRecord
	err := q.receipts.Await(waitCtx, func() bool {
		var ok bool
		found, ok = q.inboxReceipt(bidderDID, acceptURI)
		return ok
	})
	if found != nil {
		q.receipts.Remove(acceptURI)
		return found, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, nil
	}
	return nil, err
}

func (q *Requester) readReceipt(ctx context.Context, bidderDID, acceptURI string) (*market.ResolvedRecord, error) {
	if found, ok := q.inboxReceipt(bidderDID, acceptURI); ok {
		q.receipts.Remove(acceptURI)
		return found, nil
	}
	readCtx, cancel := context.WithTimeout(ctx, submitAcceptReceiptTimeout)
	defer cancel()
	return q.market.FindReceipt(readCtx, bidderDID, acceptURI)
}

func (q *Requester) waitReceiptWithin(ctx context.Context, bidderDID, acceptURI string, timeout time.Duration) (*market.ResolvedRecord, error) {
	until := q.now().Add(timeout)
	receipt, readErr := q.readReceipt(ctx, bidderDID, acceptURI)
	if readErr == nil && receipt != nil {
		return receipt, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	receipt, err := q.awaitReceipt(ctx, bidderDID, acceptURI, until)
	if err != nil || receipt != nil {
		return receipt, err
	}
	return nil, readErr
}

func (q *Requester) submitAcceptAndWait(ctx context.Context, bidder Bidder, acceptURI, acceptCID, winnerDID string) (*market.ResolvedRecord, error) {
	provisionTimeout := max(q.cfg.ReceiptTimeout, submitAcceptProvisionTimeout)
	if bidder.Endpoint == "" {
		return q.waitReceiptWithin(ctx, winnerDID, acceptURI, q.cfg.ReceiptTimeout)
	}
	if receipt, err := q.readReceipt(ctx, winnerDID, acceptURI); err == nil && receipt != nil {
		return receipt, nil
	}
	receipt, err := q.awaitReceipt(ctx, winnerDID, acceptURI, q.now().Add(acceptSettleDelay))
	if err != nil || receipt != nil {
		return receipt, err
	}
	if copies := int(q.cfg.Spec.AcceptCopies); copies > 1 {
		copiesCtx, cancel := context.WithTimeout(ctx, provisionTimeout)
		var wg sync.WaitGroup
		for range copies {
			wg.Go(func() {
				_, _ = q.market.SubmitAccept(copiesCtx, bidder, acceptURI, acceptCID)
			})
		}
		wg.Wait()
		cancel()
		receipts := []string{}
		receipt, receiptErr := q.waitReceiptWithin(ctx, winnerDID, acceptURI, provisionTimeout)
		errText := ""
		if receiptErr != nil {
			errText = receiptErr.Error()
		}
		if receiptErr == nil && receipt != nil {
			receipts = append(receipts, receipt.URI)
		}
		q.log.Info("submitAccept_copies", "copies", copies, "receipts", receipts, "err", errText)
		if receiptErr == nil && receipt != nil {
			return receipt, nil
		}
	}
	deadline := q.now().Add(q.cfg.ReceiptTimeout)
	delay := q.cfg.ReceiptPoll
	if delay <= 0 {
		delay = acceptRetryFloor
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if q.now().After(deadline) {
			return nil, nil
		}
		q.log.Warn("submitAccept_retry", "bidder", bidder.DID, "accept", acceptURI, "acceptCid", acceptCID)
		attemptCtx, cancel := context.WithTimeout(ctx, provisionTimeout)
		reply, err := q.market.SubmitAccept(attemptCtx, bidder, acceptURI, acceptCID)
		cancel()
		if err != nil {
			q.log.Warn("requester_submit_accept_failed", "bidder", bidder.DID, "err", err)
		}
		if err == nil && reply != nil && acceptDeclined(reply.Status) {
			q.log.Warn("requester_submit_accept_declined", "bidder", bidder.DID, "accept", acceptURI, "status", reply.Status, "body", reply.Body)
			if receipt, err := q.readReceipt(ctx, winnerDID, acceptURI); err == nil && receipt != nil {
				return receipt, nil
			}
			return nil, nil
		}
		if receipt, err := q.readReceipt(ctx, winnerDID, acceptURI); err == nil && receipt != nil {
			return receipt, nil
		}
		receipt, err := q.awaitReceipt(ctx, winnerDID, acceptURI, q.now().Add(delay))
		if err != nil || receipt != nil {
			return receipt, err
		}
		delay = min(delay*2, acceptRetryCeiling)
	}
}

func acceptDeclined(status int) bool {
	if status < 400 || status >= 500 {
		return false
	}
	return status != http.StatusRequestTimeout && status != http.StatusTooEarly && status != http.StatusTooManyRequests
}

func (q *Requester) rememberBidders(bidders []Bidder) {
	q.bidderMu.Lock()
	defer q.bidderMu.Unlock()
	for _, bidder := range bidders {
		if strings.TrimSpace(bidder.DID) == "" {
			continue
		}
		if bidder.Endpoint == "" && bidder.Audience == "" {
			continue
		}
		if current, ok := q.bidders[bidder.DID]; ok && current.Endpoint != "" && bidder.Endpoint == "" {
			continue
		}
		q.bidders[bidder.DID] = bidder
	}
}

func (q *Requester) bidderFor(did string) (Bidder, bool) {
	q.bidderMu.Lock()
	defer q.bidderMu.Unlock()
	bidder, ok := q.bidders[did]
	return bidder, ok
}

func (q *Requester) bidderForBid(ctx context.Context, bid BidRecord) (Bidder, bool) {
	if bidder, ok := q.bidderFor(bid.DID); ok && bidder.Endpoint != "" {
		return bidder, true
	}
	raw := strings.TrimSpace(stringOf(bid.Record["submitAccept"]))
	if raw == "" {
		return Bidder{}, false
	}
	endpoint, audience, err := resolveBidderEndpoint(ctx, q.deps.Directory, raw)
	if err != nil || endpoint == "" {
		return Bidder{}, false
	}
	bidder := Bidder{DID: bid.DID, Endpoint: endpoint, Audience: audience}
	q.rememberBidders([]Bidder{bidder})
	return bidder, true
}

func (q *Requester) bidderForDID(ctx context.Context, did string) (Bidder, bool) {
	if bidder, ok := q.bidderFor(did); ok && bidder.Endpoint != "" {
		return bidder, true
	}
	if discovered, err := q.market.Discover(ctx, q.now(), []string{did}); err == nil {
		q.rememberBidders(discovered.Bidders)
		if bidder, ok := q.bidderFor(did); ok && bidder.Endpoint != "" {
			return bidder, true
		}
	}
	parsed, err := syntax.ParseDID(did)
	if err != nil {
		return Bidder{}, false
	}
	identityDoc, err := q.deps.Directory.LookupDID(ctx, parsed)
	if err != nil || identityDoc == nil {
		return Bidder{}, false
	}
	for fragment, service := range identityDoc.Services {
		if service.URL == "" {
			continue
		}
		if fragment != defaultMarketServiceID && !strings.Contains(fragment, "market") {
			continue
		}
		endpoint, audience, err := resolveBidderEndpoint(ctx, q.deps.Directory, service.URL)
		if err != nil || endpoint == "" {
			continue
		}
		bidder := Bidder{DID: did, Endpoint: endpoint, Audience: audience}
		q.rememberBidders([]Bidder{bidder})
		return bidder, true
	}
	return Bidder{}, false
}

func (q *Requester) classifyBid(ctx context.Context, bid *BidRecord) error {
	if bid == nil {
		return errors.New("requester: no bid")
	}
	ref := payloadRef(bid.Record)
	if ref.URI == "" {
		return nil
	}
	resolved, err := q.repo.Resolve(ctx, ref)
	if err != nil {
		return err
	}
	switch market.NSIDFromURI(resolved.URI) {
	case market.BidsFreeNSID:
		bid.Free = true
		bid.Reason = stringOf(resolved.Value["reason"])
	case market.BidsX402NSID:
		bid.Cost = numberValue(resolved.Value["cost"])
	}
	if bid.Free {
		bid.Cost = 0
	}
	return nil
}

func (q *Requester) bidAllowedWithPolicy(ctx context.Context, bid BidRecord, usePolicy bool, workflow string) bool {
	if !usePolicy {
		return true
	}
	key := bid.DID + "|" + workflow
	if decision, ok := q.scopeCache.Get(key); ok {
		return decision.Allow
	}
	if q.policy == nil || (q.policy.Workflow == "" && workflow == "") {
		return true
	}
	self := q.marketDID
	if operator := strings.TrimSpace(q.cfg.Spec.OperatorDID); operator != "" {
		self = operator
	}
	input := PolicyInput{
		SelfDid:          self,
		SubjectDid:       bid.DID,
		CounterpartyDid:  bid.DID,
		RootRequesterDid: self,
	}
	if rfp, _ := bid.Record["rfp"].(map[string]any); rfp != nil {
		input.RFP = &PolicyRef{URI: stringOf(rfp["uri"]), CID: stringOf(rfp["cid"])}
	}
	decision := q.policy.Scope(ctx, input)
	q.scopeCache.Set(key, decision)
	if !decision.Allow {
		q.log.Info("requester_bid_policy_denied", "bidder", bid.DID, "reason", decision.Reason)
	}
	return decision.Allow
}
