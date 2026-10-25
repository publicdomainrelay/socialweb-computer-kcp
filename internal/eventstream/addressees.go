package eventstream

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
)

const (
	foreignLogEvery = 1000

	temporaryNSIDPrefix = "com.publicdomainrelay.temp."
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

	unread atomic.Int64

	unaddressed atomic.Int64
}

func newAddressees(ctx context.Context, role, workspace string, dyn DynamicFactory, log *slog.Logger) (*addressees, error) {
	client, err := dyn(workspace)
	if err != nil {
		return nil, err
	}
	a := &addressees{role: role, workspace: workspace, log: log}
	a.dirty.Store(true)
	changed := func() { a.dirty.Store(true) }
	if a.identities, err = kube.NewCache[v1alpha1.MarketIdentity](client.Resource(v1alpha1.MarketIdentityGVR), metav1.ListOptions{}, changed); err != nil {
		return nil, err
	}
	if role == "requester" {
		if a.contracts, err = kube.NewCache[v1alpha1.ComputeContract](client.Resource(v1alpha1.ComputeContractGVR), metav1.ListOptions{}, changed); err != nil {
			return nil, err
		}
		go a.contracts.Run(ctx)
	}
	go a.identities.Run(ctx)
	return a, nil
}

func (a *addressees) current(ctx context.Context) (*market.Addressees, bool) {
	if !a.identities.WaitSynced(ctx) || (a.contracts != nil && !a.contracts.WaitSynced(ctx)) {
		return nil, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.index == nil || a.dirty.Swap(false) {
		var dids []string
		for _, id := range a.identities.List() {
			dids = append(dids, id.Status.Did, id.Spec.Did)
		}
		if a.contracts != nil {
			a.index = market.RequesterAddresseesOf(a.contracts.List(), true, dids...)
		} else {
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

func (a *addressees) addressed(ctx context.Context, collection, author string, record map[string]any) bool {
	index, ok := a.current(ctx)
	if !ok {
		return false
	}
	var addressed bool
	if a.role == "requester" {
		addressed = index.RequesterAddressed(collection, author, record)
	} else {
		addressed = index.BidderAddressed(collection, record)
	}
	if !addressed {
		a.drop(&a.unaddressed, collection)
	}
	return addressed
}

func (a *addressees) drop(counter *atomic.Int64, collection string) {
	n := counter.Add(1)
	if n == 1 || n%foreignLogEvery == 0 {
		a.log.Info("market_foreign_dropped", "role", a.role, "workspace", a.workspace, "unread", a.unread.Load(),
			"unaddressed", a.unaddressed.Load(), "collection", collection)
	}
}
