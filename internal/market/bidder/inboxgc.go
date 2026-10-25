package bidder

import (
	"context"
	"errors"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	contract "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/market"
)

const (
	MarketInboxRetention = market.RFPBidWindow

	inboxPrunedLogEvery = 100
)

func (b *Bidder) collectInbox(ctx context.Context, sync v1alpha1.StateSync) (time.Duration, bool) {
	if sync.Spec.Sink.Adapter != MarketInboxAdapter || sync.DeletionTimestamp != nil {
		return 0, false
	}
	var payload marketInboxPayload
	foreign := payload.decode(sync.Spec.Payload) != nil || !market.BidderConsumes(payload.Collection) ||
		!market.BidderAddresseesOf(b.cfg.DID).BidderAddressed(payload.Collection, payload.Record)
	if !foreign {
		if sync.Status.Phase != v1alpha1.StateSyncPhaseAcked {
			return 0, false
		}
		if left := sync.CreationTimestamp.Add(MarketInboxRetention).Sub(b.now()); left > 0 {
			return left, false
		}
	}
	if err := b.intents.DeleteStateSync(ctx, sync.Name); err != nil && !errors.Is(err, contract.ErrNotFound) {
		if ctx.Err() == nil {
			b.log.Warn("bidder_inbox_prune_failed", "stateSync", sync.Name, "err", err)
		}
		return 0, false
	}
	var n int64
	if foreign {
		n = b.prunedForeign.Add(1)
	} else {
		n = b.prunedAged.Add(1)
	}
	if n == 1 || n%inboxPrunedLogEvery == 0 {
		b.log.Info("bidder_inbox_pruned", "foreign", b.prunedForeign.Load(), "aged", b.prunedAged.Load())
	}
	return 0, true
}
