package remotepds

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime/schema"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	pdsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/pds/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/atprotorepo"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/pds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/pds/remote"
)

const Kind = "RemotePDS"

var GroupKind = schema.GroupKind{Group: pdsv1alpha1.GroupName, Kind: Kind}

func (c *Controller) host(to commonv1.ObjectRef) (*remote.Host, error) {
	host, ok := c.hosts.Get(to.Workspace, to.Name)
	if !ok || host == nil {
		return nil, fmt.Errorf("%w: the RemotePDS %s is not ready", pds.ErrNotFound, to.String())
	}
	return host, nil
}

func (c *Controller) Provide() {
	bus.Provide(c.bus, GroupKind, pds.CapabilityAccount, func(_ context.Context, to commonv1.ObjectRef, _ []string) (pds.Transport, error) {
		return c.host(to)
	})
	bus.Provide(c.bus, GroupKind, pds.CapabilityAccount, func(_ context.Context, to commonv1.ObjectRef, scope []string) (pds.Accounts, error) {
		host, err := c.host(to)
		if err != nil {
			return nil, err
		}
		return host.Accounts(scope), nil
	})
	bus.Provide(c.bus, GroupKind, pds.CapabilitySession, func(_ context.Context, to commonv1.ObjectRef, scope []string) (pds.Sessions, error) {
		host, err := c.host(to)
		if err != nil {
			return nil, err
		}
		return host.Sessions(scope), nil
	})
	bus.Provide(c.bus, GroupKind, pds.CapabilitySync, func(_ context.Context, to commonv1.ObjectRef, _ []string) (pds.Sync, error) {
		host, err := c.host(to)
		if err != nil {
			return nil, err
		}
		return host.Sync(), nil
	})
	bus.Provide(c.bus, GroupKind, pds.CapabilityAccount, func(_ context.Context, to commonv1.ObjectRef, scope []string) (atprotorepo.Repo, error) {
		host, err := c.host(to)
		if err != nil {
			return nil, err
		}
		did, err := pds.AccountDID(scope)
		if err != nil {
			return nil, err
		}
		return host.Repo(did)
	})
}
