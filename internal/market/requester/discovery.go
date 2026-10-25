package requester

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
)

const (
	sourceConfigured = "configured"

	sourceRelayPrefix = "relay:"

	sourceVouched = "vouched"

	sourceOperated = "operated"

	bidderAssociateService = "bidder_associate"

	requesterAssociateService = "requester_associate"

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

func associationRecord(value map[string]any) (service, challenge, keyID string) {
	service, _ = value["service"].(string)
	return strings.TrimSpace(service), didValue(value["challenge"]), didValue(value["keyId"])
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

func (m *HTTPMarket) operatorsOf(ctx context.Context, self string) []string {
	var out []string
	for _, record := range m.listRepo(ctx, self, market.BadgeBlueKeysNSID) {
		service, challenge, keyID := associationRecord(record.Value)
		if service != bidderAssociateService && service != requesterAssociateService {
			continue
		}
		switch {
		case keyID == self && challenge != "" && challenge != self:
			out = append(out, challenge)
		case challenge == self && keyID != "" && keyID != self:
			out = append(out, keyID)
		}
	}
	return out
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

func associates(value map[string]any, operator string) string {
	service, challenge, keyID := associationRecord(value)
	if service != bidderAssociateService || challenge != operator || keyID == operator {
		return ""
	}
	return keyID
}

func (m *HTTPMarket) operatedBy(ctx context.Context, operator string) []string {
	var candidates []string
	for _, record := range m.listRepo(ctx, operator, market.BadgeBlueKeysNSID) {
		if did := associates(record.Value, operator); did != "" {
			candidates = append(candidates, did)
		}
	}
	var out []string
	for _, did := range BidderDIDList(candidates) {
		mirrored := slices.ContainsFunc(m.listRepo(ctx, did, market.BadgeBlueKeysNSID), func(record market.ListedRecord) bool {
			return associates(record.Value, operator) == did
		})
		if !mirrored {
			m.log().Info("requester_operated_unmirrored", "operator", operator, "bidder", did)
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
		for _, relay := range market.AutoDiscoverRelayURLs(ctx, m.Reader, did) {
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
