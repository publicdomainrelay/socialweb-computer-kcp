package participant

import (
	"context"
	"strings"
	"sync"
	"time"

	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	market "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/market"
)

type Inputs struct {
	Exposures *Index[exposurev1alpha1.Exposure]

	Identities *Index[v1alpha1.MarketIdentity]

	mu sync.Mutex

	next market.Observer
}

var _ market.Observer = (*Inputs)(nil)

func NewInputs() *Inputs {
	return &Inputs{Exposures: NewIndex[exposurev1alpha1.Exposure](), Identities: NewIndex[v1alpha1.MarketIdentity]()}
}

func (in *Inputs) Route(next market.Observer) {
	in.mu.Lock()
	in.next = next
	in.mu.Unlock()
}

func (in *Inputs) Observe(ctx context.Context, change market.Change) (time.Duration, error) {
	switch change.Kind {
	case market.KindExposure:
		if change.Gone || change.Exposure == nil {
			in.Exposures.Remove(change.Name)
		} else {
			in.Exposures.Put(change.Name, *change.Exposure)
		}
	case market.KindMarketIdentity:
		key := change.Workspace + "/" + change.Name
		if change.Gone || change.MarketIdentity == nil {
			in.Identities.Remove(key)
		} else {
			in.Identities.Put(key, *change.MarketIdentity)
		}
	}
	in.mu.Lock()
	next := in.next
	in.mu.Unlock()
	if next == nil {
		return 0, nil
	}
	return next.Observe(ctx, change)
}

func (in *Inputs) Address(name string) string {
	exposure, ok := in.Exposures.Get(strings.TrimSpace(name))
	if !ok {
		return ""
	}
	return strings.TrimRight(strings.TrimSpace(exposure.Status.Address), "/")
}

func (in *Inputs) AwaitAddress(ctx context.Context, name string, timeout time.Duration, usable func(string) bool) string {
	if usable == nil {
		usable = func(raw string) bool { return raw != "" }
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var found string
	_ = in.Exposures.Await(waitCtx, func() bool {
		found = in.Address(name)
		return usable(found)
	})
	if !usable(found) {
		return ""
	}
	return found
}

func (in *Inputs) Identity(workspace, name string) (v1alpha1.MarketIdentity, bool) {
	return in.Identities.Get(workspace + "/" + strings.TrimSpace(name))
}

func (in *Inputs) AwaitIdentity(ctx context.Context, workspaces []string, name string, timeout time.Duration) (v1alpha1.MarketIdentity, bool) {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var found v1alpha1.MarketIdentity
	ok := false
	_ = in.Identities.Await(waitCtx, func() bool {
		for _, ws := range workspaces {
			if id, has := in.Identity(ws, name); has {
				found, ok = id, true
				return true
			}
		}
		return false
	})
	return found, ok
}
