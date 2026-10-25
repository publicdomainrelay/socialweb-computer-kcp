package requester

import (
	"context"
	"encoding/json"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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

	inboxUnknown

	inboxEnded
)

func (q *Requester) Synced() {
	q.synced.Store(true)
}

func (q *Requester) addressees(skip func(v1alpha1.ComputeContract) bool) *market.Addressees {
	a := market.RequesterAddresseesOf(q.contracts.List(), skip)
	q.mu.Lock()
	runners := make([]*contractRunner, 0, len(q.runners))
	for _, runner := range q.runners {
		runners = append(runners, runner)
	}
	q.mu.Unlock()
	for _, runner := range runners {
		contract, ok := q.contracts.Get(runner.name)
		if !ok || (skip != nil && skip(contract)) {
			continue
		}
		refs := market.RefsOfContract(runner.stateCopy())
		a.Add(market.ContractRef{Name: contract.Name, UID: string(contract.UID)}, refs, skip == nil || refs.Accept == "")
	}
	return a
}

func (q *Requester) contractOf(ref market.ContractRef) (v1alpha1.ComputeContract, bool) {
	contract, ok := q.contracts.Get(ref.Name)
	if !ok || (ref.UID != "" && string(contract.UID) != ref.UID) {
		return v1alpha1.ComputeContract{}, false
	}
	return contract, true
}

func (q *Requester) chaseReceipt(ctx context.Context, a *market.Addressees, record map[string]any) (market.ContractRef, bool) {
	event, err := market.ParseEvent(record)
	if err != nil {
		return market.ContractRef{}, false
	}
	receipt, err := q.repo.Resolve(ctx, market.RecordRef{URI: event.Receipt.URI, CID: event.Receipt.CID})
	if err != nil {
		return market.ContractRef{}, false
	}
	return a.ReceiptMatch(receipt.Value)
}

func (q *Requester) recordOwner(ctx context.Context, collection, author string, record map[string]any) (v1alpha1.ComputeContract, bool) {
	a := q.addressees(nil)
	ref, chase, ok := a.RequesterMatch(collection, author, record)
	if !ok && chase != "" {
		ref, ok = q.chaseReceipt(ctx, a, record)
	}
	if !ok {
		return v1alpha1.ComputeContract{}, false
	}
	return q.contractOf(ref)
}

func contractOwners(contract v1alpha1.ComputeContract) []metav1.OwnerReference {
	return []metav1.OwnerReference{contractOwnerReference(contract)}
}

func (q *Requester) ownerOfSync(item v1alpha1.StateSync) (v1alpha1.ComputeContract, bool) {
	for _, ref := range item.OwnerReferences {
		if ref.Kind != "ComputeContract" {
			continue
		}
		if contract, ok := q.contractOf(market.ContractRef{Name: ref.Name, UID: string(ref.UID)}); ok {
			return contract, true
		}
	}
	return v1alpha1.ComputeContract{}, false
}

func (q *Requester) inboxVerdict(item v1alpha1.StateSync) inboxVerdict {
	switch item.Spec.Sink.Adapter {
	case MarketInboxAdapter, BidInboxAdapter:
		payload, err := decodeMarketInboxPayload(item.Spec.Payload)
		if err != nil || !market.RequesterConsumes(payload.Collection) {
			return inboxForeign
		}
		if _, _, ok := q.addressees(market.RequesterContractEnded).RequesterMatch(payload.Collection, payload.DID, payload.Record); ok {
			return inboxKeep
		}
		_, chase, ok := q.addressees(nil).RequesterMatch(payload.Collection, payload.DID, payload.Record)
		switch {
		case ok:
			return inboxEnded
		case chase != "":
			if contract, owned := q.ownerOfSync(item); owned {
				if market.RequesterContractEnded(contract) {
					return inboxEnded
				}
				return inboxKeep
			}
			return inboxUnknown
		}
		return inboxForeign
	case InboxAdapter:
		var payload inboxEventPayload
		if err := json.Unmarshal(item.Spec.Payload, &payload); err != nil || payload.Event.URI == "" {
			return inboxForeign
		}
		contract, ok := q.contractForReceiptURI(payload.Event.ReceiptURI)
		if !ok {
			contract, ok = q.ownerOfSync(item)
		}
		switch {
		case !ok:
			return inboxUnknown
		case contractPhaseDone(contract.Status.Phase):
			return inboxEnded
		}
		return inboxKeep
	}
	return inboxKeep
}

func (q *Requester) collectInbox(ctx context.Context, item v1alpha1.StateSync) (time.Duration, bool) {
	if !q.synced.Load() || item.DeletionTimestamp != nil {
		return 0, false
	}
	verdict := q.inboxVerdict(item)
	switch verdict {
	case inboxKeep:
		return 0, false
	case inboxEnded:
		if item.Spec.Sink.Adapter == MarketInboxAdapter && item.Status.Phase != v1alpha1.StateSyncPhaseAcked {
			return 0, false
		}
		fallthrough
	case inboxUnknown:
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
	if verdict == inboxEnded {
		n = q.prunedEnded.Add(1)
	} else {
		n = q.prunedForeign.Add(1)
	}
	if n == 1 || n%inboxPrunedLogEvery == 0 {
		q.log.Info("requester_inbox_pruned", "foreign", q.prunedForeign.Load(), "ended", q.prunedEnded.Load(), "adapter", item.Spec.Sink.Adapter)
	}
	return 0, true
}
