package requester

import (
	"context"
	"strconv"
	"strings"
	"sync"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
)

const (
	sourceConfigured = "configured"

	sourceRelayPrefix = "relay:"
)

type candidateSource struct {
	name string

	dids []string

	named bool
}

func (m *HTTPMarket) relaySources(ctx context.Context) []candidateSource {
	relays := append([]string(nil), m.RelayURLs...)
	out := make([]candidateSource, len(relays))
	var wg sync.WaitGroup
	for i, relay := range relays {
		wg.Go(func() {
			out[i] = candidateSource{name: sourceRelayPrefix + relay,
				dids: market.DiscoverBiddersFromRelay(ctx, relay, market.OfferingNSID, bidderDiscoveryTimeout, m.client(), market.NoopLog)}
		})
	}
	wg.Wait()
	return out
}

func (m *HTTPMarket) candidateSources(ctx context.Context) ([]candidateSource, int) {
	out := []candidateSource{{name: sourceConfigured, dids: BidderDIDList(m.BidderDIDs), named: true}}
	return append(out, m.relaySources(ctx)...), 0
}

func sourceCounts(sources []candidateSource, auto int) string {
	parts := make([]string, 0, len(sources)+1)
	for _, source := range sources {
		parts = append(parts, source.name+"="+strconv.Itoa(len(source.dids)))
	}
	parts = append(parts, "relaysFromRecords="+strconv.Itoa(auto))
	return strings.Join(parts, " ")
}
