package requester

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/identlink"
	peatproto "github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/atproto"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/trust"
)

const (
	sourceConfigured = "configured"

	sourceRelayPrefix = "relay:"

	sourceVouched = "vouched"

	sourceOperated = "operated"

	trustListLimit = 100

	maxTrustRepos = 64
)

type candidateSource struct {
	name string

	dids []string

	named bool
}

func didValue(value any) string {
	s, _ := value.(string)
	if s = strings.TrimSpace(s); strings.HasPrefix(s, "did:") {
		return s
	}
	return ""
}

func (m *HTTPMarket) listRepo(ctx context.Context, did, collection string) []market.ListedRecord {
	records, err := listAll(ctx, m.Reader, did, collection, trustListLimit)
	if err != nil && !errors.Is(err, market.ErrNotFound) && ctx.Err() == nil {
		m.log().Debug("requester_discovery_list_failed", "did", did, "collection", collection, "err", err)
	}
	return records
}

func (m *HTTPMarket) selfDIDs() []string {
	return BidderDIDList([]string{m.RepoDID})
}

func (q *Requester) trustResolver() *trust.Resolver {
	client := peatproto.NewClient()
	client.HTTP = q.marketHTTP()
	if network := q.cfg.Spec.Network; network != nil && strings.TrimSpace(network.PLCDirectoryURL) != "" {
		client.PLCURL = strings.TrimSpace(network.PLCDirectoryURL)
	}
	return trust.NewResolver(trust.ResolverOptions{Client: client, Source: identlink.NewRemote(q.deps.Directory, client.HTTP)})
}

func (m *HTTPMarket) walk(ctx context.Context, did string) (identlink.Result, bool) {
	if m.Trust == nil {
		return identlink.Result{}, false
	}
	walked, err := m.Trust.Walk(ctx, did, trust.Association{})
	if err != nil {
		m.log().Debug("requester_discovery_walk_failed", "did", did, "err", err)
		return identlink.Result{}, false
	}
	return walked, true
}

func (m *HTTPMarket) operatorsOf(ctx context.Context, self string) []string {
	walked, ok := m.walk(ctx, self)
	if !ok {
		return nil
	}
	return slices.DeleteFunc(slices.Clone(walked.Roots), func(root string) bool { return root == self })
}

func (m *HTTPMarket) vouchedBy(ctx context.Context, did string) []string {
	var out []string
	for _, record := range m.listRepo(ctx, did, market.VouchNSID) {
		if kind, _ := record.Value["kind"].(string); kind == "denounce" {
			continue
		}
		if rkey := record.URI[strings.LastIndex(record.URI, "/")+1:]; strings.HasPrefix(rkey, "did:") && rkey != did {
			out = append(out, rkey)
		}
	}
	return out
}

func (m *HTTPMarket) operatedBy(ctx context.Context, operator string) []string {
	var candidates []string
	for _, record := range m.listRepo(ctx, operator, identlink.EndorsementNSID) {
		if did := didValue(record.Value["agent"]); did != "" && did != operator && strings.HasSuffix(record.URI, "/"+did) {
			candidates = append(candidates, did)
		}
	}
	var out []string
	for _, did := range BidderDIDList(candidates) {
		if walked, ok := m.walk(ctx, did); !ok || !walked.Has(operator) {
			m.log().Info("requester_operated_unconfirmed", "operator", operator, "bidder", did)
			continue
		}
		out = append(out, did)
	}
	return out
}

func eachParallel(items []string, fn func(string) []string) []string {
	var mu sync.Mutex
	var wg sync.WaitGroup
	var out []string
	for _, item := range items {
		wg.Go(func() {
			found := fn(item)
			mu.Lock()
			out = append(out, found...)
			mu.Unlock()
		})
	}
	wg.Wait()
	return BidderDIDList(out)
}

func capped(dids []string) []string {
	if len(dids) > maxTrustRepos {
		return dids[:maxTrustRepos]
	}
	return dids
}

func (m *HTTPMarket) relaySources(ctx context.Context, roots []string) ([]candidateSource, int) {
	relays := append([]string(nil), m.RelayURLs...)
	auto := 0
	for _, did := range roots {
		for _, relay := range market.RelayURLsOf(ctx, m.Reader, did) {
			relay = strings.TrimRight(relay, "/")
			if !slices.Contains(relays, relay) {
				relays = append(relays, relay)
				auto++
			}
		}
	}
	out := make([]candidateSource, len(relays))
	var wg sync.WaitGroup
	for i, relay := range relays {
		wg.Go(func() {
			out[i] = candidateSource{name: sourceRelayPrefix + relay,
				dids: market.DiscoverBiddersFromRelay(ctx, relay, market.OfferingNSID, bidderDiscoveryTimeout, m.client(), market.NoopLog)}
		})
	}
	wg.Wait()
	return out, auto
}

func (m *HTTPMarket) candidateSources(ctx context.Context) ([]candidateSource, int) {
	self := m.selfDIDs()
	operators := BidderDIDList(append(append([]string{m.OperatorDID}, self...), eachParallel(self, func(did string) []string {
		return m.operatorsOf(ctx, did)
	})...))
	roots := BidderDIDList(append(append([]string(nil), self...), m.OperatorDID))
	var relays []candidateSource
	var auto int
	var vouched []string
	var wg sync.WaitGroup
	wg.Go(func() { relays, auto = m.relaySources(ctx, roots) })
	wg.Go(func() {
		vouched = capped(eachParallel(operators, func(did string) []string { return m.vouchedBy(ctx, did) }))
	})
	wg.Wait()
	operated := eachParallel(capped(BidderDIDList(append(append([]string(nil), operators...), vouched...))), func(did string) []string {
		return m.operatedBy(ctx, did)
	})
	out := []candidateSource{{name: sourceConfigured, dids: BidderDIDList(m.BidderDIDs), named: true}}
	out = append(out, relays...)
	out = append(out, candidateSource{name: sourceVouched, dids: vouched}, candidateSource{name: sourceOperated, dids: operated})
	return out, auto
}

func sourceCounts(sources []candidateSource, auto int) string {
	parts := make([]string, 0, len(sources)+1)
	for _, source := range sources {
		parts = append(parts, source.name+"="+strconv.Itoa(len(source.dids)))
	}
	parts = append(parts, "relaysFromRecords="+strconv.Itoa(auto))
	return strings.Join(parts, " ")
}
