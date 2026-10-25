package requester

import (
	"context"
	"encoding/json"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
)

const (
	MarketInboxRetention = 10 * time.Minute

	inboxPrunedLogEvery = 100
)

type inboxVerdict int

const (
	inboxKeep inboxVerdict = iota

	inboxForeign

	inboxEnded
)

func (q *Requester) Synced() {
	q.synced.Store(true)
}

func (q *Requester) ownDIDs() []string {
	return []string{q.did, q.repoDID, q.marketDID}
}

func (q *Requester) inboxVerdict(item v1alpha1.StateSync) (inboxVerdict, bool) {
	switch item.Spec.Sink.Adapter {
	case MarketInboxAdapter, BidInboxAdapter:
		payload, err := decodeMarketInboxPayload(item.Spec.Payload)
		if err != nil {
			return inboxForeign, true
		}
		if !market.RequesterConsumes(payload.Collection) {
			return inboxForeign, true
		}
		contracts := q.contracts.List()
		if market.RequesterAddresseesOf(contracts, true, q.ownDIDs()...).RequesterAddressed(payload.Collection, payload.DID, payload.Record) {
			return inboxKeep, true
		}
		if !market.RequesterAddresseesOf(contracts, false, q.ownDIDs()...).RequesterAddressed(payload.Collection, payload.DID, payload.Record) {
			return inboxForeign, true
		}
		return inboxEnded, true
	case InboxAdapter:
		var payload inboxEventPayload
		if err := json.Unmarshal(item.Spec.Payload, &payload); err != nil || payload.Event.URI == "" {
			return inboxForeign, true
		}
		contract, ok := q.contractForReceiptURI(payload.Event.ReceiptURI)
		switch {
		case !ok:
			return inboxForeign, false
		case contractPhaseDone(contract.Status.Phase):
			return inboxEnded, false
		}
		return inboxKeep, false
	}
	return inboxKeep, false
}

func (q *Requester) collectInbox(ctx context.Context, item v1alpha1.StateSync) (time.Duration, bool) {
	if !q.synced.Load() || item.DeletionTimestamp != nil {
		return 0, false
	}
	verdict, immediate := q.inboxVerdict(item)
	if verdict == inboxKeep {
		return 0, false
	}
	if verdict == inboxEnded && item.Spec.Sink.Adapter == MarketInboxAdapter && item.Status.Phase != v1alpha1.StateSyncPhaseAcked {
		return 0, false
	}
	if verdict == inboxEnded || !immediate {
		if left := item.CreationTimestamp.Add(MarketInboxRetention).Sub(q.now()); left > 0 {
			return left, false
		}
	}
	q.deleteMailbox(ctx, item.Name, "requester_inbox_prune_failed")
	if _, ok := q.syncs.Get(item.Name); ok {
		return 0, false
	}
	q.inboxed.Delete(string(item.UID) + "#" + item.Spec.IdempotencyKey)
	var n int64
	if verdict == inboxForeign {
		n = q.prunedForeign.Add(1)
	} else {
		n = q.prunedEnded.Add(1)
	}
	if n == 1 || n%inboxPrunedLogEvery == 0 {
		q.log.Info("requester_inbox_pruned", "foreign", q.prunedForeign.Load(), "ended", q.prunedEnded.Load(), "adapter", item.Spec.Sink.Adapter)
	}
	return 0, true
}
