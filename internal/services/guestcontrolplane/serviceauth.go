package guestcontrolplane

import (
	"context"
	"time"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/pds"
)

type busServiceAuth struct {
	bus *bus.Bus

	self commonv1.ObjectRef

	labels map[string]string

	pds commonv1.ObjectRef
}

var _ pds.ServiceAuth = busServiceAuth{}

func (a busServiceAuth) ServiceAuth(ctx context.Context, did, audience, lxm string, expires time.Time) (string, error) {
	ctx = bus.WithCallerLabels(ctx, a.self, a.labels)
	minter, err := bus.Resolve[pds.ServiceAuth](ctx, a.bus, a.self, a.pds, bus.Capability{Name: pds.CapabilityServiceAuth, Scope: []string{did}})
	if err != nil {
		return "", err
	}
	return minter.ServiceAuth(ctx, did, audience, lxm, expires)
}
