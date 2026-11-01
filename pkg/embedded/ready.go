package embedded

import (
	"context"
	"fmt"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/marketidentity"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
)

func (r *Requester) registerReady() error {
	options := []ctlruntime.Option{
		ctlruntime.Export(marketidentity.Export),
		ctlruntime.InWorkspaces(func(ws string) bool { return ws == RootWorkspace }),
		ctlruntime.Only(func(obj *v1alpha1.MarketIdentity) bool { return obj.Name == IdentityName }),
	}
	if err := ctlruntime.For[*v1alpha1.MarketIdentity](r.manager, "embedded-ready", ctlruntime.ReconcilerFunc[*v1alpha1.MarketIdentity](r.identitySeen), options...); err != nil {
		return fmt.Errorf("embedded: the readiness watch: %w", err)
	}
	return nil
}

func (r *Requester) identitySeen(_ context.Context, _ ctlruntime.Request, obj *v1alpha1.MarketIdentity) (ctlruntime.Result, error) {
	if obj.DeletionTimestamp.IsZero() {
		r.readyOnce.Do(func() { close(r.ready) })
	}
	return ctlruntime.Result{}, nil
}

func (r *Requester) Ready() <-chan struct{} { return r.ready }

func (r *Requester) WaitReady(ctx context.Context) error {
	select {
	case <-r.ready:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("embedded: the MarketRequester %s has not composed its MarketIdentity %s yet: %w", AppName, IdentityName, ctx.Err())
	}
}
