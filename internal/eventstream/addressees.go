package eventstream

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
)

const (
	foreignLogEvery = 1000

	closedLogEvery = 100

	temporaryNSIDPrefix = "com.publicdomainrelay.temp."

	chasedReceiptsCap = 1024
)

type addressees struct {
	role string

	workspace string

	log *slog.Logger

	contracts *kube.Cache[v1alpha1.ComputeContract]

	identities *kube.Cache[v1alpha1.MarketIdentity]

	dirty atomic.Bool

	mu sync.Mutex

	index *market.Addressees

	chased map[string]market.ContractRef

	unread atomic.Int64

	waits atomic.Int64

	unaddressed atomic.Int64

	closedRFPs atomic.Int64
}

func newAddressees(ctx context.Context, role, workspace string, dyn DynamicFactory, log *slog.Logger) (*addressees, error) {
	client, err := dyn(workspace)
	if err != nil {
		return nil, err
	}
	a := &addressees{role: role, workspace: workspace, log: log, chased: map[string]market.ContractRef{}}
	a.dirty.Store(true)
	changed := func() { a.dirty.Store(true) }
	if role == "requester" {
		if a.contracts, err = kube.NewCache[v1alpha1.ComputeContract](client.Resource(v1alpha1.ComputeContractGVR), metav1.ListOptions{}, changed); err != nil {
			return nil, err
		}
		go a.contracts.Run(ctx)
		return a, nil
	}
	if a.identities, err = kube.NewCache[v1alpha1.MarketIdentity](client.Resource(v1alpha1.MarketIdentityGVR), metav1.ListOptions{}, changed); err != nil {
		return nil, err
	}
	go a.identities.Run(ctx)
	return a, nil
}

func (a *addressees) current(ctx context.Context) (*market.Addressees, bool) {
	if a.contracts != nil && !a.contracts.WaitSynced(ctx) {
		return nil, false
	}
	if a.identities != nil && !a.identities.WaitSynced(ctx) {
		return nil, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.index == nil {
		a.log.Info("market_addressees_synced", "role", a.role, "workspace", a.workspace)
	}
	if a.index == nil || a.dirty.Swap(false) {
		if a.contracts != nil {
			a.index = market.RequesterAddresseesOf(a.contracts.List(), market.RequesterContractSettled)
		} else {
			var dids []string
			for _, id := range a.identities.List() {
				dids = append(dids, id.Status.Did, id.Spec.Did)
			}
			a.index = market.BidderAddresseesOf(dids...)
		}
	}
	return a.index, true
}

func (a *addressees) consumes(collection string) bool {
	if a.role == "requester" {
		return market.RequesterConsumes(collection)
	}
	return market.BidderConsumes(collection)
}

func (a *addressees) skipped(collection string) {
	if strings.HasPrefix(collection, temporaryNSIDPrefix) {
		a.drop(&a.unread, collection)
	}
}

func (a *addressees) wants(ctx context.Context, collection, author string) bool {
	if a.waits.Add(1) == 1 {
		a.log.Info("market_addressees_first_record", "role", a.role, "workspace", a.workspace, "collection", collection)
	}
	if !a.consumes(collection) {
		a.drop(&a.unread, collection)
		return false
	}
	if a.role != "requester" {
		return true
	}
	index, ok := a.current(ctx)
	if !ok {
		return false
	}
	if !index.RequesterWants(collection, author) {
		a.drop(&a.unread, collection)
		return false
	}
	return true
}

func (a *addressees) closed(source, collection, uri string, record map[string]any) bool {
	if a.role == "requester" || collection != market.RFPNSID || !market.RFPBidWindowOver(uri, record, time.Now()) {
		return false
	}
	if n := a.closedRFPs.Add(1); n == 1 || n%closedLogEvery == 0 {
		a.log.Info("market_rfp_closed_dropped", "role", a.role, "workspace", a.workspace, "dropped", n, "source", source, "uri", uri,
			"bidWindow", market.RFPBidWindow)
	}
	return true
}

type receiptFetch func(ctx context.Context, receiptURI string) (map[string]any, error)

func (a *addressees) owner(ctx context.Context, collection, author string, record map[string]any, fetch receiptFetch) (*market.ContractRef, bool) {
	index, ok := a.current(ctx)
	if !ok {
		return nil, false
	}
	if a.role != "requester" {
		if index.BidderAddressed(collection, record) {
			return nil, true
		}
		a.drop(&a.unaddressed, collection)
		return nil, false
	}
	ref, chase, matched := index.RequesterMatch(collection, author, record)
	if !matched && chase != "" {
		ref, matched = a.chase(ctx, index, chase, fetch)
	}
	if !matched {
		a.drop(&a.unaddressed, collection)
		return nil, false
	}
	return &ref, true
}

func (a *addressees) chase(ctx context.Context, index *market.Addressees, receiptURI string, fetch receiptFetch) (market.ContractRef, bool) {
	a.mu.Lock()
	known, seen := a.chased[receiptURI]
	a.mu.Unlock()
	if seen {
		return known, true
	}
	record, err := fetch(ctx, receiptURI)
	if err != nil {
		return market.ContractRef{}, false
	}
	ref, ok := index.ReceiptMatch(record)
	if ok {
		a.mu.Lock()
		if len(a.chased) >= chasedReceiptsCap {
			clear(a.chased)
		}
		a.chased[receiptURI] = ref
		a.mu.Unlock()
	}
	return ref, ok
}

func (a *addressees) drop(counter *atomic.Int64, collection string) {
	n := counter.Add(1)
	if n == 1 || n%foreignLogEvery == 0 {
		a.log.Info("market_foreign_dropped", "role", a.role, "workspace", a.workspace, "unread", a.unread.Load(),
			"unaddressed", a.unaddressed.Load(), "collection", collection)
	}
}
