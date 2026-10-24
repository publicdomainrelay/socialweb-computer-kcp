package marketidentity

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/pds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/pds/identity"
)

var GroupKind = schema.GroupKind{Group: v1alpha1.GroupName, Kind: "MarketIdentity"}

func (c *Controller) Provide(b *bus.Bus) {
	bus.Provide(b, GroupKind, pds.CapabilitySession, func(ctx context.Context, to commonv1.ObjectRef, scope []string) (pds.Authenticator, error) {
		return c.authenticator(ctx, to, scope)
	})
	bus.Provide(b, GroupKind, pds.CapabilityServiceAuth, func(ctx context.Context, to commonv1.ObjectRef, scope []string) (pds.ServiceAuth, error) {
		return c.authenticator(ctx, to, scope)
	})
}

func (c *Controller) authenticator(ctx context.Context, to commonv1.ObjectRef, scope []string) (*identity.Authenticator, error) {
	did, err := pds.AccountDID(scope)
	if err != nil {
		return nil, err
	}
	entry, ok := c.identities.Get(to.Workspace, to.Name)
	if !ok || entry.identity == nil {
		return nil, fmt.Errorf("%w: the MarketIdentity %s is not reconciled by this core", pds.ErrNotFound, to.String())
	}
	if held := strings.TrimSpace(entry.identity.Status.Did); held != did {
		return nil, fmt.Errorf("pds: the MarketIdentity %s signs for %q, not %s", to.String(), held, did)
	}
	auth, err := c.accounts.Authenticator(ctx, to.Workspace, entry.identity)
	if err != nil {
		return nil, fmt.Errorf("%w: the session of the MarketIdentity %s: %v", pds.ErrNotFound, to.String(), err)
	}
	return auth, nil
}
