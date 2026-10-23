package requester

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	contractmarket "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/market/participant"
)

const (
	InboxAdapter = "requester.inbox"

	BidInboxAdapter = "requester.bid-inbox"

	MarketInboxAdapter = "requester.market-inbox"

	inboxLabel = "socialweb.computer/inbox"

	inboxPrefix = "inbox-"

	bidInboxLabel = "socialweb.computer/bid-inbox"

	bidInboxPrefix = "bid-"

	bidVerifyTimeout = 10 * time.Second

	bidVerifyRetry = 2 * time.Second

	maxPendingEvents = 256
)

type inboxEventPayload struct {
	Event v1alpha1.ComputeContractEvent `json:"event"`
}

type bidInboxPayload struct {
	DID string `json:"did"`

	URI string `json:"uri"`

	CID string `json:"cid"`

	Record map[string]any `json:"record"`
}

type marketInboxPayload struct {
	DID string `json:"did"`

	URI string `json:"uri"`

	CID string `json:"cid"`

	Collection string `json:"collection"`

	Record map[string]any `json:"record"`
}

func sameEventRef(a, b v1alpha1.ComputeContractEvent) bool {
	return a.URI != "" && a.URI == b.URI && (a.CID == "" || b.CID == "" || a.CID == b.CID)
}

func samePendingEvents(left, right []v1alpha1.ComputeContractEvent) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if !sameEventRef(left[i], right[i]) {
			return false
		}
	}
	return true
}

func inboxName(event v1alpha1.ComputeContractEvent) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(event.ReceiptURI) + "\x00" + event.URI + "#" + event.CID))
	return inboxPrefix + hex.EncodeToString(sum[:16])
}

func bidInboxName(uri, cid string) string {
	sum := sha256.Sum256([]byte(uri + "#" + cid))
	return bidInboxPrefix + hex.EncodeToString(sum[:16])
}

func mailbox(name, label, adapter, key string, payload []byte) v1alpha1.StateSync {
	return v1alpha1.StateSync{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: contractmarket.KindStateSync},
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{label: "true"}},
		Spec: v1alpha1.StateSyncSpec{
			Sink:           v1alpha1.StateSyncSink{Adapter: adapter},
			Direction:      v1alpha1.StateSyncDirectionOutbound,
			Payload:        payload,
			IdempotencyKey: key,
			ConflictPolicy: v1alpha1.StateSyncConflictSourceWins,
			DeletionPolicy: v1alpha1.StateSyncDeletionRetain,
		},
	}
}

func (q *Requester) createMailbox(ctx context.Context, sync v1alpha1.StateSync) error {
	err := q.deps.Intents.CreateStateSync(ctx, sync)
	if err == nil || errors.Is(err, contractmarket.ErrExists) {
		if _, ok := q.syncs.Get(sync.Name); !ok {
			q.syncs.Put(sync.Name, sync)
		}
		return nil
	}
	return err
}

func (q *Requester) deleteMailbox(ctx context.Context, name, failure string) {
	if err := q.deps.Intents.DeleteStateSync(ctx, name); err != nil {
		if ctx.Err() == nil {
			q.log.Warn(failure, "statesync", name, "err", err)
		}
		return
	}
	q.syncs.Remove(name)
}

func (q *Requester) storeBidMailbox(ctx context.Context, did, uri, cid string, record map[string]any) error {
	if uri == "" || cid == "" {
		return errors.New("requester: the bid ref is incomplete")
	}
	payload, err := json.Marshal(bidInboxPayload{DID: did, URI: uri, CID: cid, Record: record})
	if err != nil {
		return err
	}
	return q.createMailbox(ctx, mailbox(bidInboxName(uri, cid), bidInboxLabel, BidInboxAdapter, "bid:"+uri+"#"+cid, payload))
}

func (q *Requester) storeInboxEvent(ctx context.Context, event v1alpha1.ComputeContractEvent) error {
	payload, err := json.Marshal(inboxEventPayload{Event: event})
	if err != nil {
		return err
	}
	return q.createMailbox(ctx, mailbox(inboxName(event), inboxLabel, InboxAdapter, "inbox:"+event.URI+"#"+event.CID, payload))
}

func (q *Requester) processBidMailbox(ctx context.Context, item v1alpha1.StateSync) time.Duration {
	var payload bidInboxPayload
	if err := json.Unmarshal(item.Spec.Payload, &payload); err != nil || payload.URI == "" || payload.CID == "" {
		q.log.Warn("requester_bid_inbox_invalid", "statesync", item.Name)
		q.deleteMailbox(ctx, item.Name, "requester_bid_inbox_delete_failed")
		return 0
	}
	if payload.DID == "" || payload.DID != market.ATURIAuthority(payload.URI) {
		q.log.Warn("requester_bid_authority_mismatch", "statesync", item.Name, "bid", payload.URI, "bidder", payload.DID)
		q.deleteMailbox(ctx, item.Name, "requester_bid_inbox_delete_failed")
		return 0
	}
	bid, err := market.ParseBid(payload.Record)
	if err != nil {
		q.log.Warn("requester_bid_inbox_invalid", "statesync", item.Name, "bid", payload.URI, "err", err)
		q.deleteMailbox(ctx, item.Name, "requester_bid_inbox_delete_failed")
		return 0
	}
	vctx, cancel := context.WithTimeout(ctx, bidVerifyTimeout)
	valid := market.VerifyRecordSignatures(vctx, payload.Record, market.ATURIAuthority(payload.URI), q.verifyOptions())
	verr := vctx.Err()
	cancel()
	if verr != nil {
		q.log.Warn("requester_bid_verify_unavailable", "statesync", item.Name, "bid", payload.URI, "err", verr)
		return bidVerifyRetry
	}
	if !valid {
		q.log.Warn("requester_bid_refused", "statesync", item.Name, "bid", payload.URI, "bidder", payload.DID)
		q.deleteMailbox(ctx, item.Name, "requester_bid_inbox_delete_failed")
		return 0
	}
	q.queueBid(bid.RFP.URI, BidRecord{DID: payload.DID, URI: payload.URI, CID: payload.CID, Record: payload.Record, ArrivedAt: q.now()})
	q.log.Info("requester_bid_recovered", "bidder", payload.DID, "uri", payload.URI, "rfp", bid.RFP.URI)
	q.deleteMailbox(ctx, item.Name, "requester_bid_inbox_delete_failed")
	return 0
}

func (q *Requester) contractForReceiptURI(receiptURI string) (v1alpha1.ComputeContract, bool) {
	return q.contracts.Find(func(contract v1alpha1.ComputeContract) bool {
		state := contract.Status.Contract
		return state != nil && state.Receipt != nil && state.Receipt.URI == receiptURI
	})
}

func (q *Requester) pendingOf(contract v1alpha1.ComputeContract) []v1alpha1.ComputeContractEvent {
	if written, ok := q.written[contract.Name]; ok {
		return written
	}
	return contract.Status.PendingEvents
}

func (q *Requester) writePending(ctx context.Context, name string, events []v1alpha1.ComputeContractEvent) error {
	next := append([]v1alpha1.ComputeContractEvent{}, events...)
	if err := q.deps.Intents.SetComputeContractPendingEvents(ctx, name, next); err != nil {
		return err
	}
	q.written[name] = next
	return nil
}

func (q *Requester) observePending(contract v1alpha1.ComputeContract) {
	q.pendingMu.Lock()
	defer q.pendingMu.Unlock()
	if written, ok := q.written[contract.Name]; ok && samePendingEvents(written, contract.Status.PendingEvents) {
		delete(q.written, contract.Name)
	}
}

func (q *Requester) forgetPending(name string) {
	q.pendingMu.Lock()
	delete(q.written, name)
	q.pendingMu.Unlock()
}

func (q *Requester) appendPending(ctx context.Context, contract v1alpha1.ComputeContract, event v1alpha1.ComputeContractEvent, full error) (bool, error) {
	pending := q.pendingOf(contract)
	for _, existing := range pending {
		if sameEventRef(existing, event) {
			return true, nil
		}
	}
	if len(pending) >= maxPendingEvents {
		return false, full
	}
	next := make([]v1alpha1.ComputeContractEvent, 0, len(pending)+1)
	next = append(next, pending...)
	next = append(next, event)
	if err := q.writePending(ctx, contract.Name, next); err != nil {
		return false, err
	}
	return true, nil
}

func (q *Requester) enqueueEvent(ctx context.Context, event v1alpha1.ComputeContractEvent) error {
	if event.URI == "" || event.CID == "" || event.ReceiptURI == "" {
		return errors.New("requester: the event ref is incomplete")
	}
	q.pendingMu.Lock()
	defer q.pendingMu.Unlock()
	contract, ok := q.contractForReceiptURI(event.ReceiptURI)
	if !ok {
		return q.storeInboxEvent(ctx, event)
	}
	_, err := q.appendPending(ctx, contract, event, errors.New("requester: the pending event inbox is full"))
	return err
}

func (q *Requester) mergeInboxMailbox(ctx context.Context, item v1alpha1.StateSync) {
	var payload inboxEventPayload
	if err := json.Unmarshal(item.Spec.Payload, &payload); err != nil || payload.Event.URI == "" {
		q.deleteMailbox(ctx, item.Name, "requester_inbox_delete_failed")
		return
	}
	q.pendingMu.Lock()
	contract, ok := q.contractForReceiptURI(payload.Event.ReceiptURI)
	if !ok {
		q.pendingMu.Unlock()
		return
	}
	merged, err := q.appendPending(ctx, contract, payload.Event, errInboxFull)
	q.pendingMu.Unlock()
	switch {
	case errors.Is(err, errInboxFull):
		q.log.Warn("requester_inbox_full", "contract", contract.Name, "event", payload.Event.URI)
		return
	case err != nil:
		if ctx.Err() == nil {
			q.log.Warn("requester_inbox_append_failed", "contract", contract.Name, "event", payload.Event.URI, "err", err)
		}
		return
	}
	if merged {
		q.deleteMailbox(ctx, item.Name, "requester_inbox_delete_failed")
	}
}

var errInboxFull = errors.New("requester: the pending event inbox is full")

func (q *Requester) mergeInboxFor(ctx context.Context, contract v1alpha1.ComputeContract) {
	state := contract.Status.Contract
	if state == nil || state.Receipt == nil || state.Receipt.URI == "" {
		return
	}
	receiptURI := state.Receipt.URI
	for _, item := range q.syncs.Filter(func(sync v1alpha1.StateSync) bool { return sync.Spec.Sink.Adapter == InboxAdapter }) {
		var payload inboxEventPayload
		if json.Unmarshal(item.Spec.Payload, &payload) == nil && payload.Event.ReceiptURI != receiptURI {
			continue
		}
		q.mergeInboxMailbox(ctx, item)
	}
}

func (q *Requester) resolveInboxEvent(ctx context.Context, event v1alpha1.ComputeContractEvent) (MarketEvent, error) {
	resolved, err := q.repo.Resolve(ctx, market.RecordRef{URI: event.URI, CID: event.CID})
	if err != nil {
		return MarketEvent{}, err
	}
	envelope, err := market.ParseEvent(resolved.Value)
	if err != nil {
		return MarketEvent{}, err
	}
	payload, err := q.repo.Resolve(ctx, market.RecordRef{URI: envelope.Payload.URI, CID: envelope.Payload.CID})
	if err != nil {
		return MarketEvent{}, err
	}
	return MarketEvent{Event: resolved, Payload: payload}, nil
}

func (q *Requester) processPendingEvents(ctx context.Context, name string) {
	q.pendingMu.Lock()
	defer q.pendingMu.Unlock()
	contract, ok := q.contracts.Get(name)
	if !ok {
		return
	}
	pending := q.pendingOf(contract)
	if len(pending) == 0 {
		return
	}
	remaining := make([]v1alpha1.ComputeContractEvent, 0, len(pending))
	seen := map[string]bool{}
	for _, entry := range pending {
		key := entry.URI + "#" + entry.CID
		if seen[key] {
			continue
		}
		seen[key] = true
		event, err := q.resolveInboxEvent(ctx, entry)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			q.log.Warn("requester_event_unavailable", "contract", contract.Name, "event", entry.URI, "err", err)
			remaining = append(remaining, entry)
			continue
		}
		receiptURI := strings.TrimSpace(entry.ReceiptURI)
		parsed, err := q.parseGuestEvent(ctx, event, receiptURI)
		if err != nil {
			q.log.Warn("requester_event_refused", "contract", contract.Name, "event", entry.URI, "err", err)
			continue
		}
		if guestEventApplied(contract.Status.Contract, parsed) {
			continue
		}
		if q.runnerFor(contract.Name) != nil {
			q.dispatchEvent(receiptURI, event)
			remaining = append(remaining, entry)
			continue
		}
		if contractPhaseDone(contract.Status.Phase) {
			q.log.Info("requester_event_dropped_after_contract_end", "contract", contract.Name, "event", entry.URI)
			continue
		}
		remaining = append(remaining, entry)
	}
	if samePendingEvents(remaining, pending) {
		return
	}
	if err := q.writePending(ctx, contract.Name, remaining); err != nil && ctx.Err() == nil {
		q.log.Warn("requester_event_inbox_update_failed", "contract", contract.Name, "err", err)
	}
}

func (q *Requester) registerEventSink(receiptURI string) (<-chan MarketEvent, []MarketEvent, func()) {
	q.eventMu.Lock()
	channel := make(chan MarketEvent, 32)
	q.eventSinks[receiptURI] = channel
	pending := q.undelivered[receiptURI]
	delete(q.undelivered, receiptURI)
	q.eventMu.Unlock()
	closeFn := func() {
		q.eventMu.Lock()
		if q.eventSinks[receiptURI] == channel {
			delete(q.eventSinks, receiptURI)
		}
		q.eventMu.Unlock()
	}
	return channel, pending, closeFn
}

func (q *Requester) dispatchEvent(receiptURI string, event MarketEvent) {
	if receiptURI == "" {
		return
	}
	q.eventMu.Lock()
	defer q.eventMu.Unlock()
	if channel, ok := q.eventSinks[receiptURI]; ok {
		select {
		case channel <- event:
		default:
		}
		return
	}
	list := q.undelivered[receiptURI]
	if len(list) >= 64 {
		list = list[1:]
	}
	q.undelivered[receiptURI] = append(list, event)
}

func decodeMarketInboxPayload(raw json.RawMessage) (marketInboxPayload, error) {
	var payload marketInboxPayload
	if len(raw) == 0 {
		return payload, errors.New("requester: the market inbox payload is empty")
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return payload, fmt.Errorf("requester: the market inbox payload is not JSON: %w", err)
	}
	if strings.TrimSpace(payload.URI) == "" || strings.TrimSpace(payload.CID) == "" {
		return payload, errors.New("requester: the market inbox payload names no record")
	}
	if strings.TrimSpace(payload.Collection) == "" {
		payload.Collection = market.NSIDFromURI(payload.URI)
	}
	return payload, nil
}

func isBidCollection(collection string) bool {
	switch collection {
	case market.BidNSID, market.BidsFreeNSID, market.BidsX402NSID:
		return true
	}
	return false
}

type marketInbox struct {
	q *Requester
}

var _ participant.Adapter = (*marketInbox)(nil)

func (a *marketInbox) Name() string { return MarketInboxAdapter }

func (a *marketInbox) Observe(ctx context.Context, request participant.OutboxRequest) (participant.Observation, error) {
	payload, err := decodeMarketInboxPayload(request.Payload)
	if err != nil {
		return participant.Observation{}, err
	}
	if isBidCollection(payload.Collection) {
		if _, ok := a.q.syncs.Get(bidInboxName(payload.URI, payload.CID)); !ok {
			return participant.Observation{At: time.Now().UTC().Format(time.RFC3339)}, nil
		}
		return participant.InboxObservation(request, payload.URI), nil
	}
	if payload.Collection == market.ReceiptNSID {
		a.q.noteReceipt(market.ResolvedRecord{URI: payload.URI, CID: payload.CID, Value: payload.Record})
	}
	return participant.InboxObservation(request, request.Key), nil
}

func (a *marketInbox) Apply(ctx context.Context, request participant.OutboxRequest) (participant.Observation, error) {
	payload, err := decodeMarketInboxPayload(request.Payload)
	if err != nil {
		return participant.Observation{}, err
	}
	q := a.q
	switch {
	case isBidCollection(payload.Collection):
		if err := q.storeBidMailbox(ctx, payload.DID, payload.URI, payload.CID, payload.Record); err != nil {
			return participant.Observation{}, err
		}
	case payload.Collection == market.ReceiptNSID:
		q.noteReceipt(market.ResolvedRecord{URI: payload.URI, CID: payload.CID, Value: payload.Record})
		event := v1alpha1.ComputeContractEvent{URI: payload.URI, CID: payload.CID, ReceiptURI: payload.URI, ReceivedAt: q.nowISO()}
		if err := q.storeInboxEvent(ctx, event); err != nil {
			return participant.Observation{}, err
		}
	case payload.Collection == market.EventNSID:
		envelope, err := market.ParseEvent(payload.Record)
		if err != nil {
			return participant.Observation{}, fmt.Errorf("requester: the inbox event is invalid: %w", err)
		}
		event := v1alpha1.ComputeContractEvent{URI: payload.URI, CID: payload.CID, ReceiptURI: envelope.Receipt.URI, ReceivedAt: q.nowISO()}
		if err := q.storeInboxEvent(ctx, event); err != nil {
			return participant.Observation{}, err
		}
	}
	return participant.InboxObservation(request, payload.URI), nil
}

func (a *marketInbox) Delete(ctx context.Context, request participant.OutboxRequest) error {
	if _, err := decodeMarketInboxPayload(request.Payload); err != nil {
		a.q.log.Debug("requester_market_inbox_release", "key", request.Key, "err", err)
	}
	return nil
}

type inboxAck struct {
	name string
}

var _ participant.Adapter = inboxAck{}

func (a inboxAck) Name() string { return a.name }

func (a inboxAck) Observe(ctx context.Context, request participant.OutboxRequest) (participant.Observation, error) {
	return participant.InboxObservation(request, request.Key), nil
}

func (a inboxAck) Apply(ctx context.Context, request participant.OutboxRequest) (participant.Observation, error) {
	return a.Observe(ctx, request)
}

func (a inboxAck) Delete(ctx context.Context, request participant.OutboxRequest) error {
	return ctx.Err()
}
