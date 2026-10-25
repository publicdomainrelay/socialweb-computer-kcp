package requester

import (
	"cmp"
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/xrpc"
)

const (
	bidderRepoLookupTimeout = 10 * time.Second

	maxBidderRepos = 32

	bidCatchUpDelay = 5 * time.Second

	getRepoStatusNSID = "com.atproto.sync.getRepoStatus"
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

func (q *Requester) bidderRepos(ctx context.Context, dids []string) []v1alpha1.ContractBidderRepo {
	if q.deps.Directory == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, bidderRepoLookupTimeout)
	defer cancel()
	seen := q.nowISO()
	out := make([]v1alpha1.ContractBidderRepo, 0, len(dids))
	for _, did := range dids {
		parsed, err := syntax.ParseDID(did)
		if err != nil {
			continue
		}
		ident, err := q.deps.Directory.LookupDID(ctx, parsed)
		if err != nil || ident == nil {
			q.log.Warn("requester_bidder_pds_unresolved", "bidder", did, "err", err)
			continue
		}
		if pds := cmp.Or(q.relayCrawledPDS(ctx, did), strings.TrimSpace(ident.PDSEndpoint())); pds != "" {
			out = append(out, v1alpha1.ContractBidderRepo{Did: did, Pds: pds, SeenAt: seen})
		}
	}
	return out
}

func (q *Requester) relayCrawledPDS(ctx context.Context, did string) string {
	for _, relay := range q.cfg.Relays() {
		status, err := xrpc.Query[struct {
			Hostname string `json:"hostname"`
		}](ctx, &xrpc.Client{Host: relay, HTTP: q.deps.HTTP}, getRepoStatusNSID, url.Values{"did": {did}})
		host := strings.TrimSpace(status.Hostname)
		if err != nil || host == "" || strings.ContainsAny(host, "/ ") {
			continue
		}
		if _, port, err := net.SplitHostPort(host); err == nil && port != "" {
			return "http://" + host
		}
		return "https://" + host
	}
	return ""
}

func (r *contractRunner) noteBidderRepos(ctx context.Context, dids []string) {
	repos := r.q.bidderRepos(ctx, dids)
	if len(repos) == 0 || r.isGone() {
		return
	}
	if err := r.q.deps.Intents.MergeComputeContractStatus(ctx, r.name, v1alpha1.ComputeContractStatus{BidderRepos: repos}); err != nil {
		if !notFound(err) {
			r.log().Warn("requester_bidder_repos_failed", "err", err)
		}
		return
	}
	pdses := make([]string, 0, len(repos))
	for _, repo := range repos {
		pdses = append(pdses, repo.Did+"@"+repo.Pds)
	}
	r.log().Info("requester_bidder_repos", "bidders", pdses)
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
