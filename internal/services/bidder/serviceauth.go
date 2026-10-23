package bidder

import (
	"context"
	"errors"
	"time"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/pds"
)

var errNoSessionToken = errors.New("bidder: the app Pds mints service auth through the bus; no session token is held")

type busServiceAuth struct {
	bus *bus.Bus

	self commonv1.ObjectRef

	pds commonv1.ObjectRef

	did string
}

func (a busServiceAuth) AccessToken(context.Context) (string, error) {
	return "", errNoSessionToken
}

func (a busServiceAuth) ServiceAuth(ctx context.Context, audience, lxm string) (string, error) {
	minter, err := bus.Resolve[pds.ServiceAuth](ctx, a.bus, a.self, a.pds, bus.Capability{Name: pds.CapabilityServiceAuth, Scope: []string{a.did}})
	if err != nil {
		return "", err
	}
	return minter.ServiceAuth(ctx, a.did, audience, lxm, time.Now().Add(serviceAuthTokenLifetime))
}
