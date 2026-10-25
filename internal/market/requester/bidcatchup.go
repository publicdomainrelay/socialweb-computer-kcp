package requester

import (
	"context"
	"errors"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
)

const (
	maxBidderRepos = 32

	bidCatchUpDelay = 5 * time.Second
)

func consideredBidderDIDs(bidders []Bidder, only []string) []string {
	dids := append([]string(nil), only...)
	for _, bidder := range bidders {
		dids = append(dids, bidder.DID)
	}
	dids = BidderDIDList(dids)
	if len(dids) > maxBidderRepos {
		dids = dids[:maxBidderRepos]
	}
	return dids
}

func (r *contractRunner) catchUpBids(ctx context.Context, dids []string, rfpURI, rfpCID string) {
	if len(dids) == 0 || rfpURI == "" || rfpCID == "" {
		return
	}
	delay := min(bidCatchUpDelay, max(r.q.cfg.BidWindow/2, time.Second))
	r.q.wg.Go(func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		for _, did := range dids {
			record, err := r.q.repo.GetRecord(ctx, did, market.BidNSID, market.BidRKey(did, rfpURI, rfpCID))
			if err != nil {
				if !errors.Is(err, market.ErrNotFound) && ctx.Err() == nil {
					r.log().Debug("requester_bid_catch_up_failed", "bidder", did, "err", err)
				}
				continue
			}
			if err := r.q.storeBidMailbox(ctx, did, record.URI, record.CID, record.Value); err != nil {
				if ctx.Err() == nil {
					r.log().Warn("requester_bid_catch_up_failed", "bidder", did, "bid", record.URI, "err", err)
				}
				continue
			}
			r.log().Info("requester_bid_catch_up", "bidder", did, "bid", record.URI)
		}
	})
}
